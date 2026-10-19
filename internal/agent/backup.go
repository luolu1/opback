package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	// unstable marks a staged copy whose source kept changing while it was
	// copied. The copy is still uploaded, but it is kept out of the
	// incremental manifest so the next run tries again.
	unstable bool
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

	selected := files
	for _, f := range selected {
		stats.FilesTotal++
		stats.BytesTotal += f.size
	}
	progress(stats)

	// Optional staging: copy the selection to a temporary directory first, then
	// upload from that stable copy. This keeps a long upload from reading a
	// file that is still being written to. It is opt-in per job.
	uploadFiles := selected
	stagingRan := false
	if job.Options.Staging && len(selected) > 0 {
		toStage := selected
		if job.Options.Incremental {
			// Do not copy files that incremental mode will skip anyway.
			toStage = make([]sourceFile, 0, len(selected))
			for _, f := range selected {
				if mf.matches(f.rel, f.size, f.mtime) {
					stats.FilesSkipped++
					continue
				}
				toStage = append(toStage, f)
			}
			if len(toStage) == 0 {
				uploadFiles = nil
			}
		}
		if len(toStage) > 0 {
			stagingRan = true
			var stagedRoot string
			uploadFiles, stagedRoot = stageFiles(ctx, job, root, info.IsDir(), toStage, &stats, addErr, logf)
			if stagedRoot != "" {
				defer os.RemoveAll(stagedRoot)
			}
		}
		progress(stats)
	}

	if len(selected) == 0 {
		logf("源目录中没有需要备份的文件")
	} else {
		logf("发现 %d 个文件，共 %d 字节", stats.FilesTotal, stats.BytesTotal)
		if len(uploadFiles) == 0 {
			if stagingRan {
				logf("复制阶段没有可上传的文件")
			}
		} else {
			// Make sure the destination root exists.
			if err := ol.EnsureDir(ctx, job.Dest); err != nil {
				addErr("创建目标目录 %s 失败: %v", job.Dest, err)
				return stats, errs
			}

			// Create every needed sub-directory in depth order.
			for _, d := range neededDirs(uploadFiles) {
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
			if concurrency > len(uploadFiles) {
				concurrency = len(uploadFiles)
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

			for _, f := range uploadFiles {
				if ctx.Err() != nil {
					break
				}
				queue <- f
			}
			close(queue)
			wg.Wait()
		}
	}

	if ctx.Err() != nil {
		addErr("任务已取消：%v", ctx.Err())
	}

	// Reconcile the destination with the source selection: remove files that
	// no longer exist at the source (subject to the retention window). This
	// also runs when the source is empty so that a fully deleted directory
	// still gets cleaned up. Use the original selection so a staging failure
	// never causes an existing destination file to be deleted.
	if job.Options.DeleteExtra && ctx.Err() == nil {
		expected := make(map[string]bool, len(selected))
		for _, f := range selected {
			expected[f.rel] = true
		}
		cleanupDestination(ctx, ol, job, expected, time.Now().UTC(), addErr, logf)
	}

	if err := mf.save(); err != nil {
		addErr("保存增量索引失败: %v", err)
	}
	return stats, errs
}

// stageFiles copies the selected files into a fresh temporary directory and
// returns the staged list. The caller uploads from that copy and removes the
// directory afterwards. Copying is sequential so temporary disk usage and I/O
// stay predictable, regardless of the upload concurrency.
func stageFiles(
	ctx context.Context,
	job types.Job,
	sourceRoot string,
	sourceIsDir bool,
	files []sourceFile,
	stats *backupStats,
	addErr func(string, ...any),
	logf func(string, ...any),
) ([]sourceFile, string) {
	dir := strings.TrimSpace(job.Options.StagingDir)
	if dir == "" {
		dir = os.TempDir()
	}

	// Refuse a staging directory inside the source: the copy would live in the
	// tree being backed up and leftovers from an interrupted run could be
	// picked up by a later scan.
	if sourceIsDir {
		absSource, err1 := filepath.Abs(sourceRoot)
		absDir, err2 := filepath.Abs(dir)
		if err1 == nil && err2 == nil {
			if rel, err := filepath.Rel(absSource, absDir); err == nil {
				if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
					addErr("临时目录 %s 不能位于源目录 %s 内", absDir, absSource)
					stats.FilesFailed += len(files)
					return nil, ""
				}
			}
		}
	}

	root, err := os.MkdirTemp(dir, "opback-stage-*")
	if err != nil {
		addErr("创建临时目录失败（%s）: %v", dir, err)
		stats.FilesFailed += len(files)
		return nil, ""
	}
	logf("正在复制 %d 个文件到临时目录 %s", len(files), root)

	staged := make([]sourceFile, 0, len(files))
	for _, f := range files {
		if ctx.Err() != nil {
			break
		}
		dst := filepath.Join(root, filepath.FromSlash(f.rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			stats.FilesFailed++
			addErr("创建临时子目录失败（%s）: %v", f.rel, err)
			continue
		}
		info, changed, err := copyToStaging(ctx, f.abs, dst, job.Options.VerifyConsistency)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				stats.FilesSkipped++
				logf("跳过已消失的文件 %s", f.rel)
				continue
			}
			stats.FilesFailed++
			addErr("复制 %s 到临时目录失败: %v", f.rel, err)
			continue
		}
		if changed {
			addErr("文件 %s 在复制期间仍被修改，临时副本可能不是一致快照", f.rel)
		}
		staged = append(staged, sourceFile{
			rel:      f.rel,
			abs:      dst,
			size:     info.Size(),
			mtime:    info.ModTime().Unix(),
			unstable: changed,
		})
	}
	return staged, root
}

// copyToStaging copies exactly the bytes that existed when the source was
// opened and preserves the modification time, so incremental backups keep
// working. changed reports that the source was modified while copying.
func copyToStaging(ctx context.Context, src, dst string, verify bool) (os.FileInfo, bool, error) {
	const attempts = 2 // one retry for truncation races and verification
	var last os.FileInfo
	for attempt := 1; attempt <= attempts; attempt++ {
		info, changed, err := copyOnce(ctx, src, dst)
		if err != nil {
			if isTruncated(err) && attempt < attempts {
				continue
			}
			_ = os.Remove(dst)
			return nil, false, err
		}
		last = info
		if !verify || !changed {
			return info, false, nil
		}
	}
	return last, true, nil
}

// copyOnce performs a single bounded copy. It never reads past the size seen
// when the source was opened, so a growing file cannot make the copy unbounded.
func copyOnce(ctx context.Context, src, dst string) (os.FileInfo, bool, error) {
	in, err := os.Open(src)
	if err != nil {
		return nil, false, err
	}
	defer in.Close()

	before, err := in.Stat()
	if err != nil {
		return nil, false, err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, before.Mode().Perm())
	if err != nil {
		return nil, false, err
	}
	_, copyErr := io.CopyN(out, contextReader{ctx: ctx, r: in}, before.Size())
	if copyErr == nil {
		_ = out.Chmod(before.Mode().Perm())
	}
	closeErr := out.Close()
	if copyErr != nil {
		return before, true, copyErr
	}
	if closeErr != nil {
		return before, false, closeErr
	}
	_ = os.Chtimes(dst, before.ModTime(), before.ModTime())

	after, err := os.Stat(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Removed while copying: the copy itself is complete. Keep it and
			// flag the race.
			return before, true, nil
		}
		return before, false, err
	}
	changed := after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime())
	return before, changed, nil
}

func isTruncated(err error) bool {
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

// contextReader aborts a long file copy when the run is cancelled.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
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

	attempts := 1
	if job.Options.VerifyConsistency {
		attempts = 2
	}
	dest := joinOpenList(job.Dest, f.rel)

	var (
		uploaded   int64
		finalInfo  os.FileInfo
		consistent bool
	)
	for attempt := 1; attempt <= attempts; attempt++ {
		fh, err := os.Open(f.abs)
		if err != nil {
			// The file was listed earlier but vanished before we got to it. This
			// is normal for live directories (e.g. the panel's atomic-write temp
			// files, which are renamed into place while the backup is running),
			// so treat it as a skip instead of a failure.
			if errors.Is(err, fs.ErrNotExist) {
				mu.Lock()
				stats.FilesSkipped++
				mu.Unlock()
				logf("跳过已消失的文件 %s", f.rel)
				return
			}
			mu.Lock()
			stats.FilesFailed++
			mu.Unlock()
			addErr("打开 %s 失败: %v", f.rel, err)
			return
		}

		fi, statErr := fh.Stat()
		if statErr != nil {
			fh.Close()
			mu.Lock()
			stats.FilesFailed++
			mu.Unlock()
			addErr("读取 %s 元数据失败: %v", f.rel, statErr)
			return
		}

		err = ol.Put(ctx, dest, fh, fi.Size(), openlist.PutOptions{
			Overwrite:    job.Options.Overwrite,
			LastModified: fi.ModTime(),
		})
		fh.Close()
		if err != nil {
			mu.Lock()
			stats.FilesFailed++
			mu.Unlock()
			addErr("上传 %s 失败: %v", f.rel, err)
			return
		}

		uploaded = fi.Size()
		finalInfo = fi
		if !job.Options.VerifyConsistency {
			consistent = true
			break
		}

		// Re-stat after the upload. If size or mtime moved, the file was still
		// being written to while it was read, so the stored copy is not a
		// consistent snapshot of the live file. Retry once with newer contents.
		after, serr := os.Stat(f.abs)
		if serr == nil && after.Size() == fi.Size() && after.ModTime().Equal(fi.ModTime()) {
			consistent = true
			break
		}
		if attempt < attempts {
			logf("文件 %s 在上传期间发生变化，重试上传（%d/%d）", f.rel, attempt, attempts)
		}
	}

	mu.Lock()
	stats.FilesDone++
	stats.BytesDone += uploaded
	if consistent && finalInfo != nil && !f.unstable {
		mf.set(f.rel, finalInfo.Size(), finalInfo.ModTime().Unix())
	}
	mu.Unlock()

	if !consistent {
		addErr("文件 %s 在上传期间持续被修改，已保存当时内容但可能不是一致快照；可启用「先复制到临时目录再上传」，或先用数据库工具生成一致性转储再备份该转储", f.rel)
	}
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
