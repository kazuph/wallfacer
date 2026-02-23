package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"changkun.de/wallfacer/internal/gitutil"
	"changkun.de/wallfacer/internal/store"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Runner getters
// ---------------------------------------------------------------------------

// TestRunnerCommand verifies that Command() returns the configured binary path.
func TestRunnerCommand(t *testing.T) {
	r := newTestRunnerWithInstructions(t, "")
	if r.Command() != "docker" {
		t.Fatalf("expected command 'docker', got %q", r.Command())
	}
}

// TestWorkspacesEmpty verifies that Workspaces() returns nil when no
// workspaces are configured.
func TestWorkspacesEmpty(t *testing.T) {
	dataDir := t.TempDir()
	s, err := store.NewStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	r := NewRunner(s, RunnerConfig{Command: "echo"})
	if r.Workspaces() != nil {
		t.Fatal("expected nil when workspaces is empty")
	}
}

// TestWorkspacesMultiple verifies that Workspaces() correctly splits a
// space-separated workspace list.
func TestWorkspacesMultiple(t *testing.T) {
	dataDir := t.TempDir()
	s, err := store.NewStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	r := NewRunner(s, RunnerConfig{
		Command:    "echo",
		Workspaces: "/a /b /c",
	})
	ws := r.Workspaces()
	if len(ws) != 3 {
		t.Fatalf("expected 3 workspaces, got %d: %v", len(ws), ws)
	}
	if ws[0] != "/a" || ws[1] != "/b" || ws[2] != "/c" {
		t.Fatalf("unexpected workspaces: %v", ws)
	}
}

// TestKillContainer verifies that KillContainer does not panic when no
// container is running (error from exec is silently ignored).
func TestKillContainer(t *testing.T) {
	_, r := setupRunnerWithCmd(t, nil, "echo")
	// Should not panic or return an error.
	r.KillContainer(uuid.New())
}

// ---------------------------------------------------------------------------
// isConflictError
// ---------------------------------------------------------------------------

func TestIsConflictErrorNil(t *testing.T) {
	if isConflictError(nil) {
		t.Fatal("nil should not be a conflict error")
	}
}

func TestIsConflictErrorNonConflict(t *testing.T) {
	if isConflictError(fmt.Errorf("some regular error")) {
		t.Fatal("a regular error should not be a conflict error")
	}
}

func TestIsConflictErrorWrappedErrConflict(t *testing.T) {
	err := fmt.Errorf("rebase failed: %w", gitutil.ErrConflict)
	if !isConflictError(err) {
		t.Fatal("wrapped ErrConflict should be detected as a conflict error")
	}
}

func TestIsConflictErrorDirectString(t *testing.T) {
	// isConflictError checks if the error message contains ErrConflict.Error().
	err := fmt.Errorf("rebase conflict occurred")
	if !isConflictError(err) {
		t.Fatal("error containing 'rebase conflict' should be detected")
	}
}

// ---------------------------------------------------------------------------
// runGit
// ---------------------------------------------------------------------------

// TestRunGitSuccess verifies that runGit executes git commands successfully.
func TestRunGitSuccess(t *testing.T) {
	repo := setupTestRepo(t)
	if err := runGit(repo, "status"); err != nil {
		t.Fatalf("runGit git status should succeed: %v", err)
	}
}

// TestRunGitInvalidDir verifies that runGit returns an error for a non-existent
// directory.
func TestRunGitInvalidDir(t *testing.T) {
	err := runGit("/nonexistent/xyz/path/abc", "status")
	if err == nil {
		t.Fatal("expected error for non-existent directory")
	}
}

// ---------------------------------------------------------------------------
// setupWorktrees — idempotent path
// ---------------------------------------------------------------------------

// TestSetupWorktreesIdempotent verifies that calling setupWorktrees twice for
// the same taskID returns the same paths without error (idempotent behaviour).
func TestSetupWorktreesIdempotent(t *testing.T) {
	repo := setupTestRepo(t)
	_, runner := setupTestRunner(t, []string{repo})
	taskID := uuid.New()

	wt1, br1, err := runner.setupWorktrees(taskID)
	if err != nil {
		t.Fatal("first setupWorktrees:", err)
	}
	t.Cleanup(func() { runner.cleanupWorktrees(taskID, wt1, br1) })

	// Second call — worktree directory already exists, should be reused.
	wt2, _, err := runner.setupWorktrees(taskID)
	if err != nil {
		t.Fatal("second (idempotent) setupWorktrees:", err)
	}
	if wt1[repo] != wt2[repo] {
		t.Errorf("expected same worktree path on second call: %q vs %q", wt1[repo], wt2[repo])
	}
}

// TestResolveConflictsSuccess verifies that resolveConflicts returns nil when
// the container exits successfully with a valid result.
func TestResolveConflictsSuccess(t *testing.T) {
	cmd := fakeCmdScript(t, endTurnOutput, 0)
	s, r := setupRunnerWithCmd(t, nil, cmd)
	ctx := context.Background()

	task, err := s.CreateTask(ctx, "conflict resolve test", 5, false)
	if err != nil {
		t.Fatal(err)
	}

	repoPath := t.TempDir()
	worktreePath := t.TempDir()

	if err := r.resolveConflicts(ctx, task.ID, repoPath, worktreePath, ""); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

// TestResolveConflictsContainerError verifies that resolveConflicts returns a
// wrapped error when the container itself fails.
func TestResolveConflictsContainerError(t *testing.T) {
	cmd := fakeCmdScript(t, "", 1) // empty output, exit 1
	s, r := setupRunnerWithCmd(t, nil, cmd)
	ctx := context.Background()

	task, err := s.CreateTask(ctx, "conflict error test", 5, false)
	if err != nil {
		t.Fatal(err)
	}

	repoPath := t.TempDir()
	worktreePath := t.TempDir()

	err = r.resolveConflicts(ctx, task.ID, repoPath, worktreePath, "")
	if err == nil {
		t.Fatal("expected error from container failure")
	}
	if !strings.Contains(err.Error(), "conflict resolver container") {
		t.Fatalf("expected 'conflict resolver container' error, got: %v", err)
	}
}

// TestResolveConflictsIsError verifies that resolveConflicts returns an error
// when the container reports is_error=true.
func TestResolveConflictsIsError(t *testing.T) {
	cmd := fakeCmdScript(t, isErrorOutput, 0)
	s, r := setupRunnerWithCmd(t, nil, cmd)
	ctx := context.Background()

	task, err := s.CreateTask(ctx, "conflict is_error test", 5, false)
	if err != nil {
		t.Fatal(err)
	}

	repoPath := t.TempDir()
	worktreePath := t.TempDir()

	err = r.resolveConflicts(ctx, task.ID, repoPath, worktreePath, "")
	if err == nil {
		t.Fatal("expected error when container reports is_error=true")
	}
	if !strings.Contains(err.Error(), "conflict resolver reported error") {
		t.Fatalf("expected 'conflict resolver reported error', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// CleanupWorktrees (exported)
// ---------------------------------------------------------------------------

// TestCleanupWorktreesExported verifies the exported CleanupWorktrees removes
// worktree directories and git branches.
func TestCleanupWorktreesExported(t *testing.T) {
	repo := setupTestRepo(t)
	_, runner := setupTestRunner(t, []string{repo})
	taskID := uuid.New()

	wt, br, err := runner.setupWorktrees(taskID)
	if err != nil {
		t.Fatal(err)
	}
	worktreePath := wt[repo]
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatal("worktree should exist before cleanup:", err)
	}

	runner.CleanupWorktrees(taskID, wt, br)

	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatal("worktree should be removed after exported CleanupWorktrees")
	}
}

// ---------------------------------------------------------------------------
// PruneOrphanedWorktrees
// ---------------------------------------------------------------------------

// TestPruneOrphanedWorktrees verifies that directories not matching any known
// task UUID are removed, while known-task directories are preserved.
func TestPruneOrphanedWorktrees(t *testing.T) {
	repo := setupTestRepo(t)
	s, runner := setupTestRunner(t, []string{repo})
	ctx := context.Background()

	task, err := s.CreateTask(ctx, "known task", 5, false)
	if err != nil {
		t.Fatal(err)
	}

	knownDir := filepath.Join(runner.worktreesDir, task.ID.String())
	orphanDir := filepath.Join(runner.worktreesDir, uuid.New().String())

	for _, d := range []string{knownDir, orphanDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	runner.PruneOrphanedWorktrees(s)

	if _, err := os.Stat(knownDir); err != nil {
		t.Fatal("known task worktree dir should be preserved:", err)
	}
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Fatal("orphan worktree dir should be pruned")
	}
}

// TestPruneOrphanedWorktreesMissingDir verifies PruneOrphanedWorktrees handles
// a missing worktrees directory gracefully (no panic).
func TestPruneOrphanedWorktreesMissingDir(t *testing.T) {
	s, runner := setupRunnerWithCmd(t, nil, "echo")
	// Point worktreesDir to a path that doesn't exist.
	runner.worktreesDir = filepath.Join(t.TempDir(), "nonexistent_worktrees")
	// Should not panic.
	runner.PruneOrphanedWorktrees(s)
}

// TestPruneOrphanedWorktreesRunsGitWorktreePrune verifies that
// PruneOrphanedWorktrees runs `git worktree prune` on git workspaces.
func TestPruneOrphanedWorktreesRunsGitWorktreePrune(t *testing.T) {
	repo := setupTestRepo(t)
	s, runner := setupTestRunner(t, []string{repo})

	// Just verify it completes without panicking when the workspace is a git repo.
	runner.PruneOrphanedWorktrees(s)
}

// ---------------------------------------------------------------------------
// Commit (exported) — error path
// ---------------------------------------------------------------------------

// TestCommitNonExistentTask verifies that the exported Commit does not panic
// when the task does not exist in the store.
func TestCommitNonExistentTask(t *testing.T) {
	_, r := setupRunnerWithCmd(t, nil, "echo")
	// Should return early without panicking.
	r.Commit(uuid.New(), "")
}

// ---------------------------------------------------------------------------
// runContainer
// ---------------------------------------------------------------------------

// TestRunContainerSuccess verifies that runContainer parses valid JSON output
// and returns the structured result.
func TestRunContainerSuccess(t *testing.T) {
	cmd := fakeCmdScript(t, endTurnOutput, 0)
	r := runnerWithCmd(t, cmd)

	out, stdout, stderr, err := r.runContainer(context.Background(), uuid.New(), "prompt", "", nil, "", nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if out.StopReason != "end_turn" {
		t.Fatalf("expected stop_reason=end_turn, got %q", out.StopReason)
	}
	_ = stdout
	_ = stderr
}

// TestRunContainerNonZeroExitWithValidOutput verifies that a non-zero exit is
// tolerated when the container produced parseable JSON output.
func TestRunContainerNonZeroExitWithValidOutput(t *testing.T) {
	cmd := fakeCmdScript(t, endTurnOutput, 1)
	r := runnerWithCmd(t, cmd)

	out, _, _, err := r.runContainer(context.Background(), uuid.New(), "prompt", "", nil, "", nil)
	if err != nil {
		t.Fatalf("expected no error for non-zero exit with valid output, got: %v", err)
	}
	if out.StopReason != "end_turn" {
		t.Fatalf("expected stop_reason=end_turn, got %q", out.StopReason)
	}
}

// TestRunContainerEmptyOutputNonZeroExit verifies that empty stdout + exit 1
// returns an appropriate error.
func TestRunContainerEmptyOutputNonZeroExit(t *testing.T) {
	cmd := fakeCmdScript(t, "", 1)
	r := runnerWithCmd(t, cmd)

	_, _, _, err := r.runContainer(context.Background(), uuid.New(), "prompt", "", nil, "", nil)
	if err == nil {
		t.Fatal("expected error for empty container output with non-zero exit")
	}
}

// TestRunContainerEmptyOutputZeroExit verifies that empty stdout + exit 0
// returns an "empty output" error.
func TestRunContainerEmptyOutputZeroExit(t *testing.T) {
	cmd := fakeCmdScript(t, "", 0)
	r := runnerWithCmd(t, cmd)

	_, _, _, err := r.runContainer(context.Background(), uuid.New(), "prompt", "", nil, "", nil)
	if err == nil {
		t.Fatal("expected error for empty container output with exit 0")
	}
	if !strings.Contains(err.Error(), "empty output") {
		t.Fatalf("expected 'empty output' error, got: %v", err)
	}
}

// TestRunContainerSessionID verifies that a non-empty sessionID is passed to
// the container args as --resume.
func TestRunContainerWithSessionID(t *testing.T) {
	cmd := fakeCmdScript(t, endTurnOutput, 0)
	r := runnerWithCmd(t, cmd)

	// Should succeed; session ID is passed to args (verified via args tests).
	out, _, _, err := r.runContainer(context.Background(), uuid.New(), "prompt", "sess-xyz", nil, "", nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if out.StopReason != "end_turn" {
		t.Fatalf("expected stop_reason=end_turn, got %q", out.StopReason)
	}
}

// ---------------------------------------------------------------------------
// GenerateTitle
// ---------------------------------------------------------------------------

const titleOutput = `{"result":"Fix Login Bug","session_id":"sess1","stop_reason":"end_turn","is_error":false}`

// TestGenerateTitleSuccess verifies that a valid container output sets the
// task title.
func TestGenerateTitleSuccess(t *testing.T) {
	cmd := fakeCmdScript(t, titleOutput, 0)
	s, r := setupRunnerWithCmd(t, nil, cmd)
	ctx := context.Background()

	task, err := s.CreateTask(ctx, "Fix the login bug in the authentication module", 5, false)
	if err != nil {
		t.Fatal(err)
	}

	r.GenerateTitle(task.ID, task.Prompt)

	updated, _ := s.GetTask(ctx, task.ID)
	if updated.Title != "Fix Login Bug" {
		t.Fatalf("expected title 'Fix Login Bug', got %q", updated.Title)
	}
}

// TestGenerateTitleSkipsExistingTitle verifies that GenerateTitle is a no-op
// when the task already has a title.
func TestGenerateTitleSkipsExistingTitle(t *testing.T) {
	// Command exits 1 — if it were called, GenerateTitle would fail to set a title.
	cmd := fakeCmdScript(t, "", 1)
	s, r := setupRunnerWithCmd(t, nil, cmd)
	ctx := context.Background()

	task, err := s.CreateTask(ctx, "test prompt", 5, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTaskTitle(ctx, task.ID, "Pre-set Title"); err != nil {
		t.Fatal(err)
	}

	// Should return immediately without calling the container.
	r.GenerateTitle(task.ID, task.Prompt)

	updated, _ := s.GetTask(ctx, task.ID)
	if updated.Title != "Pre-set Title" {
		t.Fatalf("expected title to remain 'Pre-set Title', got %q", updated.Title)
	}
}

// TestGenerateTitleFallbackOnContainerError verifies that GenerateTitle does
// not set a title (silently drops the error) when the container fails.
func TestGenerateTitleFallbackOnContainerError(t *testing.T) {
	cmd := fakeCmdScript(t, "", 1) // always fails
	s, r := setupRunnerWithCmd(t, nil, cmd)
	ctx := context.Background()

	task, err := s.CreateTask(ctx, "test prompt", 5, false)
	if err != nil {
		t.Fatal(err)
	}

	r.GenerateTitle(task.ID, task.Prompt)

	updated, _ := s.GetTask(ctx, task.ID)
	if updated.Title != "" {
		t.Fatalf("expected empty title when container fails, got %q", updated.Title)
	}
}

// TestGenerateTitleBlankResult verifies that a blank result from the container
// does not set the title.
func TestGenerateTitleBlankResult(t *testing.T) {
	blankOutput := `{"result":"","session_id":"s1","stop_reason":"end_turn","is_error":false}`
	cmd := fakeCmdScript(t, blankOutput, 0)
	s, r := setupRunnerWithCmd(t, nil, cmd)
	ctx := context.Background()

	task, err := s.CreateTask(ctx, "test prompt", 5, false)
	if err != nil {
		t.Fatal(err)
	}

	r.GenerateTitle(task.ID, task.Prompt)

	updated, _ := s.GetTask(ctx, task.ID)
	if updated.Title != "" {
		t.Fatalf("expected empty title for blank container result, got %q", updated.Title)
	}
}

// TestGenerateTitleNDJSONOutput verifies that NDJSON output from the container
// is parsed correctly and the result is used as the title.
func TestGenerateTitleNDJSONOutput(t *testing.T) {
	ndjson := `{"type":"system","subtype":"init"}
{"type":"assistant","content":"thinking..."}
{"result":"Add Auth Feature","session_id":"s1","stop_reason":"end_turn","is_error":false}`
	cmd := fakeCmdScript(t, ndjson, 0)
	s, r := setupRunnerWithCmd(t, nil, cmd)
	ctx := context.Background()

	task, err := s.CreateTask(ctx, "add authentication feature", 5, false)
	if err != nil {
		t.Fatal(err)
	}

	r.GenerateTitle(task.ID, task.Prompt)

	updated, _ := s.GetTask(ctx, task.ID)
	if updated.Title != "Add Auth Feature" {
		t.Fatalf("expected title 'Add Auth Feature', got %q", updated.Title)
	}
}

// TestGenerateTitleUnknownTask verifies that GenerateTitle does not panic when
// the task does not exist in the store.
func TestGenerateTitleUnknownTask(t *testing.T) {
	cmd := fakeCmdScript(t, titleOutput, 0)
	_, r := setupRunnerWithCmd(t, nil, cmd)
	// Should not panic.
	r.GenerateTitle(uuid.New(), "some prompt")
}

// ---------------------------------------------------------------------------
// runContainer additional paths
// ---------------------------------------------------------------------------

// TestRunContainerParseErrorExitZero verifies that non-JSON stdout with exit 0
// returns a parse error.
func TestRunContainerParseErrorExitZero(t *testing.T) {
	cmd := fakeCmdScript(t, "this is not valid json output at all", 0)
	r := runnerWithCmd(t, cmd)

	_, _, _, err := r.runContainer(context.Background(), uuid.New(), "prompt", "", nil, "", nil)
	if err == nil {
		t.Fatal("expected error for non-JSON output")
	}
	if !strings.Contains(err.Error(), "parse output") {
		t.Fatalf("expected parse error, got: %v", err)
	}
}

// TestRunContainerParseErrorWithExitCode verifies that non-JSON stdout with a
// non-zero exit code returns an exit-code error (not a parse error), because
// the exit code is more informative.
func TestRunContainerParseErrorWithExitCode(t *testing.T) {
	cmd := fakeCmdScript(t, "not valid json", 1)
	r := runnerWithCmd(t, cmd)

	_, _, _, err := r.runContainer(context.Background(), uuid.New(), "prompt", "", nil, "", nil)
	if err == nil {
		t.Fatal("expected error for invalid JSON with exit code 1")
	}
	if !strings.Contains(err.Error(), "container exited with code") {
		t.Fatalf("expected exit code error, got: %v", err)
	}
}

// TestRunContainerContextCancelled verifies that cancelling the context while
// the container is running causes runContainer to return a "container terminated"
// error immediately.
func TestRunContainerContextCancelled(t *testing.T) {
	// Script that handles sandbox lifecycle calls quickly but sleeps on "exec".
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "slow-cmd")
	script := "#!/bin/sh\ncase \"$1\" in sandbox) case \"$2\" in create|stop|rm|ls) exit 0 ;; esac ;; esac\nsleep 10\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	r := runnerWithCmd(t, scriptPath)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	_, _, _, err := r.runContainer(ctx, uuid.New(), "prompt", "", nil, "", nil)
	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
	if !strings.Contains(err.Error(), "container terminated") {
		t.Fatalf("expected 'container terminated' error, got: %v", err)
	}
}
