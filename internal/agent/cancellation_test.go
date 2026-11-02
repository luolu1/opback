package agent

import (
	"context"
	"testing"

	"opback/internal/types"
)

func TestCancelRunCancelsOnlyRequestedRun(t *testing.T) {
	agent := &Agent{cancels: map[string]context.CancelFunc{}}
	firstCtx, firstCancel := context.WithCancel(context.Background())
	secondCtx, secondCancel := context.WithCancel(context.Background())
	t.Cleanup(firstCancel)
	t.Cleanup(secondCancel)
	agent.registerRun("run-first", firstCancel)
	agent.registerRun("run-second", secondCancel)

	agent.cancelRun("run-first")

	if firstCtx.Err() != context.Canceled {
		t.Fatalf("first run error = %v, want context.Canceled", firstCtx.Err())
	}
	if secondCtx.Err() != nil {
		t.Fatalf("second run error = %v, want nil", secondCtx.Err())
	}
}

func TestFinalStatusCancelledPreservesTerminalClassification(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	status := finalStatus(ctx, backupStats{FilesTotal: 4, FilesDone: 2, FilesFailed: 1}, []string{"upload error"})

	if status != types.RunCancelled {
		t.Fatalf("status = %q, want %q", status, types.RunCancelled)
	}
}
