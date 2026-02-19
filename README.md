# Wallfacer

A Kanban task runner for Claude Code. Create tasks as cards in a web UI, drag them to In Progress to trigger Claude Code execution in a sandbox, and inspect results when done.

## Architecture

```
Browser (Kanban UI)
    │
    ↓
Go Server (:8080)  ──per-task dirs──→  data/<uuid>/
    │
    ↓ (os/exec → podman run)
Sandbox Container (ephemeral) → Claude Code CLI
```

The Go server runs natively on the host and persists tasks to per-task directories. It launches ephemeral sandbox containers via `podman run` (or `docker run`).

## Setup

```bash
# 1. Get an OAuth token (needs a browser)
claude setup-token

# 2. Configure
cp .env.example .env
# Edit .env and paste your token

# 3. Build sandbox image
make build

# 4. Start the server with workspaces
wallfacer ~/projects/myapp ~/projects/lib
```

The browser opens automatically to http://localhost:8080.

## Usage

```bash
# Mount specific workspace directories
wallfacer ~/project1 ~/project2

# Defaults to current directory if no args given
wallfacer

# Custom port, skip browser
wallfacer -addr :9090 -no-browser ~/myapp

# All flags
wallfacer -h
```

## Task Lifecycle

```
BACKLOG ──drag──→ IN_PROGRESS ──auto──→ DONE
                      │
                      ├──auto──→ WAITING ──feedback──→ IN_PROGRESS
                      │                  ──mark done──→ DONE (+ commit-and-push)
                      │
                      └──auto──→ FAILED ──resume──→ IN_PROGRESS (same session)
                                        ──retry───→ BACKLOG (fresh session)
```

- Drag a card from Backlog to In Progress to start execution
- Claude finishes (`end_turn`) → card moves to Done
- Claude asks a question (empty stop_reason) → card moves to Waiting
- Submit feedback on a Waiting card → resumes execution
- Mark a Waiting card as Done → moves to Done and auto-runs commit-and-push
- Resume a Failed card → continues in the same Claude session
- Retry a Failed/Done card → resets to Backlog with a fresh session
- `max_tokens` / `pause_turn` → auto-continues in the background
- Token usage (input, output, cache, cost) is tracked and displayed per task

## Make Targets

| Target | Description |
|---|---|
| `make build` | Build the sandbox image |
| `make server` | Build and run the Go server natively |
| `make run PROMPT="…"` | Headless one-shot Claude execution |
| `make shell` | Open a bash shell in a sandbox container |
| `make clean` | Remove the sandbox image |

## Project Structure

```
.
├── Makefile              # Top-level convenience targets
├── main.go               # CLI flags, workspace resolution, HTTP routes, browser launch
├── handler.go            # API handlers: tasks CRUD, feedback, resume, complete, outputs
├── runner.go             # Container orchestration, raw output persistence, usage tracking
├── store.go              # Per-task directory persistence, data models
├── ui/
│   └── index.html        # Kanban board UI (vanilla JS + Tailwind + Sortable.js)
├── go.mod
├── go.sum
├── sandbox/
│   ├── Dockerfile        # Ubuntu 24.04 + Go + Node + Python + Claude Code
│   ├── entrypoint.sh     # Git safe.directory fix, launches Claude
│   └── .dockerignore
└── data/                 # Per-task storage (auto-created)
    └── <uuid>/
        ├── task.json     # Task metadata
        ├── traces/       # Event log
        └── outputs/      # Raw Claude Code output per turn
```

## Configuration

Set in `.env` (passed to sandbox containers):

| Variable | Description |
|---|---|
| `CLAUDE_CODE_OAUTH_TOKEN` | OAuth token from `claude setup-token` |

CLI flags (all have env var fallbacks):

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `-addr` | `ADDR` | `:8080` | Listen address |
| `-data` | `DATA_DIR` | `data` | Data directory |
| `-container` | `CONTAINER_CMD` | `/opt/podman/bin/podman` | Container runtime |
| `-image` | `SANDBOX_IMAGE` | `wallfacer:latest` | Sandbox image |
| `-env` | `ENV_FILE` | `.env` | Env file for container |
| `-no-browser` | — | `false` | Don't open browser |

Positional arguments are workspace directories to mount (defaults to current directory).

## Requirements

- [Go](https://go.dev/) 1.25+
- [Podman](https://podman.io/) (or Docker)
- Claude Pro or Max subscription (for OAuth token)

## License

See [LICENSE](LICENSE).
