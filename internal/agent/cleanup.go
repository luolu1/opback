package agent

import (
	"context"
	"path"
	"sort"
	"time"

	"opback/internal/openlist"
	"opback/internal/types"
)

// trashDirName is the managed trash folder created inside a job's destination
// root. It holds files that were removed from the destination by DeleteExtra,
// grouped into one timestamped snapshot folder per run.
const trashDirName = ".olb-trash"

// trashTimeFmt names each trash snapshot after the run's start time. It is
// parseable with time.Parse and sorts lexicographically, so it doubles as the
// ordering used when purging expired snapshots.
const trashTimeFmt = "20060102T150405Z"

// destEntry is one entry found under the destination root, addressed by its
// path relative to that root.
type destEntry struct {
	rel   string
	isDir bool
}

// cleanupDestination reconciles the job's destination with the set of files
// expected from the source. Files that exist at the destination but are not in
// expected are "extra": they are moved into a timestamped trash folder (when a
// retention window is configured) or deleted immediately. Trash snapshots
// older than the retention window are then purged.
//
// Anything under the trash folder is always ignored, so the cleanup never
// fights with the retained copies.
func cleanupDestination(
	ctx context.Context,
	ol *openlist.Client,
	job types.Job,
	expected map[string]bool,
	now time.Time,
	addErr func(string, ...any),
	logf func(string, ...any),
) {
	dest := job.Dest
	if err := ol.EnsureDir(ctx, dest); err != nil {
		addErr("创建目标目录 %s 失败: %v", dest, err)
		return
	}

	entries, err := listDest(ctx, ol, dest)
	if err != nil {
		addErr("列举目标目录 %s 失败: %v", dest, err)
		return
	}

	// Group extra files by their parent directory so each directory can be
	// moved with a single API call.
	byDir := map[string][]string{}
	var extras int
	for _, e := range entries {
		if e.isDir || expected[e.rel] {
			continue
		}
		extras++
		dir := path.Dir(e.rel)
		if dir == "." {
			dir = ""
		}
		byDir[dir] = append(byDir[dir], path.Base(e.rel))
	}

	trashRoot := joinOpenList(dest, trashDirName)

	if extras > 0 {
		if job.Options.RetentionDays > 0 {
			moveToTrash(ctx, ol, dest, trashRoot, now, byDir, addErr, logf)
		} else {
			for _, dir := range sortedKeys(byDir) {
				srcDir := joinOpenList(dest, dir)
				if err := ol.Remove(ctx, srcDir, byDir[dir]); err != nil {
					addErr("删除目标端多余文件失败 (%s): %v", srcDir, err)
				}
			}
			logf("清理目标端多余文件：永久删除 %d 个文件", extras)
		}

		// Directories that just became empty are no longer needed.
		if err := ol.RemoveEmptyDirs(ctx, dest); err != nil {
			logf("清理空目录失败（忽略）：%v", err)
		}
	}

	// Purge expired snapshots. This also runs when nothing was moved so that
	// previously retained files eventually expire even on idle runs.
	if job.Options.RetentionDays > 0 && hasTrash(ctx, ol, trashRoot) {
		pruneTrash(ctx, ol, trashRoot, now, job.Options.RetentionDays, addErr, logf)
	}
}

// moveToTrash moves each group of extra files into the current snapshot folder.
func moveToTrash(
	ctx context.Context,
	ol *openlist.Client,
	dest, trashRoot string,
	now time.Time,
	byDir map[string][]string,
	addErr func(string, ...any),
	logf func(string, ...any),
) {
	snapRoot := joinOpenList(trashRoot, now.UTC().Format(trashTimeFmt))
	moved := 0
	for _, dir := range sortedKeys(byDir) {
		names := byDir[dir]
		sort.Strings(names)
		dstDir := joinOpenList(snapRoot, dir)
		if err := ol.EnsureDir(ctx, dstDir); err != nil {
			addErr("创建回收站目录 %s 失败: %v", dstDir, err)
			continue
		}
		srcDir := joinOpenList(dest, dir)
		if err := ol.Move(ctx, srcDir, dstDir, names); err != nil {
			addErr("移动到回收站失败 (%s): %v", srcDir, err)
			continue
		}
		moved += len(names)
	}
	if moved > 0 {
		logf("清理目标端多余文件：移动 %d 个文件到回收站 %s", moved, snapRoot)
	}
}

// pruneTrash deletes snapshot folders older than retentionDays. Folder names
// that do not parse as a snapshot timestamp are left alone.
func pruneTrash(
	ctx context.Context,
	ol *openlist.Client,
	trashRoot string,
	now time.Time,
	retentionDays int,
	addErr func(string, ...any),
	logf func(string, ...any),
) {
	res, err := ol.List(ctx, trashRoot, false)
	if err != nil {
		addErr("读取回收站 %s 失败: %v", trashRoot, err)
		return
	}
	cutoff := now.UTC().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	var purged int
	for _, o := range res.Content {
		if !o.IsDir {
			continue
		}
		ts, err := time.Parse(trashTimeFmt, o.Name)
		if err != nil || ts.After(cutoff) {
			continue
		}
		if err := ol.Remove(ctx, trashRoot, []string{o.Name}); err != nil {
			addErr("清理过期回收站 %s/%s 失败: %v", trashRoot, o.Name, err)
			continue
		}
		purged++
	}
	if purged > 0 {
		logf("清理回收站：删除 %d 个过期快照", purged)
	}
}

// listDest walks root recursively and returns every file and directory below
// it, addressed relative to root. The managed trash folder is skipped so its
// contents are never mistaken for extras.
func listDest(ctx context.Context, ol *openlist.Client, root string) ([]destEntry, error) {
	var out []destEntry
	var walk func(dir, relDir string) error
	walk = func(dir, relDir string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := ol.List(ctx, dir, false)
		if err != nil {
			return err
		}
		for _, o := range res.Content {
			if relDir == "" && o.Name == trashDirName {
				continue
			}
			rel := o.Name
			if relDir != "" {
				rel = relDir + "/" + o.Name
			}
			out = append(out, destEntry{rel: rel, isDir: o.IsDir})
			if o.IsDir {
				if err := walk(joinOpenList(dir, o.Name), rel); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(root, ""); err != nil {
		return nil, err
	}
	return out, nil
}

// hasTrash reports whether the trash folder currently exists.
func hasTrash(ctx context.Context, ol *openlist.Client, trashRoot string) bool {
	obj, err := ol.Get(ctx, trashRoot)
	return err == nil && obj.IsDir
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
