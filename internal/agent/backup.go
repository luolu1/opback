package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"opback/internal/openlist"
	"opback/internal/types"
)

// backupStats accumulates the outcome of one backup run.
type backupStats struct {
	FilesTotal   int
	FilesDone    int
	FilesSkipped int
	FilesFailed  int
	BytesTotal   int64
	BytesDone    int64
}

// sourceFile is one file selected for upload.
type sourceFile struct {
	rel   string // forward-slash path relative to the source root
	abs   string // absolute path on disk
	size  int64
	mtime int64 // Unix seconds
}

const maxErrorMessages = 50

// runBackup copies every selected file from the job's source into OpenList.
// It reports progress through the progress callback and returns the final
// statistics together with any per-file error messages.
func runBackup(
	ctx context.Context,
	ol *openlist.Client,
	job types.Job,
	mf *manifest,
	progress func(backupStats),
	logf func(string, ...any),
) (backupStats, []string) {
	var stats backupStats
	var errMu sync.Mutex
	var errs []string
	addErr := func(format string, args ...any) {
		errMu.Lock()
		defer errMu.Unlock()
		if len(errs) < maxErrorMessages {
			errs = append(errs, fmt.Sprintf(format, args...))
		}
	}

	root := filepath.Clean(job.Source)
	info, err := os.Stat(root)
	if err != nil {
		addErr("无法访问源路径 %s: %v", job.Source, err)
		progress(stats)
		return stats, errs
	}

	// Collect the file list.
	var files []sourceFile
	if !info.IsDir() {
		// A single file is backed up under its own name.
		files = append(files, sourceFile{
			rel:   filepath.Base(root),
			abs:   root,
			size:  info.Size(),
			mtime: info.ModTime().Unix(),
		})
	} else {
		files = collectFiles(root, job.Options, logf)
	}

	for _, f := range files {
		stats.FilesTotal++
		stats.BytesTotal += f.size
	}
	progress(stats)

	if len(files) == 0 {
		logf("源目录中没有需要备份的文件")
	} else {
		logf("发现 %d 个文件，共 %d 字节", stats.FilesTotal, stats.BytesTotal)

		// Make sure the destination root exists.
		if err := ol.EnsureDir(ctx, job.Dest); err != nil {
			addErr("创建目标目录 %s 失败: %v", job.Dest, err)
			return stats, errs
		}

		// Create every needed sub-directory in depth order.
		for _, d := range neededDirs(files) {
			if ctx.Err() != nil {
				break
			}
			if err := ol.Mkdir(ctx, joinOpenList(job.Dest, d)); err != nil {
				addErr("创建目录 %s 失败: %v", joinOpenList(job.Dest, d), err)
			}
		}

		// Upload files with a bounded worker pool.
		concurrency := job.Options.Concurrency
		if concurrency <= 0 {
			concurrency = 2
		}
		if concurrency > len(files) {
			concurrency = len(files)
		}

		var mu sync.Mutex // guards stats and errs updates
		queue := make(chan sourceFile)
		var wg sync.WaitGroup

		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for f := range queue {
					if ctx.Err() != nil {
						return
					}
					uploadOneFile(ctx, ol, job, mf, f, &mu, &stats, addErr, logf)
					progress(stats)
				}
			}()
		}

		for _, f := range files {
			if ctx.Err() != nil {
				break
			}
			queue <- f
		}
		close(queue)
		wg.Wait()
	}

	// Reconcile the destination with the source selection: remove files that
	// no longer exist at the source (subject to the retention window). This
	// also runs when the source is empty so that a fully deleted directory
	// still gets cleaned up.
	if job.Options.DeleteExtra && ctx.Err() == nil {
		expected := make(map[string]bool, len(files))
		for _, f := range files {
			expected[f.rel] = true
		}
		cleanupDestination(ctx, ol, job, expected, time.Now().UTC(), addErr, logf)
	}

	if err := mf.save(); err != nil {
		addErr("保存增量索引失败: %v", err)
	}
	return stats, errs
}

// uploadOneFile uploads a single file and updates the shared statistics.
func uploadOneFile(
	ctx context.Context,
	ol *openlist.Client,
	job types.Job,
	mf *manifest,
	f sourceFile,
	mu *sync.Mutex,
	stats *backupStats,
	addErr func(string, ...any),
	logf func(string, ...any),
) {
	// Incremental: skip unchanged files already present from a prior run.
	if job.Options.Incremental && mf.matches(f.rel, f.size, f.mtime) {
		mu.Lock()
		stats.FilesSkipped++
		mu.Unlock()
		return
	}

	fh, err := os.Open(f.abs)
	if err != nil {
		mu.Lock()
		// The file was listed earlier but vanished before we got to it. This is
		// normal for live directories (e.g. the panel's atomic-write temp files,
		// which are renamed into place while the backup is running), so treat it
		// as a skip instead of a failure.
		if errors.Is(err, fs.ErrNotExist) {
			stats.FilesSkipped++
			mu.Unlock()
			logf("跳过已消失的文件 %s", f.rel)
			return
		}
		stats.FilesFailed++
		mu.Unlock()
		addErr("打开 %s 失败: %v", f.rel, err)
		return
	}
	defer fh.Close()

	fi, err := fh.Stat()
	if err != nil {
		mu.Lock()
		stats.FilesFailed++
		mu.Unlock()
		addErr("读取 %s 元数据失败: %v", f.rel, err)
		return
	}

	dest := joinOpenList(job.Dest, f.rel)
	err = ol.Put(ctx, dest, fh, fi.Size(), openlist.PutOptions{
		Overwrite:    job.Options.Overwrite,
		LastModified: fi.ModTime(),
	})

	mu.Lock()
	defer mu.Unlock()
	if err != nil {
		stats.FilesFailed++
		addErr("上传 %s 失败: %v", f.rel, err)
		return
	}
	stats.FilesDone++
	stats.BytesDone += fi.Size()
	mf.set(f.rel, fi.Size(), fi.ModTime().Unix())
}

// collectFiles walks the source root, honouring the include/exclude options.
// It follows symlinks when requested and guards against cycles.
func collectFiles(root string, opt types.JobOptions, logf func(string, ...any)) []sourceFile {
	var files []sourceFile
	visited := map[string]bool{}

	var walk func(dir, relDir string)
	walk = func(dir, relDir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			logf("无法读取目录 %s: %v", dir, err)
			return
		}
		for _, e := range entries {
			name := e.Name()
			abs := filepath.Join(dir, name)
			rel := name
			if relDir != "" {
				rel = relDir + "/" + name
			}

			if e.Type()&fs.ModeSymlink != 0 && !opt.FollowSymlinks {
				logf("跳过符号链接 %s", rel)
				continue
			}

			// os.Stat follows symlinks so linked directories are traversed.
			fi, err := os.Stat(abs)
			if err != nil {
				logf("无法访问 %s: %v", rel, err)
				continue
			}

			if fi.IsDir() {
				if matchAny(opt.Exclude, rel, true) {
					continue
				}
				if real, err := filepath.EvalSymlinks(abs); err == nil {
					if visited[real] {
						continue
					}
					visited[real] = true
				}
				walk(abs, rel)
				continue
			}

			if matchAny(opt.Exclude, rel, false) {
				continue
			}
			if len(opt.Include) > 0 && !matchAny(opt.Include, rel, false) {
				continue
			}

			files = append(files, sourceFile{
				rel:   rel,
				abs:   abs,
				size:  fi.Size(),
				mtime: fi.ModTime().Unix(),
			})
		}
	}
	walk(root, "")
	return files
}

// neededDirs returns every directory that must exist at the destination,
// sorted shallowest first so parents are created before their children.
func neededDirs(files []sourceFile) []string {
	set := map[string]bool{}
	for _, f := range files {
		d := path.Dir(f.rel)
		for d != "." && d != "/" && d != "" {
			set[d] = true
			d = path.Dir(d)
		}
	}
	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		di, dj := strings.Count(out[i], "/"), strings.Count(out[j], "/")
		if di != dj {
			return di < dj
		}
		return out[i] < out[j]
	})
	return out
}

// matchAny reports whether rel matches any of the glob patterns. Patterns are
// matched against the full relative path, its base name, and the base name of
// each parent directory (so "node_modules" excludes it at any depth).
func matchAny(patterns []string, rel string, isDir bool) bool {
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if ok, _ := path.Match(p, rel); ok {
			return true
		}
		if ok, _ := path.Match(p, path.Base(rel)); ok {
			return true
		}
		for _, seg := range strings.Split(rel, "/") {
			if ok, _ := path.Match(p, seg); ok {
				return true
			}
		}
	}
	return false
}

// joinOpenList joins an OpenList base path with a relative path.
func joinOpenList(dest, rel string) string {
	dest = "/" + strings.Trim(dest, "/")
	rel = strings.Trim(rel, "/")
	if rel == "" || rel == "." {
		return dest
	}
	return dest + "/" + rel
}
