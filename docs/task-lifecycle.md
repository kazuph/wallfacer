# Task Lifecycle

## State Machine

Tasks progress through a well-defined set of states. Every transition is recorded as an immutable event in `data/<uuid>/traces/`.

```
BACKLOG ──drag──→ IN_PROGRESS ──end_turn──────────────────→ DONE
   │                  │                                        │
   │                  ├──max_tokens / pause_turn──→ (loop)     └──drag──→ ARCHIVED
   │                  │
   │                  ├──empty stop_reason──→ WAITING ──feedback──→ IN_PROGRESS
   │                  │                              ──mark done──→ COMMITTING → DONE
   │                  │                              ──cancel────→ CANCELLED
   │                  │
   │                  └──is_error / timeout──→ FAILED ──resume──→ IN_PROGRESS (same session)
   │                                                  ──retry───→ BACKLOG (fresh session)
   │                                                  ──cancel──→ CANCELLED
   │
   └──cancel──→ CANCELLED ──retry──→ BACKLOG
```

## States

| State | Description |
|---|---|
| `backlog` | Queued, not yet started |
| `in_progress` | Container running, Claude Code executing |
| `waiting` | Claude paused mid-task, awaiting user feedback |
| `committing` | Transient: commit pipeline running after mark-done |
| `done` | Completed; changes committed and merged |
| `failed` | Container error, Claude error, or timeout |
| `cancelled` | Explicitly cancelled; sandbox cleaned up, history preserved |
| `archived` | Done task moved off the active board |

## Turn Loop

Each pass through the loop in `runner.go` `Run()`:

1. Increment turn counter
2. Run container with current prompt and session ID
3. Save raw stdout to `data/<uuid>/outputs/turn-NNNN.json`; stderr (if any) to `turn-NNNN.stderr.txt`
4. Parse `stop_reason` from Claude Code JSON output:

| `stop_reason` | `is_error` | Result |
|---|---|---|
| `end_turn` | false | Exit loop → trigger commit pipeline → `done` |
| `max_tokens` | false | Auto-continue (next iteration, same session) |
| `pause_turn` | false | Auto-continue (next iteration, same session) |
| empty / unknown | false | Set `waiting`; block until user provides feedback |
| any | true | Set `failed` |

5. Accumulate token usage (`input_tokens`, `output_tokens`, cache tokens, `cost_usd`)

## Session Continuity

Claude Code supports `--resume <session-id>`. The first turn creates a new session; subsequent turns (auto-continue or post-feedback) pass the same session ID, preserving the full conversation context.

Setting `FreshStart = true` on a task skips `--resume`, starting a brand-new session. This is what happens when a user retries a failed task.

## Feedback & Waiting State

When `stop_reason` is empty, Claude has asked a question or is blocked. The task enters `waiting`:

- Worktrees are **not** cleaned up — the git branch is preserved
- User submits feedback via `POST /api/tasks/{id}/feedback`
- Handler writes a `feedback` event to the trace log, then launches a new `runner.Run` goroutine using the existing session ID
- The task resumes from exactly where it paused, with the feedback message as the next prompt

Alternatively, the user can mark the task done from `waiting`, which skips further Claude turns and jumps straight to the commit pipeline.

## Cancellation

Any task in `backlog`, `in_progress`, `waiting`, or `failed` can be cancelled via `POST /api/tasks/{id}/cancel`. The handler:

1. **Kills the container** (if `in_progress`) — sends `podman kill wallfacer-<uuid>`. The running goroutine detects the cancelled status and exits without overwriting it to `failed`.
2. **Cleans up worktrees** — removes the git worktree and deletes the task branch, discarding all prepared changes.
3. **Sets status to `cancelled`** and appends a `state_change` event.
4. **Preserves history** — `data/<uuid>/traces/` and `data/<uuid>/outputs/` are left intact so execution logs, token usage, and the event timeline remain visible.

From `cancelled`, the user can retry the task (moves it back to `backlog`) to restart from scratch.

## Data Models

Defined in `store.go`:

**Task**
```
ID          string       // UUID
Prompt      string       // original task description
Status      string       // current state
SessionID   string       // Claude Code session ID (persisted across turns)
StopReason  string       // last stop_reason from Claude
Turns       int          // number of completed turns
Timeout     int          // per-turn timeout in seconds
Usage       TaskUsage    // accumulated token counts and cost
Worktrees   []Worktree   // per-repo worktree paths and branch names
CommitHash  []string     // commit hashes after merge
```

**TaskEvent** (append-only trace log)
```
Type      string    // state_change | output | feedback | error
Timestamp time.Time
Payload   any       // type-specific data
```

**TaskUsage**
```
InputTokens              int
OutputTokens             int
CacheReadInputTokens     int
CacheCreationInputTokens int
CostUSD                  float64
```

## Persistence

Each task owns a directory under `data/<uuid>/`:

```
data/<uuid>/
├── task.json          # current task state (atomically overwritten on each update)
├── traces/
│   ├── 0001.json      # first event
│   ├── 0002.json      # second event
│   └── ...            # append-only
└── outputs/
    ├── turn-0001.json        # raw Claude Code JSON output
    ├── turn-0001.stderr.txt  # stderr (if non-empty)
    └── ...
```

All writes are atomic (temp file + `os.Rename`). On startup, `task.json` files are loaded into memory. See [Architecture](architecture.md#design-choices) for the persistence design rationale.

## Crash Recovery

On startup, any task whose status is `in_progress` or `committing` is treated as a crash victim:

1. Worktrees are cleaned up (`git worktree remove --force`, `git branch -D`)
2. Status set to `failed`
3. An `error` event and a `state_change` event are appended to the trace log
4. The user can then resume (same session) or retry (fresh session) from the UI
