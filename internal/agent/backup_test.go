package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"opback/internal/openlist"
	"opback/internal/types"
)

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
