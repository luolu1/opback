package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"opback/internal/openlist"
	"opback/internal/types"
)

func TestDirectorySourceUsesDestinationAsRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"file.txt", filepath.Join("sub", "nested.txt")} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	files := collectFiles(root, types.JobOptions{}, func(string, ...any) {})
	got := make(map[string]string, len(files))
	dest := joinOpenList("/backups", filepath.Base(root))
	for _, f := range files {
		got[joinOpenList(dest, f.rel)] = f.rel
	}

	want := map[string]string{
		"/backups/" + filepath.Base(root) + "/file.txt":       "file.txt",
		"/backups/" + filepath.Base(root) + "/sub/nested.txt": "sub/nested.txt",
	}
	if len(got) != len(want) {
		t.Fatalf("got destination paths %v, want %v", got, want)
	}
	for path, rel := range want {
		if got[path] != rel {
			t.Fatalf("destination path %q has relative path %q, want %q", path, got[path], rel)
		}
	}
	if _, ok := got["/backups/file.txt"]; ok {
		t.Fatalf("source directory basename was not appended to destination: %v", got)
	}
}

func TestSingleFileSourceKeepsFilenameUnderDestination(t *testing.T) {
	root := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(root, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	path := joinOpenList("/backups", filepath.Base(root))
	if path != "/backups/file.txt" {
		t.Fatalf("single-file destination = %q, want /backups/file.txt", path)
	}
}

// TestUploadOneFileSkipsVanishedFile covers the race where a file is listed by
// collectFiles but disappears before the upload worker opens it. Live
// directories (such as the panel's own data dir, whose store is written via an
// atomic temp file + rename) routinely trigger this, and it must be reported
// as a skip rather than a failure.
func TestUploadOneFileSkipsVanishedFile(t *testing.T) {
	var (
		stats backupStats
		mu    sync.Mutex
		errs  []string
	)
	addErr := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}
	logf := func(string, ...any) {}

	f := sourceFile{
		rel:   "gone.txt",
		abs:   filepath.Join(t.TempDir(), "gone.txt"), // never created
		size:  12,
		mtime: 123,
	}
	// The file is opened before any network call, so the endpoint is never hit.
	ol := openlist.New("http://127.0.0.1:0", "", "")

	uploadOneFile(context.Background(), ol, types.Job{}, nil, f, &mu, &stats, addErr, logf)

	if stats.FilesSkipped != 1 {
		t.Fatalf("FilesSkipped = %d, want 1", stats.FilesSkipped)
	}
	if stats.FilesFailed != 0 {
		t.Fatalf("FilesFailed = %d, want 0", stats.FilesFailed)
	}
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}
