# Wallfacer

A Kanban task runner for [Claude Code](https://claude.ai/code). Create tasks as cards in a web UI, drag them to "In Progress" to trigger Claude Code execution in a sandbox container, and inspect results when done.

<table>
  <tr>
    <td><img src="./images/overview.png" alt="Overview" width="100%"></td>
    <td><img src="./images/inprogress.png" alt="In Progress" width="100%"></td>
    <td><img src="./images/waiting.png" alt="Waiting" width="100%"></td>
  </tr>
</table>

## Prerequisites

- [Go](https://go.dev/) 1.25+
- [Podman](https://podman.io/) or [Docker](https://www.docker.com/)
- Claude Pro or Max subscription (for the OAuth token)

## Quick Start

**1. Get an OAuth token**

```bash
claude setup-token
```

**2. Configure the environment file**

```bash
mkdir -p ~/.wallfacer
cp .env.example ~/.wallfacer/.env
# Edit ~/.wallfacer/.env and paste your token
```

**3. Build the sandbox image**

```bash
make build
```

**4. Build and start the server**

```bash
go build -o wallfacer .
wallfacer run ~/projects/myapp ~/projects/mylib
```

The browser opens automatically to http://localhost:8080.

## Usage

```bash
# Mount specific workspace directories
wallfacer run ~/project1 ~/project2

# Defaults to current directory if no args given
wallfacer run

# Custom port, skip auto-opening the browser
wallfacer run -addr :9090 -no-browser ~/myapp

# Show configuration and env file status
wallfacer env

# All flags
wallfacer run -help
```

### How It Works

1. Create a task card in the Backlog column with a prompt for Claude
2. Drag it to In Progress — Wallfacer launches a sandbox container and runs Claude Code
3. When Claude finishes, the card moves to Done and changes are committed to your repo
4. If Claude asks a question, the card moves to Waiting — submit feedback to continue

See [Task Lifecycle](docs/task-lifecycle.md) for details on all states and transitions.

### Make Targets

| Target | Description |
|---|---|
| `make build` | Build the sandbox image |
| `make server` | Build and run the Go server |
| `make run PROMPT="..."` | Headless one-shot Claude execution |
| `make shell` | Debug shell inside a sandbox container |
| `make clean` | Remove the sandbox image |

## Documentation

- [Architecture](docs/architecture.md) — system overview, tech stack, project structure, configuration
- [Task Lifecycle](docs/task-lifecycle.md) — states, turn loop, feedback, data models, persistence
- [Git Worktrees](docs/git-worktrees.md) — per-task isolation, commit pipeline, conflict resolution
- [Orchestration](docs/orchestration.md) — API routes, container execution, SSE, concurrency

## Origin Story

Wallfacer was built in about a week of spare time. The idea came from using Claude Code for everyday coding tasks. After a while, the workflow settled into writing task descriptions, running Claude, reviewing the output, and repeating. The main bottleneck was watching Claude Code's execution and managing all these tasks, so a Kanban board felt like a natural fit for managing that loop.

The first version was a Go server with a simple web UI. Tasks go into a backlog, get dragged to "in progress" to run Claude Code in a container, and move to "done" when finished. Git worktrees keep each task isolated so multiple can run at the same time without stepping on each other.

At some point Wallfacer was stable enough to develop itself and can create a task card like "add retry logic," drag it to in progress, and let Claude implement the feature inside a Wallfacer sandbox. Most of the later features were built this way.

## License

See [LICENSE](LICENSE).
