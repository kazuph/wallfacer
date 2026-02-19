package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Runner struct {
	store        *Store
	command      string
	sandboxImage string
	envFile      string
	workspaces   string
}

type RunnerConfig struct {
	Command      string
	SandboxImage string
	EnvFile      string
	Workspaces   string
}

func NewRunner(store *Store, cfg RunnerConfig) *Runner {
	return &Runner{
		store:        store,
		command:      cfg.Command,
		sandboxImage: cfg.SandboxImage,
		envFile:      cfg.EnvFile,
		workspaces:   cfg.Workspaces,
	}
}

type claudeOutput struct {
	Result     string `json:"result"`
	SessionID  string `json:"session_id"`
	StopReason string `json:"stop_reason"`
	IsError    bool   `json:"is_error"`
}

func (r *Runner) Command() string {
	return r.command
}

func (r *Runner) Run(taskID uuid.UUID, prompt, sessionID string) {
	bgCtx := context.Background()
	resumedFromWaiting := sessionID != ""

	task, err := r.store.GetTask(bgCtx, taskID)
	if err != nil {
		log.Printf("runner: get task %s: %v", taskID, err)
		return
	}

	// Apply per-task total timeout across all turns.
	timeout := time.Duration(task.Timeout) * time.Minute
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(bgCtx, timeout)
	defer cancel()

	turns := task.Turns

	for {
		turns++
		log.Printf("runner: task %s turn %d (session=%s, timeout=%s)", taskID, turns, sessionID, timeout)

		output, err := r.runContainer(ctx, taskID, prompt, sessionID)
		if err != nil {
			log.Printf("runner: task %s container error: %v", taskID, err)
			r.store.UpdateTaskStatus(bgCtx, taskID, "failed")
			r.store.UpdateTaskResult(bgCtx, taskID, err.Error(), sessionID, "", turns)
			r.store.InsertEvent(bgCtx, taskID, "error", map[string]string{"error": err.Error()})
			r.store.InsertEvent(bgCtx, taskID, "state_change", map[string]string{
				"from": "in_progress", "to": "failed",
			})
			return
		}

		r.store.InsertEvent(bgCtx, taskID, "output", map[string]string{
			"result":      output.Result,
			"stop_reason": output.StopReason,
			"session_id":  output.SessionID,
		})

		if output.SessionID != "" {
			sessionID = output.SessionID
		}
		r.store.UpdateTaskResult(bgCtx, taskID, output.Result, sessionID, output.StopReason, turns)

		if output.IsError {
			r.store.UpdateTaskStatus(bgCtx, taskID, "failed")
			r.store.InsertEvent(bgCtx, taskID, "state_change", map[string]string{
				"from": "in_progress", "to": "failed",
			})
			return
		}

		switch output.StopReason {
		case "end_turn":
			r.store.UpdateTaskStatus(bgCtx, taskID, "done")
			r.store.InsertEvent(bgCtx, taskID, "state_change", map[string]string{
				"from": "in_progress", "to": "done",
			})

			// Auto commit-and-push after feedback-resumed tasks complete.
			if resumedFromWaiting && sessionID != "" {
				r.commitAndPush(ctx, taskID, sessionID, turns)
			}
			return

		case "max_tokens", "pause_turn":
			log.Printf("runner: task %s auto-continuing (stop_reason=%s)", taskID, output.StopReason)
			prompt = ""
			continue

		default:
			// Empty or unknown stop_reason — waiting for user feedback
			r.store.UpdateTaskStatus(bgCtx, taskID, "waiting")
			r.store.InsertEvent(bgCtx, taskID, "state_change", map[string]string{
				"from": "in_progress", "to": "waiting",
			})
			return
		}
	}
}

// commitAndPush runs an additional container turn to commit and push changes.
func (r *Runner) commitAndPush(ctx context.Context, taskID uuid.UUID, sessionID string, turns int) {
	bgCtx := context.Background()
	log.Printf("runner: task %s auto commit-and-push (session=%s)", taskID, sessionID)

	r.store.InsertEvent(bgCtx, taskID, "output", map[string]string{
		"result": "Auto-running /commit-and-push...",
	})

	output, err := r.runContainer(ctx, taskID, "Run /commit-and-push to commit all changes and push to the remote repository.", sessionID)
	if err != nil {
		log.Printf("runner: task %s commit-and-push error: %v", taskID, err)
		r.store.InsertEvent(bgCtx, taskID, "error", map[string]string{
			"error": "commit-and-push failed: " + err.Error(),
		})
		return
	}

	r.store.InsertEvent(bgCtx, taskID, "output", map[string]string{
		"result":      output.Result,
		"stop_reason": output.StopReason,
		"session_id":  output.SessionID,
	})

	turns++
	sid := sessionID
	if output.SessionID != "" {
		sid = output.SessionID
	}
	r.store.UpdateTaskResult(bgCtx, taskID, output.Result, sid, output.StopReason, turns)
	log.Printf("runner: task %s commit-and-push completed", taskID)
}

func (r *Runner) runContainer(ctx context.Context, taskID uuid.UUID, prompt, sessionID string) (*claudeOutput, error) {
	containerName := "wallfacer-" + taskID.String()

	// Remove any leftover container from a previous interrupted run.
	exec.Command(r.command, "rm", "-f", containerName).Run()

	args := []string{"run", "--rm", "--network=host", "--name", containerName}

	if r.envFile != "" {
		args = append(args, "--env-file", r.envFile)
	}

	// Mount claude config volume.
	args = append(args, "-v", "claude-config:/home/claude/.claude")

	// Mount workspaces.
	if r.workspaces != "" {
		for _, ws := range strings.Fields(r.workspaces) {
			ws = strings.TrimSpace(ws)
			if ws == "" {
				continue
			}
			parts := strings.Split(ws, "/")
			basename := parts[len(parts)-1]
			if basename == "" && len(parts) > 1 {
				basename = parts[len(parts)-2]
			}
			args = append(args, "-v", ws+":/workspace/"+basename+":z")
		}
	}

	args = append(args, "-w", "/workspace", r.sandboxImage)
	args = append(args, "-p", prompt, "--output-format", "json")
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}

	cmd := exec.CommandContext(ctx, r.command, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	log.Printf("runner: exec %s %s", r.command, strings.Join(args, " "))
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("container exited with code %d: stderr=%s stdout=%s", exitErr.ExitCode(), stderr.String(), truncate(stdout.String(), 500))
		}
		return nil, fmt.Errorf("exec container: %w", err)
	}

	var output claudeOutput
	raw := strings.TrimSpace(stdout.String())
	if raw == "" {
		return nil, fmt.Errorf("empty output from container")
	}
	if err := json.Unmarshal([]byte(raw), &output); err != nil {
		return nil, fmt.Errorf("parse output: %w (raw: %s)", err, truncate(raw, 200))
	}

	return &output, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
