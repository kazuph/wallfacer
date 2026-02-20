# Orchestration Flows

## HTTP API

All state changes flow through `handler.go`. The handler never blocks — long-running work is always handed off to a goroutine.

### Routes

| Method + Path | Handler action |
|---|---|
| `GET /api/config` | Return workspace paths |
| `GET /api/tasks` | List all tasks (from in-memory store) |
| `POST /api/tasks` | Create task, assign UUID, persist to disk |
| `PATCH /api/tasks/{id}` | Update status / position / prompt / timeout — may launch `runner.Run` goroutine |
| `DELETE /api/tasks/{id}` | Delete task + cleanup worktrees |
| `POST /api/tasks/{id}/feedback` | Write feedback event → launch `runner.Run` (resume) goroutine |
| `POST /api/tasks/{id}/done` | Set `committing` → launch commit pipeline goroutine |
| `POST /api/tasks/{id}/cancel` | Kill container (if running), clean up worktrees, set `cancelled`; traces/logs kept |
| `POST /api/tasks/{id}/resume` | Resume failed task, same session → launch `runner.Run` goroutine |
| `POST /api/tasks/{id}/archive` | Move done task to archived |
| `POST /api/tasks/{id}/unarchive` | Restore archived task |
| `GET /api/tasks/stream` | SSE: push task list on any state change |
| `GET /api/tasks/{id}/events` | Return full event trace log |
| `GET /api/tasks/{id}/outputs/{filename}` | Serve raw turn output file |
| `GET /api/tasks/{id}/logs` | SSE: stream live `podman logs -f` output |
| `GET /api/git/status` | Current branch / remote status for all workspaces |
| `GET /api/git/stream` | SSE: poll git status every few seconds |
| `POST /api/git/push` | Run `git push` on a workspace |

### Triggering Task Execution

When a `PATCH /api/tasks/{id}` request moves a task to `in_progress`, the handler:

1. Updates the task record (status, session ID)
2. Launches a background goroutine: `go h.runner.Run(id, prompt, sessionID, false)`
3. Returns `200 OK` immediately — the client does not wait for execution

The same pattern applies to feedback resumption and commit-and-push.

## Background Goroutine Model

No message queue, no worker pool. Concurrency is plain Go goroutines:

```go
// Task execution (new or resumed)
go h.runner.Run(id, prompt, sessionID, freshStart)

// Post-feedback resumption
go h.runner.Run(id, feedbackMessage, sessionID, false)

// Commit pipeline after mark-done
go func() {
    h.runner.Commit(id)
    store.UpdateStatus(id, "done")
}()
```

Tasks are long-running and IO-bound (container execution, git operations), so goroutines are appropriate — no CPU contention, and Go's scheduler handles the rest.

## Container Execution (`runner.go` `runContainer`)

Each turn launches an ephemeral container:

```
podman run --rm \
  --name wallfacer-<uuid> \
  --env-file ~/.wallfacer/.env \
  -v claude-config:/home/claude/.claude \
  -v <worktree-path>:/workspace/<repo-name> \
  -v ~/.gitconfig:/home/claude/.gitconfig:ro \
  wallfacer:latest \
  claude -p "<prompt>" \
         --resume <session-id> \
         --verbose \
         --output-format stream-json
```

- `--rm` — container is destroyed on exit; no state leaks between tasks
- `--resume` — omitted on the first turn or when `FreshStart` is set
- Output is captured as NDJSON, parsed, and saved to disk
- Stderr is saved separately if non-empty

The container name `wallfacer-<uuid>` lets the server stream logs with `podman logs -f wallfacer-<uuid>` while the container is running.

## SSE Live Update Flow

Both task state and git status use the same SSE push pattern:

```
UI opens EventSource → GET /api/tasks/stream
  handler registers subscriber channel
  ↓
any store.Write() call → notify() sends signal (non-blocking, coalesced)
  ↓
handler wakes, serialises full task list as JSON
  sends: data: <json>\n\n
  ↓
UI receives event → re-renders board
```

`notify()` uses a buffered channel of size 1. If a signal is already pending (UI hasn't drained yet), the new signal is dropped — the subscriber will still get the latest state on the next drain. This coalesces bursts of rapid state changes into a single UI update.

The same pattern applies to `GET /api/git/stream`, except the source is a time-based ticker (polling `git status` every few seconds) rather than a store write signal.

Live container logs use a different mechanism: `GET /api/tasks/{id}/logs` opens a process pipe to `podman logs -f <name>` and streams its stdout line-by-line as SSE events.

## Store Concurrency

`store.go` manages an in-memory `map[string]*Task` behind a `sync.RWMutex`:

- Reads (`List`, `Get`) acquire a read lock
- Writes (`Create`, `Update`, `UpdateStatus`) acquire a write lock, mutate memory, then atomically persist to disk (temp file + `os.Rename`)
- After every write, `notify()` is called to wake SSE subscribers

Event traces are append-only. Each event is written as a separate file (`traces/NNNN.json`) using the same atomic write pattern. Files are never modified after creation.

## Token Tracking & Cost

Per-turn usage is extracted from the Claude Code JSON output and accumulated on the `Task`:

```
TaskUsage {
  InputTokens              int
  OutputTokens             int
  CacheReadInputTokens     int
  CacheCreationInputTokens int
  CostUSD                  float64
}
```

Usage is displayed on task cards and aggregated in the Done column header. It persists in `task.json` across server restarts.

## Multi-Workspace Support

Multiple workspace paths can be passed at startup (see [Architecture — Configuration](architecture.md#configuration)). For each workspace:

- Git status is polled independently and shown in the UI header
- A separate worktree is created per task per workspace
- The commit pipeline runs phases 1–3 for each workspace in sequence
- `PROGRESS.md` is maintained per workspace

Non-git directories are supported as plain mount targets (no worktree, no commit pipeline for that workspace).

## Conflict Resolution Flow

When `git rebase` fails during the commit pipeline:

```
rebase fails with conflict
  ↓
wallfacer invokes Claude Code (same session ID) with conflict details
  ↓
Claude resolves conflicts, stages files
  ↓
wallfacer runs `git rebase --continue`
  ↓
if still failing: repeat up to 3 times
  ↓
if all retries exhausted: mark task failed, clean up worktrees
```

Using the same session ID means Claude has full context of the original task when making conflict resolution decisions.
