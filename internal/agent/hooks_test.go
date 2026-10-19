package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"opback/internal/types"
)

func testLogger() (func(string, ...any), func() []string) {
	var lines []string
	return func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}, func() []string { return lines }
}

func TestRunHookSuccessAndEnv(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "hook.txt")
	job := types.Job{
		ID:     "job_test",
		Name:   "unit",
		Source: dir,
		Dest:   "/backup",
		Options: types.JobOptions{
			PreCommand:         `printf '%s|%s|%s' "$OPBACK_JOB_NAME" "$OPBACK_HOOK" "$(pwd)" > hook.txt`,
			HookTimeoutSeconds: 10,
		},
	}

	logf, logs := testLogger()
	if err := runHook(context.Background(), "pre", job, types.TriggerManual, logf); err != nil {
		t.Fatalf("runHook: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read hook output: %v", err)
	}
	want := "unit|pre|" + dir
	if string(got) != want {
		t.Fatalf("hook output = %q, want %q", got, want)
	}
	if len(logs()) == 0 {
		t.Fatal("expected hook output to be logged")
	}
}

func TestRunHookEmptyIsNoop(t *testing.T) {
	job := types.Job{Options: types.JobOptions{}}
	logf, logs := testLogger()
	if err := runHook(context.Background(), "pre", job, types.TriggerSchedule, logf); err != nil {
		t.Fatalf("empty hook returned error: %v", err)
	}
	if len(logs()) != 0 {
		t.Fatalf("empty hook logged: %v", logs())
	}
}

func TestRunHookFailure(t *testing.T) {
	job := types.Job{
		Options: types.JobOptions{PreCommand: "echo boom >&2; exit 3", HookTimeoutSeconds: 10},
	}
	logf, _ := testLogger()
	err := runHook(context.Background(), "pre", job, types.TriggerManual, logf)
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error %q should include command output", err)
	}
}

func TestRunHookTimeout(t *testing.T) {
	job := types.Job{
		Options: types.JobOptions{PreCommand: "sleep 5", HookTimeoutSeconds: 1},
	}
	logf, _ := testLogger()
	start := time.Now()
	err := runHook(context.Background(), "pre", job, types.TriggerManual, logf)
	if err == nil {
		t.Fatal("expected timeout")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Fatalf("error %q should mention timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("hook took %v, timeout was not enforced", elapsed)
	}
}
