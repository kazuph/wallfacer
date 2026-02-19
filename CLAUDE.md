# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Wallfacer is a Kanban task runner for Claude Code. It provides a web UI where tasks are created as cards, dragged to "In Progress" to trigger Claude Code execution in an isolated sandbox container, and results are inspected when done.

**Architecture:** Browser → Go server (:8080) → per-task directory storage (`data/<uuid>/`). The server runs natively on the host and launches ephemeral sandbox containers via `os/exec` (podman/docker).

## Build & Run Commands

```bash
make build          # Build the wallfacer sandbox image
make server         # Build and run the Go server natively
make shell          # Open bash shell in sandbox container for debugging
make clean          # Remove the sandbox image
make run PROMPT="…" # Headless one-shot Claude execution with a prompt
```

CLI usage (after `go build -o wallfacer .`):

```bash
wallfacer ~/project1 ~/project2   # Mount workspaces, open browser
wallfacer                          # Defaults to current directory
wallfacer -addr :9090 -no-browser  # Custom port, no browser
```

The Makefile uses Podman (`/opt/podman/bin/podman`) by default. Adjust `PODMAN` variable if using Docker.

## Server Development

The Go source lives at the top level. Module path: `changkun.de/wallfacer`. Go version: 1.25.7.

```bash
go build -o wallfacer .   # Build server binary
go vet ./...              # Lint
```

There are no tests currently. The server uses `net/http` stdlib routing (Go 1.22+ pattern syntax) with no framework.

Key server files:
- `main.go` — CLI flags, workspace resolution, store init, HTTP routing, browser launch, server startup
- `handler.go` — API handlers: tasks CRUD, feedback, resume, complete, event retrieval, output serving
- `runner.go` — Container orchestration via `os/exec`; creates/runs/parses sandbox output; persists raw output per turn; usage tracking
- `store.go` — Per-task directory persistence (`data/<uuid>/task.json` + `traces/` + `outputs/`), data models (Task, TaskUsage, TaskEvent)
- `ui/index.html` — Kanban board UI (vanilla JS + Tailwind CSS CDN + Sortable.js)

## API Routes

- `GET /` — Kanban UI (embedded UI files)
- `GET /api/tasks` — List all tasks
- `POST /api/tasks` — Create task (JSON: `{prompt, timeout}`)
- `PATCH /api/tasks/{id}` — Update status/position/prompt/timeout
- `DELETE /api/tasks/{id}` — Delete task
- `POST /api/tasks/{id}/feedback` — Submit feedback for waiting tasks
- `POST /api/tasks/{id}/done` — Mark waiting task as done (triggers commit-and-push)
- `POST /api/tasks/{id}/resume` — Resume failed task with existing session
- `GET /api/tasks/{id}/events` — Task event timeline
- `GET /api/tasks/{id}/outputs/{filename}` — Raw Claude Code output per turn
- `GET /api/tasks/{id}/logs` — Stream live container logs (SSE-style)

## Task Lifecycle

States: `backlog` → `in_progress` → `done` | `waiting` | `failed`

- Drag Backlog → In Progress triggers `runner.Run()` in a background goroutine
- Claude `end_turn` → Done; empty stop_reason → Waiting (needs user feedback)
- `max_tokens`/`pause_turn` → auto-continue in same session
- Feedback on Waiting card resumes execution
- "Mark as Done" on Waiting card → Done + auto commit-and-push
- "Resume" on Failed card (with session) → resumes in existing session
- "Retry" on Failed/Done card → resets to Backlog with fresh session

## Data Directory Structure

```
data/<uuid>/
├── task.json                    # Task metadata (prompt, status, usage, etc.)
├── traces/                      # Event sourcing
│   ├── 0001.json                # state_change, output, feedback, error events
│   └── ...
└── outputs/                     # Raw Claude Code output per turn
    ├── turn-0001.json           # Full JSON stdout from Claude Code
    ├── turn-0001.stderr.txt     # Stderr (only if non-empty)
    └── ...
```

## Key Conventions

- **UUIDs** for all task IDs (auto-generated via `github.com/google/uuid`)
- **Event sourcing** via per-task trace files; types: `state_change`, `output`, `feedback`, `error`
- **Per-task directory storage** with atomic writes (temp file + rename); `sync.RWMutex` for concurrency
- **Raw output persistence** saves full Claude Code JSON stdout and stderr per turn to `outputs/`
- **Usage tracking** accumulates input/output tokens, cache tokens, and cost across turns
- **Container execution** creates sibling containers via `os/exec`; mounts host workspaces under `/workspace/<basename>`
- **Frontend** polls every 2 seconds; uses optimistic UI updates; escapes HTML to prevent XSS
- **No framework** on backend (stdlib `net/http`) or frontend (vanilla JS)

## CLI Flags & Environment

CLI flags (with env var fallbacks):

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `-addr` | `ADDR` | `:8080` | Listen address |
| `-data` | `DATA_DIR` | `data` | Data directory |
| `-container` | `CONTAINER_CMD` | `/opt/podman/bin/podman` | Container runtime command |
| `-image` | `SANDBOX_IMAGE` | `wallfacer:latest` | Sandbox container image |
| `-env` | `ENV_FILE` | `.env` | Env file for container (Claude token) |
| `-no-browser` | — | `false` | Do not open browser on start |

Positional arguments are workspace directories to mount (defaults to cwd).

Sandbox env (in `.env`): `CLAUDE_CODE_OAUTH_TOKEN` (required)
