package runner

import (
	"strings"
	"sync"
	"time"

	"changkun.de/wallfacer/internal/store"
	"github.com/google/uuid"
)

// ContainerInfo represents a single sandbox container returned by ListContainers.
type ContainerInfo struct {
	ID        string `json:"id"`         // short container ID
	Name      string `json:"name"`       // full container name (e.g. wallfacer-task-<uuid>)
	TaskID    string `json:"task_id"`    // task UUID extracted from name, empty if not a task container
	Image     string `json:"image"`      // image name
	State     string `json:"state"`      // running | exited | paused | ...
	Status    string `json:"status"`     // human-readable status (e.g. "Up 5 minutes")
	CreatedAt int64  `json:"created_at"` // unix timestamp
}

const (
	maxRebaseRetries   = 3
	defaultTaskTimeout = 15 * time.Minute
)

// RunnerConfig holds all configuration needed to construct a Runner.
type RunnerConfig struct {
	Command          string
	EnvFile          string
	Workspaces       string // space-separated workspace paths
	WorktreesDir     string
	InstructionsPath string
}

// Runner orchestrates Claude Code container execution for tasks.
// It manages worktree isolation, container lifecycle, and the commit pipeline.
type Runner struct {
	store            *store.Store
	command          string
	envFile          string
	workspaces       string
	worktreesDir     string
	instructionsPath string
	repoMu           sync.Map // per-repo *sync.Mutex for serializing rebase+merge
}

// NewRunner constructs a Runner from the given store and config.
func NewRunner(s *store.Store, cfg RunnerConfig) *Runner {
	return &Runner{
		store:            s,
		command:          cfg.Command,
		envFile:          cfg.EnvFile,
		workspaces:       cfg.Workspaces,
		worktreesDir:     cfg.WorktreesDir,
		instructionsPath: cfg.InstructionsPath,
	}
}

// Command returns the container runtime binary path (docker).
func (r *Runner) Command() string {
	return r.command
}

// EnvFile returns the path to the env file used for containers.
func (r *Runner) EnvFile() string {
	return r.envFile
}

// Workspaces returns the list of configured workspace paths.
func (r *Runner) Workspaces() []string {
	if r.workspaces == "" {
		return nil
	}
	return strings.Fields(r.workspaces)
}

// repoLock returns a per-repo mutex, creating one on first access.
// Used to serialize rebase+merge operations on the same repository.
func (r *Runner) repoLock(repoPath string) *sync.Mutex {
	v, _ := r.repoMu.LoadOrStore(repoPath, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// KillContainer stops and removes the sandbox for a task.
// Safe to call when no sandbox is running -- errors are silently ignored.
func (r *Runner) KillContainer(taskID uuid.UUID) {
	r.RemoveSandbox(taskID)
}
