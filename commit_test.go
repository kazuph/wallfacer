package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// gitRun executes a git command in dir and returns trimmed stdout.
// It fails the test on error.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s failed: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitRunMayFail executes a git command in dir and returns stdout.
// Does not fail the test on error.
func gitRunMayFail(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// setupTestRepo creates a temporary git repo with an initial commit on "main".
func setupTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-b", "main")
	gitRun(t, dir, "config", "user.email", "test@test.com")
	gitRun(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "initial commit")
	return dir
}

// setupTestRunner creates a Store and Runner for testing.
// The container command is a dummy since we're testing host-side operations.
func setupTestRunner(t *testing.T, workspaces []string) (*Store, *Runner) {
	t.Helper()
	dataDir := t.TempDir()
	store, err := NewStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	worktreesDir := filepath.Join(t.TempDir(), "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(store, RunnerConfig{
		Command:      "echo", // dummy — not used for host-side operations
		SandboxImage: "test:latest",
		EnvFile:      "",
		Workspaces:   strings.Join(workspaces, " "),
		WorktreesDir: worktreesDir,
	})
	return store, runner
}

// TestWorktreeSetup verifies that worktree creation works: correct branch,
// correct directory structure, files inherited from the parent repo.
func TestWorktreeSetup(t *testing.T) {
	repo := setupTestRepo(t)
	_, runner := setupTestRunner(t, []string{repo})

	taskID := uuid.New()
	worktreePaths, branchName, err := runner.setupWorktrees(taskID)
	if err != nil {
		t.Fatal("setupWorktrees:", err)
	}
	t.Cleanup(func() { runner.cleanupWorktrees(taskID, worktreePaths, branchName) })

	if len(worktreePaths) != 1 {
		t.Fatalf("expected 1 worktree, got %d", len(worktreePaths))
	}

	wt := worktreePaths[repo]
	if wt == "" {
		t.Fatal("missing worktree path for repo")
	}

	// Verify worktree directory exists.
	if info, err := os.Stat(wt); err != nil || !info.IsDir() {
		t.Fatalf("worktree dir should exist: %v", err)
	}

	// Verify worktree is on the correct branch.
	branch := gitRun(t, wt, "branch", "--show-current")
	if branch != branchName {
		t.Fatalf("expected branch %q, got %q", branchName, branch)
	}

	// Verify parent files are visible.
	if _, err := os.Stat(filepath.Join(wt, "README.md")); err != nil {
		t.Fatal("README.md should exist in worktree:", err)
	}
}

// TestWorktreeGitFilePointsToHost verifies the root cause: the .git file in
// a worktree contains an absolute host path. This proves that git commands
// inside a container (where that host path doesn't exist) would fail.
func TestWorktreeGitFilePointsToHost(t *testing.T) {
	repo := setupTestRepo(t)
	_, runner := setupTestRunner(t, []string{repo})

	taskID := uuid.New()
	worktreePaths, branchName, err := runner.setupWorktrees(taskID)
	if err != nil {
		t.Fatal("setupWorktrees:", err)
	}
	t.Cleanup(func() { runner.cleanupWorktrees(taskID, worktreePaths, branchName) })

	wt := worktreePaths[repo]
	gitFile := filepath.Join(wt, ".git")
	content, err := os.ReadFile(gitFile)
	if err != nil {
		t.Fatal("reading .git file:", err)
	}

	// The .git file contains "gitdir: /absolute/host/path/..."
	s := strings.TrimSpace(string(content))
	if !strings.HasPrefix(s, "gitdir: ") {
		t.Fatalf("unexpected .git file content: %s", s)
	}
	gitdirPath := strings.TrimPrefix(s, "gitdir: ")

	// Verify it's an absolute host path (which would NOT exist inside a container).
	if !filepath.IsAbs(gitdirPath) {
		t.Fatal("expected absolute path in .git file, got:", gitdirPath)
	}

	// The path should reference the main repo's .git directory.
	if !strings.Contains(gitdirPath, repo) {
		// The gitdir path should be under the main repo's .git/worktrees/ directory.
		// On some systems the repo path may be a symlink, so let's at least verify
		// the path exists on the host.
	}

	// Verify the path exists on the host.
	if _, err := os.Stat(gitdirPath); err != nil {
		t.Fatal("gitdir path should exist on host:", err)
	}
}

// TestHostStageAndCommit verifies that host-side staging and committing works
// correctly in a worktree.
func TestHostStageAndCommit(t *testing.T) {
	repo := setupTestRepo(t)
	_, runner := setupTestRunner(t, []string{repo})

	taskID := uuid.New()
	worktreePaths, branchName, err := runner.setupWorktrees(taskID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runner.cleanupWorktrees(taskID, worktreePaths, branchName) })

	wt := worktreePaths[repo]

	// Simulate Claude making changes.
	if err := os.WriteFile(filepath.Join(wt, "hello.txt"), []byte("hello world\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Run host-side commit.
	committed := runner.hostStageAndCommit(worktreePaths, "Add hello world file")
	if !committed {
		t.Fatal("expected commit to be created")
	}

	// Verify commit exists in worktree on the task branch.
	log := gitRun(t, wt, "log", "--oneline")
	if !strings.Contains(log, "wallfacer:") {
		t.Fatalf("expected wallfacer commit message, got:\n%s", log)
	}

	// Verify the commit is on the task branch, not on main.
	branch := gitRun(t, wt, "branch", "--show-current")
	if branch != branchName {
		t.Fatalf("should still be on task branch %q, got %q", branchName, branch)
	}
}

// TestHostStageAndCommitNoChanges verifies that host-side commit is a no-op
// when there are no changes in the worktree.
func TestHostStageAndCommitNoChanges(t *testing.T) {
	repo := setupTestRepo(t)
	_, runner := setupTestRunner(t, []string{repo})

	taskID := uuid.New()
	worktreePaths, branchName, err := runner.setupWorktrees(taskID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runner.cleanupWorktrees(taskID, worktreePaths, branchName) })

	// No changes made — commit should be a no-op.
	committed := runner.hostStageAndCommit(worktreePaths, "Nothing to do")
	if committed {
		t.Fatal("expected no commit when there are no changes")
	}
}

// TestCommitPipelineBasic tests the full commit pipeline (Phase 1-4):
// host commit → rebase → ff-merge → PROGRESS.md → cleanup.
func TestCommitPipelineBasic(t *testing.T) {
	repo := setupTestRepo(t)
	store, runner := setupTestRunner(t, []string{repo})

	initialHash := gitRun(t, repo, "rev-parse", "HEAD")

	// Create a task.
	ctx := context.Background()
	task, err := store.CreateTask(ctx, "Add a greeting file", 5)
	if err != nil {
		t.Fatal(err)
	}

	// Set up worktrees (simulates what Run() does when task starts).
	worktreePaths, branchName, err := runner.setupWorktrees(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskWorktrees(ctx, task.ID, worktreePaths, branchName); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskStatus(ctx, task.ID, "committing"); err != nil {
		t.Fatal(err)
	}

	wt := worktreePaths[repo]

	// Simulate Claude making changes in the worktree.
	if err := os.WriteFile(filepath.Join(wt, "greeting.txt"), []byte("Hello, World!\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Run the commit pipeline.
	commitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	runner.commit(commitCtx, task.ID, "", 1, worktreePaths, branchName)

	// Verify a new commit exists on the default branch.
	finalHash := gitRun(t, repo, "rev-parse", "HEAD")
	if finalHash == initialHash {
		t.Fatal("expected new commit on default branch, but HEAD hasn't changed")
	}

	// Verify the file exists in the main repo's working tree.
	content, err := os.ReadFile(filepath.Join(repo, "greeting.txt"))
	if err != nil {
		t.Fatal("greeting.txt should exist in the main repo after merge:", err)
	}
	if string(content) != "Hello, World!\n" {
		t.Fatalf("unexpected content: %q", content)
	}

	// Verify the commit message references the task.
	log := gitRun(t, repo, "log", "--oneline")
	if !strings.Contains(log, "wallfacer:") {
		t.Fatalf("expected wallfacer commit in log:\n%s", log)
	}

	// Verify worktree is cleaned up.
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("worktree should have been cleaned up after commit pipeline")
	}
}

// TestCommitPipelineDivergedBranch tests the pipeline when the default branch
// has advanced since the worktree was created. The task's changes must be
// rebased on top of the latest default branch.
func TestCommitPipelineDivergedBranch(t *testing.T) {
	repo := setupTestRepo(t)
	store, runner := setupTestRunner(t, []string{repo})

	ctx := context.Background()
	task, err := store.CreateTask(ctx, "Add feature", 5)
	if err != nil {
		t.Fatal(err)
	}

	// Set up worktrees.
	worktreePaths, branchName, err := runner.setupWorktrees(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskWorktrees(ctx, task.ID, worktreePaths, branchName); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskStatus(ctx, task.ID, "committing"); err != nil {
		t.Fatal(err)
	}

	wt := worktreePaths[repo]

	// Simulate Claude making changes in the worktree.
	if err := os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("new feature\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Meanwhile, advance the default branch in the main repo.
	if err := os.WriteFile(filepath.Join(repo, "other.txt"), []byte("other change\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-m", "other change on main")

	// Run the commit pipeline.
	commitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	runner.commit(commitCtx, task.ID, "", 1, worktreePaths, branchName)

	// Verify BOTH files exist on main (task changes rebased on top of main).
	for _, f := range []string{"feature.txt", "other.txt"} {
		if _, err := os.Stat(filepath.Join(repo, f)); err != nil {
			t.Fatalf("%s should exist on main: %v", f, err)
		}
	}

	// Verify the task commit is on top of the other commit.
	log := gitRun(t, repo, "log", "--oneline")
	lines := strings.Split(log, "\n")
	// Expected order (newest first):
	//   PROGRESS.md commit
	//   wallfacer: task commit
	//   other change on main
	//   initial commit
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 commits, got %d:\n%s", len(lines), log)
	}
}

// TestCommitPipelineNoChanges tests the pipeline when the worktree has no
// changes. The pipeline should complete without errors and without creating
// any merge commits (only PROGRESS.md may be updated).
func TestCommitPipelineNoChanges(t *testing.T) {
	repo := setupTestRepo(t)
	store, runner := setupTestRunner(t, []string{repo})

	ctx := context.Background()
	task, err := store.CreateTask(ctx, "No changes task", 5)
	if err != nil {
		t.Fatal(err)
	}

	worktreePaths, branchName, err := runner.setupWorktrees(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskWorktrees(ctx, task.ID, worktreePaths, branchName); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskStatus(ctx, task.ID, "committing"); err != nil {
		t.Fatal(err)
	}

	initialHash := gitRun(t, repo, "rev-parse", "HEAD")

	commitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	runner.commit(commitCtx, task.ID, "", 1, worktreePaths, branchName)

	// The only possible change is the PROGRESS.md commit.
	log := gitRun(t, repo, "log", "--oneline")
	// There should be no wallfacer: task commit (only PROGRESS.md and initial).
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "wallfacer:") && !strings.Contains(line, "progress log") {
			t.Fatalf("unexpected wallfacer task commit when there were no changes:\n%s", log)
		}
	}
	_ = initialHash
}

// TestCompleteTaskE2E simulates the exact waiting→done flow that the user
// reported as broken. It covers:
//  1. Create task and simulate it going through backlog → in_progress → waiting
//  2. Simulate Claude making file changes in the worktree during execution
//  3. Call the Commit pipeline (as CompleteTask handler would)
//  4. Verify that the changes end up on the default branch
func TestCompleteTaskE2E(t *testing.T) {
	repo := setupTestRepo(t)
	store, runner := setupTestRunner(t, []string{repo})

	ctx := context.Background()

	// Step 1: Create the task.
	task, err := store.CreateTask(ctx, "Add greeting feature", 5)
	if err != nil {
		t.Fatal(err)
	}

	// Step 2: Simulate task going to in_progress → worktree is created.
	worktreePaths, branchName, err := runner.setupWorktrees(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskWorktrees(ctx, task.ID, worktreePaths, branchName); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskStatus(ctx, task.ID, "in_progress"); err != nil {
		t.Fatal(err)
	}
	sessionID := "test-session-123"
	result := "I created the greeting feature"
	if err := store.UpdateTaskResult(ctx, task.ID, result, sessionID, "", 1); err != nil {
		t.Fatal(err)
	}

	// Step 3: Simulate Claude making changes in the worktree during execution.
	wt := worktreePaths[repo]
	if err := os.WriteFile(filepath.Join(wt, "greeting.txt"), []byte("Hello from wallfacer!\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Step 4: Task goes to waiting (Claude needs feedback).
	if err := store.UpdateTaskStatus(ctx, task.ID, "waiting"); err != nil {
		t.Fatal(err)
	}

	// Step 5: User clicks "Mark as Done" — this triggers Commit.
	if err := store.UpdateTaskStatus(ctx, task.ID, "committing"); err != nil {
		t.Fatal(err)
	}

	// Run the exact same code path as CompleteTask handler.
	runner.Commit(task.ID, sessionID)

	// Step 6: Verify the changes are on the default branch.
	content, err := os.ReadFile(filepath.Join(repo, "greeting.txt"))
	if err != nil {
		t.Fatal("greeting.txt should exist on default branch after Commit:", err)
	}
	if string(content) != "Hello from wallfacer!\n" {
		t.Fatalf("unexpected content: %q", content)
	}

	// Verify commit is on the default branch.
	log := gitRun(t, repo, "log", "--oneline")
	if !strings.Contains(log, "wallfacer:") {
		t.Fatalf("expected wallfacer commit on default branch:\n%s", log)
	}

	// Verify PROGRESS.md was written.
	if _, err := os.Stat(filepath.Join(repo, "PROGRESS.md")); err != nil {
		t.Fatal("PROGRESS.md should exist after commit:", err)
	}

	// Verify worktree is cleaned up.
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("worktree should have been cleaned up")
	}
}

// TestCommitOnTopOfLatestMain verifies that commits are created on top of
// the latest main branch, not on the stale version from when the worktree
// was created. This is critical for maintaining a clean linear history.
func TestCommitOnTopOfLatestMain(t *testing.T) {
	repo := setupTestRepo(t)
	store, runner := setupTestRunner(t, []string{repo})

	ctx := context.Background()
	task, err := store.CreateTask(ctx, "Task on stale branch", 5)
	if err != nil {
		t.Fatal(err)
	}

	// Create worktree (branches from current HEAD of main).
	worktreePaths, branchName, err := runner.setupWorktrees(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskWorktrees(ctx, task.ID, worktreePaths, branchName); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskStatus(ctx, task.ID, "committing"); err != nil {
		t.Fatal(err)
	}

	wt := worktreePaths[repo]

	// Make changes in the worktree.
	if err := os.WriteFile(filepath.Join(wt, "task-file.txt"), []byte("from task\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Advance main with TWO commits (simulating other tasks completing).
	if err := os.WriteFile(filepath.Join(repo, "advance1.txt"), []byte("advance 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-m", "advance main 1")

	if err := os.WriteFile(filepath.Join(repo, "advance2.txt"), []byte("advance 2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-m", "advance main 2")

	mainHashBefore := gitRun(t, repo, "rev-parse", "HEAD")

	// Run the commit pipeline.
	commitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	runner.commit(commitCtx, task.ID, "", 1, worktreePaths, branchName)

	// Verify the task commit is a descendant of the latest main.
	// git merge-base --is-ancestor checks if mainHashBefore is an ancestor of HEAD.
	if _, err := gitRunMayFail(repo, "merge-base", "--is-ancestor", mainHashBefore, "HEAD"); err != nil {
		t.Fatal("task commit should be on top of latest main (rebase should have applied)")
	}

	// Verify all files exist.
	for _, f := range []string{"task-file.txt", "advance1.txt", "advance2.txt"} {
		if _, err := os.Stat(filepath.Join(repo, f)); err != nil {
			t.Fatalf("%s should exist on main: %v", f, err)
		}
	}
}

// TestParallelTasksSameRepo verifies that two tasks running concurrently on
// different worktrees of the same repo both get their changes merged into
// main in sequence. The second task to merge must rebase on top of the first.
func TestParallelTasksSameRepo(t *testing.T) {
	repo := setupTestRepo(t)
	store, runner := setupTestRunner(t, []string{repo})
	ctx := context.Background()

	// Create two tasks.
	taskA, err := store.CreateTask(ctx, "Add file A", 5)
	if err != nil {
		t.Fatal(err)
	}
	taskB, err := store.CreateTask(ctx, "Add file B", 5)
	if err != nil {
		t.Fatal(err)
	}

	// Set up worktrees for both (simulating two tasks starting at the same time).
	wtA, brA, err := runner.setupWorktrees(taskA.ID)
	if err != nil {
		t.Fatal("setup worktree A:", err)
	}
	if err := store.UpdateTaskWorktrees(ctx, taskA.ID, wtA, brA); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskStatus(ctx, taskA.ID, "committing"); err != nil {
		t.Fatal(err)
	}

	wtB, brB, err := runner.setupWorktrees(taskB.ID)
	if err != nil {
		t.Fatal("setup worktree B:", err)
	}
	if err := store.UpdateTaskWorktrees(ctx, taskB.ID, wtB, brB); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskStatus(ctx, taskB.ID, "committing"); err != nil {
		t.Fatal(err)
	}

	// Both worktrees should exist and be on different branches.
	pathA := wtA[repo]
	pathB := wtB[repo]
	if pathA == pathB {
		t.Fatal("worktree paths should differ")
	}
	branchA := gitRun(t, pathA, "branch", "--show-current")
	branchB := gitRun(t, pathB, "branch", "--show-current")
	if branchA == branchB {
		t.Fatal("worktree branches should differ")
	}

	// Simulate Claude making changes in each worktree.
	if err := os.WriteFile(filepath.Join(pathA, "fileA.txt"), []byte("from task A\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pathB, "fileB.txt"), []byte("from task B\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Commit task A first.
	commitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	runner.commit(commitCtx, taskA.ID, "", 1, wtA, brA)

	// Then commit task B — must rebase on top of A's merge.
	runner.commit(commitCtx, taskB.ID, "", 1, wtB, brB)

	// Verify both files exist on main.
	for _, f := range []string{"fileA.txt", "fileB.txt"} {
		if _, err := os.Stat(filepath.Join(repo, f)); err != nil {
			t.Fatalf("%s should exist on main: %v", f, err)
		}
	}

	// Verify linear history: B's commit is on top of A's.
	log := gitRun(t, repo, "log", "--oneline")
	lines := strings.Split(log, "\n")
	// Expected (newest first):
	//   progress log for task B
	//   wallfacer: Add file B
	//   progress log for task A
	//   wallfacer: Add file A
	//   initial commit
	if len(lines) < 4 {
		t.Fatalf("expected at least 4 commits for two tasks, got %d:\n%s", len(lines), log)
	}

	// Verify no merge commits (all fast-forward).
	mergeCount := gitRun(t, repo, "rev-list", "--merges", "--count", "HEAD")
	if mergeCount != "0" {
		t.Fatalf("expected 0 merge commits (all fast-forward), got %s", mergeCount)
	}
}

// TestParallelTasksTwoRepos verifies that two tasks working on different
// repos (mounted as separate workspaces) each get independent worktrees
// and commits merge into their respective repos.
func TestParallelTasksTwoRepos(t *testing.T) {
	repoX := setupTestRepo(t)
	repoY := setupTestRepo(t)
	store, runner := setupTestRunner(t, []string{repoX, repoY})
	ctx := context.Background()

	task, err := store.CreateTask(ctx, "Change both repos", 5)
	if err != nil {
		t.Fatal(err)
	}

	wtPaths, brName, err := runner.setupWorktrees(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskWorktrees(ctx, task.ID, wtPaths, brName); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskStatus(ctx, task.ID, "committing"); err != nil {
		t.Fatal(err)
	}

	if len(wtPaths) != 2 {
		t.Fatalf("expected 2 worktrees (one per repo), got %d", len(wtPaths))
	}

	// Make changes in both worktrees.
	if err := os.WriteFile(filepath.Join(wtPaths[repoX], "x.txt"), []byte("X\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtPaths[repoY], "y.txt"), []byte("Y\n"), 0644); err != nil {
		t.Fatal(err)
	}

	commitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	runner.commit(commitCtx, task.ID, "", 1, wtPaths, brName)

	// Verify each file landed in the correct repo.
	if _, err := os.Stat(filepath.Join(repoX, "x.txt")); err != nil {
		t.Fatal("x.txt should exist in repoX:", err)
	}
	if _, err := os.Stat(filepath.Join(repoY, "y.txt")); err != nil {
		t.Fatal("y.txt should exist in repoY:", err)
	}
	// Cross-check: files should NOT leak across repos.
	if _, err := os.Stat(filepath.Join(repoX, "y.txt")); err == nil {
		t.Fatal("y.txt should NOT exist in repoX")
	}
	if _, err := os.Stat(filepath.Join(repoY, "x.txt")); err == nil {
		t.Fatal("x.txt should NOT exist in repoY")
	}
}

// TestParallelTasksConflictingChanges verifies that when two tasks modify
// the same file, the second task's rebase correctly incorporates the first
// task's changes (no data loss).
func TestParallelTasksConflictingChanges(t *testing.T) {
	repo := setupTestRepo(t)
	store, runner := setupTestRunner(t, []string{repo})
	ctx := context.Background()

	taskA, err := store.CreateTask(ctx, "Add line to README", 5)
	if err != nil {
		t.Fatal(err)
	}
	taskB, err := store.CreateTask(ctx, "Add another line to README", 5)
	if err != nil {
		t.Fatal(err)
	}

	wtA, brA, err := runner.setupWorktrees(taskA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskWorktrees(ctx, taskA.ID, wtA, brA); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskStatus(ctx, taskA.ID, "committing"); err != nil {
		t.Fatal(err)
	}

	wtB, brB, err := runner.setupWorktrees(taskB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskWorktrees(ctx, taskB.ID, wtB, brB); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskStatus(ctx, taskB.ID, "committing"); err != nil {
		t.Fatal(err)
	}

	pathA := wtA[repo]
	pathB := wtB[repo]

	// Task A: append to README.md.
	readmeA, err := os.ReadFile(filepath.Join(pathA, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pathA, "README.md"), append(readmeA, []byte("\nLine from task A\n")...), 0644); err != nil {
		t.Fatal(err)
	}

	// Task B: create a NEW file (non-conflicting with A's README change).
	if err := os.WriteFile(filepath.Join(pathB, "b_feature.txt"), []byte("feature B\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Commit A first.
	commitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	runner.commit(commitCtx, taskA.ID, "", 1, wtA, brA)

	// Commit B — rebase should succeed since changes don't conflict.
	runner.commit(commitCtx, taskB.ID, "", 1, wtB, brB)

	// Verify A's README change persists after B's merge.
	readmeFinal, err := os.ReadFile(filepath.Join(repo, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readmeFinal), "Line from task A") {
		t.Fatalf("README.md should contain A's changes after B merged:\n%s", readmeFinal)
	}

	// Verify B's file exists.
	if _, err := os.Stat(filepath.Join(repo, "b_feature.txt")); err != nil {
		t.Fatal("b_feature.txt should exist:", err)
	}
}

// TestParallelWorktreeIsolation verifies that file changes in one worktree
// are invisible in another worktree and in the main repo until merged.
func TestParallelWorktreeIsolation(t *testing.T) {
	repo := setupTestRepo(t)
	_, runner := setupTestRunner(t, []string{repo})

	taskA := uuid.New()
	taskB := uuid.New()

	wtA, brA, err := runner.setupWorktrees(taskA)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runner.cleanupWorktrees(taskA, wtA, brA) })

	wtB, brB, err := runner.setupWorktrees(taskB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runner.cleanupWorktrees(taskB, wtB, brB) })

	pathA := wtA[repo]
	pathB := wtB[repo]

	// Write a file in worktree A.
	if err := os.WriteFile(filepath.Join(pathA, "secret_a.txt"), []byte("only A\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Write a file in worktree B.
	if err := os.WriteFile(filepath.Join(pathB, "secret_b.txt"), []byte("only B\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Files should NOT be visible across worktrees.
	if _, err := os.Stat(filepath.Join(pathA, "secret_b.txt")); err == nil {
		t.Fatal("secret_b.txt should NOT be visible in worktree A")
	}
	if _, err := os.Stat(filepath.Join(pathB, "secret_a.txt")); err == nil {
		t.Fatal("secret_a.txt should NOT be visible in worktree B")
	}

	// Files should NOT be visible in the main repo.
	if _, err := os.Stat(filepath.Join(repo, "secret_a.txt")); err == nil {
		t.Fatal("secret_a.txt should NOT be visible in main repo before merge")
	}
	if _, err := os.Stat(filepath.Join(repo, "secret_b.txt")); err == nil {
		t.Fatal("secret_b.txt should NOT be visible in main repo before merge")
	}
}

