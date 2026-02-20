# Wallfacer Architecture

Wallfacer is a Kanban task runner that executes Claude Code in isolated sandbox containers. Users create tasks on a web board; dragging a card from Backlog to In Progress triggers autonomous AI execution, git isolation, and auto-merge back to the main branch.

## Overview

```
┌─────────────────────────────────────────────────────────────┐
│  Browser (Vanilla JS + Tailwind + Sortable.js)              │
│  5-column Kanban board — SSE for live updates               │
└────────────────────────┬────────────────────────────────────┘
                         │ HTTP / SSE
┌────────────────────────▼────────────────────────────────────┐
│  Go Server (native on host)                                 │
│  main.go · handler.go · runner.go · store.go · git.go      │
└──────┬──────────────────────────────────────┬───────────────┘
       │ os/exec (podman/docker)              │ git commands
┌──────▼──────────────┐              ┌────────▼──────────────┐
│  Sandbox Container  │              │  Git Worktrees        │
│  Ubuntu 24.04       │              │  ~/.wallfacer/        │
│  Claude Code CLI    │◄────mount────│  worktrees/<uuid>/    │
└─────────────────────┘              └───────────────────────┘
```

## Technology Stack

**Backend**
- Go 1.25, `stdlib net/http` (no framework)
- `os/exec` for container orchestration
- `sync.RWMutex` for in-memory store concurrency
- `github.com/google/uuid` for task IDs

**Frontend**
- Vanilla JavaScript (no framework)
- Tailwind CSS, Sortable.js, Marked.js
- `EventSource` (SSE) for live updates
- `localStorage` for theme preferences

**Infrastructure**
- Podman or Docker (container runtime)
- Ubuntu 24.04 sandbox image with Claude Code CLI installed
- Git worktrees for per-task isolation

**Persistence**
- Filesystem only — no database
- `~/.wallfacer/data/<uuid>/` per task
- Atomic writes via temp file + `os.Rename`

## Project Structure

```
wallfacer/
├── main.go              # CLI dispatch, HTTP routing, server init, browser launch
├── handler.go           # HTTP API handlers (CRUD, feedback, git, SSE)
├── runner.go            # Container orchestration, task execution loop, commit pipeline
├── store.go             # In-memory task store, event sourcing, atomic file I/O
├── git.go               # Git worktree operations, branch detection, rebase/merge
├── logger.go            # Structured logging (pretty-print + JSON)
│
├── ui/
│   ├── index.html       # 5-column Kanban board layout
│   └── js/
│       ├── state.js     # Global state management
│       ├── api.js       # HTTP client & SSE stream setup
│       ├── tasks.js     # Task CRUD operations
│       ├── render.js    # Board rendering & DOM updates
│       ├── modal.js     # Task detail modal
│       ├── git.js       # Git status display
│       ├── dnd.js       # Drag-and-drop (Sortable.js)
│       └── events.js    # Event timeline rendering
│
├── sandbox/
│   ├── Dockerfile       # Ubuntu 24.04 + Go + Node + Python + Claude Code
│   └── entrypoint.sh    # Git config setup, Claude Code launcher
│
├── Makefile             # build, server, run, shell, clean targets
├── go.mod, go.sum
└── docs/                # This documentation
```

## Key Design Choices

| Choice | Rationale |
|---|---|
| Git worktrees per task | Full isolation; concurrent tasks don't interfere; Claude sees a clean branch |
| Goroutines, no queue | Simplicity; Go's scheduler handles parallelism; tasks are long-running and IO-bound |
| Filesystem persistence, no DB | Zero dependencies; atomic rename is crash-safe; human-readable for debugging |
| SSE, not WebSocket | Simpler server-side; one-directional push is all the UI needs |
| Ephemeral containers | No state leaks between tasks; each run starts clean |
| Event sourcing (traces/) | Full audit trail; enables crash recovery and replay |

## Server Initialization

`main.go` → `runServer`:

```
parse CLI flags / env vars
→ load tasks from data/<uuid>/task.json into memory
→ create worktreesDir (~/.wallfacer/worktrees/)
→ pruneOrphanedWorktrees()   (removes stale worktree dirs + runs `git worktree prune`)
→ recover crashed tasks      (in_progress / committing → failed)
→ register HTTP routes
→ start listener on :8080
→ open browser (unless -no-browser)
```

## Configuration

CLI flags (all have env var fallbacks):

| Flag | Env var | Default |
|---|---|---|
| `-addr` | `ADDR` | `:8080` |
| `-data` | `DATA_DIR` | `~/.wallfacer/data` |
| `-container` | `CONTAINER_CMD` | `/opt/podman/bin/podman` |
| `-image` | `SANDBOX_IMAGE` | `wallfacer:latest` |
| `-env-file` | `ENV_FILE` | `~/.wallfacer/.env` |
| `-no-browser` | — | `false` |

`~/.wallfacer/.env` must contain `CLAUDE_CODE_OAUTH_TOKEN`.

CLI subcommands:
- `wallfacer run [flags] [workspace ...]` — start the server
- `wallfacer env` — check config and token status
