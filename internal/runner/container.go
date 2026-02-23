package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"changkun.de/wallfacer/internal/envconfig"
	"changkun.de/wallfacer/internal/logger"
	"github.com/google/uuid"
)

// claudeUsage mirrors the token-usage object in Claude Code's JSON output.
type claudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// claudeOutput is the top-level result object emitted by Claude Code
// (either as a single JSON blob or as the last line of NDJSON stream-json).
type claudeOutput struct {
	Result       string      `json:"result"`
	SessionID    string      `json:"session_id"`
	StopReason   string      `json:"stop_reason"`
	Subtype      string      `json:"subtype"`
	IsError      bool        `json:"is_error"`
	TotalCostUSD float64     `json:"total_cost_usd"`
	Usage        claudeUsage `json:"usage"`
}

// sandboxName returns the Docker sandbox name for a task.
// Uses a short prefix to stay under UNIX socket path length limits.
func sandboxName(taskID uuid.UUID) string {
	return "wf-" + taskID.String()[:8]
}

// CreateSandbox creates a new Docker sandbox for a task.
// Any existing sandbox with the same name is removed first.
// Retries up to 3 times with backoff when Docker sandbox API returns transient errors.
func (r *Runner) CreateSandbox(ctx context.Context, taskID uuid.UUID, workspacePaths []string) error {
	name := sandboxName(taskID)
	// Remove any leftover sandbox from a previous interrupted run.
	exec.Command(r.command, "sandbox", "stop", name).Run()
	exec.Command(r.command, "sandbox", "rm", name).Run()

	args := []string{"sandbox", "create", "--name", name, "claude"}
	args = append(args, workspacePaths...)

	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			wait := time.Duration(attempt) * time.Second
			logger.Runner.Info("retrying sandbox create", "name", name, "attempt", attempt, "wait", wait)
			time.Sleep(wait)
		}

		cmd := exec.CommandContext(ctx, r.command, args...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			logger.Runner.Info("sandbox created", "name", name, "workspaces", workspacePaths)
			return nil
		}
		lastErr = fmt.Errorf("create sandbox %s: %w (output: %s)", name, err, strings.TrimSpace(string(out)))
		logger.Runner.Warn("sandbox create attempt failed", "name", name, "attempt", attempt, "error", lastErr)
	}
	return lastErr
}

// StopSandbox stops a sandbox without removing it (preserves session).
func (r *Runner) StopSandbox(taskID uuid.UUID) {
	name := sandboxName(taskID)
	exec.Command(r.command, "sandbox", "stop", name).Run()
}

// RemoveSandbox removes a sandbox and all its resources.
func (r *Runner) RemoveSandbox(taskID uuid.UUID) {
	name := sandboxName(taskID)
	exec.Command(r.command, "sandbox", "stop", name).Run()
	exec.Command(r.command, "sandbox", "rm", name).Run()
}

// execInSandbox runs Claude Code in an existing sandbox and parses its NDJSON output.
// The workdir parameter, when non-empty, sets the working directory inside the sandbox.
func (r *Runner) execInSandbox(
	ctx context.Context,
	taskID uuid.UUID,
	prompt, sessionID, workdir string,
) (*claudeOutput, []byte, []byte, error) {
	name := sandboxName(taskID)

	args := []string{"sandbox", "exec"}
	if r.envFile != "" {
		args = append(args, "--env-file", r.envFile)
	}
	if workdir != "" {
		args = append(args, "-w", workdir)
	}
	args = append(args, name, "claude", "-p", prompt, "--verbose", "--output-format", "stream-json", "--dangerously-skip-permissions")
	if model := r.modelFromEnv(); model != "" {
		args = append(args, "--model", model)
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}

	cmd := exec.CommandContext(ctx, r.command, args...)
	var stdout, stderr bytes.Buffer

	// Write stdout to both the buffer and a live.log file for real-time streaming.
	liveLogPath := r.store.LiveLogPath(taskID)
	if dir := filepath.Dir(liveLogPath); dir != "" {
		os.MkdirAll(dir, 0700)
	}
	liveLog, liveErr := os.Create(liveLogPath)
	if liveErr == nil {
		cmd.Stdout = io.MultiWriter(&stdout, liveLog)
		cmd.Stderr = io.MultiWriter(&stderr, liveLog)
		defer liveLog.Close()
	} else {
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
	}

	logger.Runner.Debug("exec sandbox", "cmd", r.command, "args", strings.Join(args, " "))
	runErr := cmd.Run()

	// Clean up the live log after execution is done.
	if liveErr == nil {
		liveLog.Close()
		os.Remove(liveLogPath)
	}

	if ctx.Err() != nil {
		return nil, stdout.Bytes(), stderr.Bytes(), fmt.Errorf("container terminated: %w", ctx.Err())
	}

	raw := strings.TrimSpace(stdout.String())
	if raw == "" {
		if runErr != nil {
			if exitErr, ok := runErr.(*exec.ExitError); ok {
				return nil, stdout.Bytes(), stderr.Bytes(),
					fmt.Errorf("container exited with code %d: stderr=%s", exitErr.ExitCode(), stderr.String())
			}
			return nil, stdout.Bytes(), stderr.Bytes(), fmt.Errorf("exec container: %w", runErr)
		}
		stderrStr := strings.TrimSpace(stderr.String())
		if stderrStr != "" {
			return nil, stdout.Bytes(), stderr.Bytes(),
				fmt.Errorf("empty output from container: stderr=%s", truncate(stderrStr, 500))
		}
		return nil, stdout.Bytes(), stderr.Bytes(), fmt.Errorf("empty output from container")
	}

	output, parseErr := parseOutput(raw)
	if parseErr != nil {
		if runErr != nil {
			if exitErr, ok := runErr.(*exec.ExitError); ok {
				return nil, stdout.Bytes(), stderr.Bytes(),
					fmt.Errorf("container exited with code %d: stderr=%s stdout=%s",
						exitErr.ExitCode(), stderr.String(), truncate(raw, 500))
			}
			return nil, stdout.Bytes(), stderr.Bytes(), fmt.Errorf("exec container: %w", runErr)
		}
		return nil, stdout.Bytes(), stderr.Bytes(),
			fmt.Errorf("parse output: %w (raw: %s)", parseErr, truncate(raw, 200))
	}

	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			logger.Runner.Warn("sandbox exited non-zero but produced valid output",
				"task", taskID, "code", exitErr.ExitCode())
		} else {
			logger.Runner.Warn("sandbox error but produced valid output", "task", taskID, "error", runErr)
		}
	}

	return output, stdout.Bytes(), stderr.Bytes(), nil
}

// runContainer executes Claude Code in a sandbox and parses its NDJSON output.
// This is the main entry point called by the turn loop in execute.go.
// The sandbox must already exist (created by CreateSandbox).
func (r *Runner) runContainer(
	ctx context.Context,
	taskID uuid.UUID,
	prompt, sessionID string,
	worktreeOverrides map[string]string,
	boardDir string,
	siblingMounts map[string]map[string]string,
) (*claudeOutput, []byte, []byte, error) {
	// Determine working directory: use the first worktree path.
	var workdir string
	if len(worktreeOverrides) == 1 {
		for _, wt := range worktreeOverrides {
			workdir = wt
		}
	}
	return r.execInSandbox(ctx, taskID, prompt, sessionID, workdir)
}

// runOneShotSandbox creates a temporary sandbox, runs a Claude command, and removes it.
// Used for lightweight tasks like title and commit message generation.
func (r *Runner) runOneShotSandbox(ctx context.Context, name, prompt string, workspacePaths []string) (*claudeOutput, error) {
	// Clean up any leftover sandbox.
	exec.Command(r.command, "sandbox", "rm", name).Run()

	// Create sandbox.
	createArgs := []string{"sandbox", "create", "--name", name, "claude"}
	if len(workspacePaths) > 0 {
		createArgs = append(createArgs, workspacePaths...)
	} else {
		// Need at least one workspace; use a temp directory.
		tmpDir, err := os.MkdirTemp("", "wallfacer-oneshot-*")
		if err != nil {
			return nil, fmt.Errorf("create temp dir: %w", err)
		}
		defer os.RemoveAll(tmpDir)
		createArgs = append(createArgs, tmpDir)
	}

	createCmd := exec.CommandContext(ctx, r.command, createArgs...)
	if out, err := createCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("create oneshot sandbox %s: %w (output: %s)", name, err, strings.TrimSpace(string(out)))
	}
	defer func() {
		exec.Command(r.command, "sandbox", "stop", name).Run()
		exec.Command(r.command, "sandbox", "rm", name).Run()
	}()

	// Execute.
	execArgs := []string{"sandbox", "exec"}
	if r.envFile != "" {
		execArgs = append(execArgs, "--env-file", r.envFile)
	}
	execArgs = append(execArgs, name, "claude", "-p", prompt, "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions")
	if model := r.modelFromEnv(); model != "" {
		execArgs = append(execArgs, "--model", model)
	}

	cmd := exec.CommandContext(ctx, r.command, execArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("oneshot sandbox terminated: %w", ctx.Err())
	}

	raw := strings.TrimSpace(stdout.String())
	if raw == "" {
		if runErr != nil {
			return nil, fmt.Errorf("oneshot sandbox failed: %w (stderr: %s)", runErr, truncate(stderr.String(), 200))
		}
		return nil, fmt.Errorf("empty output from oneshot sandbox")
	}

	output, parseErr := parseOutput(raw)
	if parseErr != nil {
		if runErr != nil {
			return nil, fmt.Errorf("oneshot sandbox failed: %w (stderr: %s)", runErr, truncate(stderr.String(), 200))
		}
		return nil, fmt.Errorf("parse oneshot output: %w", parseErr)
	}
	return output, nil
}

// SandboxInfo represents a sandbox returned by docker sandbox ls.
type SandboxInfo struct {
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	Workspaces []string `json:"workspaces"`
}

// ListSandboxes lists all wallfacer sandboxes.
func (r *Runner) ListSandboxes() ([]SandboxInfo, error) {
	out, err := exec.Command(r.command, "sandbox", "ls", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("sandbox ls: %w", err)
	}

	// Docker sandbox ls --json has inconsistent keys:
	// "vms" when populated, "sandboxes" when empty.
	var parsed struct {
		VMs       []SandboxInfo `json:"vms"`
		Sandboxes []SandboxInfo `json:"sandboxes"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("parse sandbox ls: %w (raw: %s)", err, truncate(string(out), 200))
	}

	all := append(parsed.VMs, parsed.Sandboxes...)

	// Filter to wallfacer sandboxes only.
	var result []SandboxInfo
	for _, s := range all {
		if strings.HasPrefix(s.Name, "wf-") {
			result = append(result, s)
		}
	}
	return result, nil
}

// ListContainers returns sandbox info in the legacy ContainerInfo format
// for backward compatibility with the handler API.
func (r *Runner) ListContainers() ([]ContainerInfo, error) {
	sandboxes, err := r.ListSandboxes()
	if err != nil {
		return nil, err
	}

	result := make([]ContainerInfo, 0, len(sandboxes))
	for _, s := range sandboxes {
		taskID := strings.TrimPrefix(s.Name, "wf-")
		if taskID == s.Name {
			taskID = strings.TrimPrefix(s.Name, "wallfacer-")
		}
		if taskID == s.Name {
			taskID = ""
		}
		result = append(result, ContainerInfo{
			Name:   s.Name,
			TaskID: taskID,
			State:  s.Status,
			Status: s.Status,
		})
	}
	return result, nil
}

// copyInstructionsToWorktrees copies the workspace CLAUDE.md into each
// worktree root so Claude Code can discover it. Docker sandbox doesn't
// support arbitrary volume mounts, so we copy the file instead.
func copyInstructionsToWorktrees(instructionsPath string, worktreePaths map[string]string) {
	if instructionsPath == "" {
		return
	}
	content, err := os.ReadFile(instructionsPath)
	if err != nil {
		return
	}
	for _, wt := range worktreePaths {
		dest := filepath.Join(wt, "CLAUDE.md")
		// Don't overwrite if already exists (repo may have its own CLAUDE.md).
		if _, err := os.Stat(dest); err == nil {
			continue
		}
		os.WriteFile(dest, content, 0644)
	}
}

// modelFromEnv reads CLAUDE_CODE_MODEL from the env file (if configured).
// Returns an empty string when the file cannot be read or the key is absent.
func (r *Runner) modelFromEnv() string {
	if r.envFile == "" {
		return ""
	}
	cfg, err := envconfig.Parse(r.envFile)
	if err != nil {
		return ""
	}
	return cfg.Model
}

// parseOutput tries to parse raw as a single JSON object first; if that fails
// it scans backwards through NDJSON lines looking for the last valid object.
func parseOutput(raw string) (*claudeOutput, error) {
	var output claudeOutput
	if err := json.Unmarshal([]byte(raw), &output); err == nil {
		return &output, nil
	}
	lines := strings.Split(raw, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		if err := json.Unmarshal([]byte(line), &output); err == nil {
			return &output, nil
		}
	}
	return nil, fmt.Errorf("no valid JSON object found in output")
}

// extractSessionID scans raw NDJSON output for a session_id field.
// Claude Code emits session_id in early stream messages, so it is often
// present even when the container is killed mid-execution (e.g. timeout).
func extractSessionID(raw []byte) string {
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var obj struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal([]byte(line), &obj) == nil && obj.SessionID != "" {
			return obj.SessionID
		}
	}
	return ""
}

// runGit is a helper to run a git command and discard output (best-effort).
func runGit(dir string, args ...string) error {
	return exec.Command("git", append([]string{"-C", dir}, args...)...).Run()
}
