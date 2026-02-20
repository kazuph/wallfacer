# Git Operations & Worktree Lifecycle

## Core Principle

Every task gets its own git worktree. Claude Code operates in an isolated copy of each repository on a dedicated branch, leaving the main working tree untouched and allowing multiple tasks to run concurrently without interfering with each other.

```
Main repo (~/projects/myapp)          Task worktree
  branch: main                          branch: task/a1b2c3d4
  working tree: clean                   working tree: mounted into container
                                        path: ~/.wallfacer/worktrees/<uuid>/myapp
```

## Worktree Setup

Called by `setupWorktrees()` in `runner.go` when a task enters `in_progress`.

For each configured workspace:

```
1. git rev-parse --git-dir
       └─ verify the path is a git repository

2. git worktree add -b task/<uuid8> \
       ~/.wallfacer/worktrees/<task-uuid>/<repo-basename>
       └─ creates a new branch and a new working tree simultaneously

3. store worktree path + branch name on the Task struct
```

Branch naming uses the first 8 characters of the task UUID: `task/a1b2c3d4`.

Multiple workspaces → multiple worktrees, all grouped under `~/.wallfacer/worktrees/<task-uuid>/`:

```
~/.wallfacer/worktrees/
└── <task-uuid>/
    ├── myapp/       # worktree for ~/projects/myapp
    └── mylib/       # worktree for ~/projects/mylib
```

## Container Mounts

The sandbox container sees worktrees, not the live main working directory:

```
~/.wallfacer/worktrees/<uuid>/<repo>  →  /workspace/<repo>   (read-write)
~/.wallfacer/.env                      →  /run/secrets/.env   (read-only)
~/.gitconfig                           →  /home/claude/.gitconfig (read-only)
claude-config (named volume)           →  /home/claude/.claude
```

Claude Code operates on `/workspace/<repo>` — the isolated worktree branch — so all edits land on `task/<uuid8>` and never touch `main`.

## Commit Pipeline

Triggered automatically after `end_turn`, or manually when a user marks a `waiting` task as done. Runs four sequential phases in `runner.go`.

### Phase 1 — Claude Commits (in container)

A new container run is launched with a commit prompt. Claude executes:
```
git add -A
git commit -m "<meaningful message>"
```
in each worktree. This happens inside the sandbox with the same user identity as the main run.

### Phase 2 — Rebase & Merge (host-side, `git.go`)

```
git rebase <default-branch>
  └─ rebases task branch on top of the current default branch HEAD
  └─ on conflict: retry up to 3 times, invoking Claude's conflict resolver each time

git merge --ff-only <task-branch>
  └─ fast-forward merges the rebased task branch into the default branch
  └─ collect resulting commit hashes
```

`defaultBranch()` resolves the target branch by checking, in order:
1. `origin/HEAD` (remote default)
2. Current `HEAD` branch name
3. Falls back to `"main"`

**Conflict resolution loop:** If `git rebase` exits non-zero, Wallfacer invokes Claude Code again — using the original task's session ID — passing it the conflict details. Claude resolves the conflicts and stages the result. The rebase is then continued and retried. Up to 3 attempts are made before the task is marked `failed`.

### Phase 3 — PROGRESS.md (host-side)

In each workspace's main working tree, Wallfacer appends a record to `PROGRESS.md`:

```markdown
## <task-uuid> — <timestamp>

**Branch:** task/a1b2c3d4
**Commit:** abc123ef
**Prompt:** <original task prompt>
**Result:** <Claude's final output summary>
```

The file is then auto-committed directly on the default branch (not via worktree).

### Phase 4 — Cleanup

```
git worktree remove --force   ← remove worktree directory
git branch -D task/<uuid8>    ← delete task branch
rm -rf data/<uuid>            ← remove task output files
```

Cleanup is idempotent and safe to call multiple times (errors are logged, not fatal).

## Orphan Pruning

`pruneOrphanedWorktrees()` runs on every server startup:

1. Scan `~/.wallfacer/worktrees/` for subdirectories
2. For each directory, check if a task with matching UUID exists in the store
3. If no matching task found: remove the directory + run `git worktree prune` on all workspaces to clear stale refs from `.git/worktrees/`

This handles crashes where cleanup never ran.

## Git Helper Functions (`git.go`)

| Function | Purpose |
|---|---|
| `isGitRepo(path)` | Check if a directory is inside a git repo |
| `defaultBranch(repoPath)` | Detect the default branch name |
| `createWorktree(repo, branch, path)` | `git worktree add -b <branch> <path>` |
| `removeWorktree(repo, path)` | `git worktree remove --force <path>` |
| `rebaseOntoDefault(worktree)` | Rebase task branch onto default branch |
| `ffMerge(repo, branch)` | Fast-forward merge into default branch |
| `hasCommitsAheadOf(worktree, base)` | Check whether the worktree has unpushed commits |
| `getCommitHash(path)` | Get current HEAD SHA in a worktree or repo |

## Git Status API

The server exposes git status for the UI's header bar:

- `GET /api/git/status` — returns current branch, remote tracking info, and ahead/behind counts for each workspace
- `GET /api/git/stream` — SSE endpoint that polls git status every few seconds and pushes updates
- `POST /api/git/push` — runs `git push` on a specified workspace (e.g. after reviewing merged commits)
