package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"changkun.de/wallfacer/internal/gitutil"
	"changkun.de/wallfacer/internal/logger"
	"github.com/google/uuid"
)

// GitStatus returns git status for every configured workspace.
func (h *Handler) GitStatus(w http.ResponseWriter, r *http.Request) {
	workspaces := h.runner.Workspaces()
	statuses := make([]gitutil.WorkspaceGitStatus, 0, len(workspaces))
	for _, ws := range workspaces {
		statuses = append(statuses, gitutil.WorkspaceStatus(ws))
	}
	writeJSON(w, http.StatusOK, statuses)
}

// GitStatusStream streams git status for all workspaces as SSE (5-second poll).
func (h *Handler) GitStatusStream(w http.ResponseWriter, r *http.Request) {
	if !acquireSSESlot(w) {
		return
	}
	defer releaseSSESlot()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	collect := func() []gitutil.WorkspaceGitStatus {
		workspaces := h.runner.Workspaces()
		statuses := make([]gitutil.WorkspaceGitStatus, 0, len(workspaces))
		for _, ws := range workspaces {
			statuses = append(statuses, gitutil.WorkspaceStatus(ws))
		}
		return statuses
	}

	send := func(statuses []gitutil.WorkspaceGitStatus) bool {
		data, err := json.Marshal(statuses)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	current := collect()
	if !send(current) {
		return
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			next := collect()
			nextData, _ := json.Marshal(next)
			curData, _ := json.Marshal(current)
			if string(nextData) != string(curData) {
				if !send(next) {
					return
				}
				current = next
			}
		}
	}
}

// GitPush runs `git push` for the requested workspace.
func (h *Handler) GitPush(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Workspace string `json:"workspace"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if !h.isAllowedWorkspace(req.Workspace) {
		http.Error(w, "workspace not configured", http.StatusBadRequest)
		return
	}

	logger.Git.Info("push", "workspace", req.Workspace)
	out, err := exec.CommandContext(r.Context(), "git", "-C", req.Workspace, "push").CombinedOutput()
	if err != nil {
		logger.Git.Error("push failed", "workspace", req.Workspace, "error", err, "output", string(out))
		msg := "push failed"
		outStr := string(out)
		if strings.Contains(outStr, "non-fast-forward") {
			msg = "push rejected: non-fast-forward update (try syncing first)"
		} else if strings.Contains(outStr, "permission denied") || strings.Contains(outStr, "Authentication failed") {
			msg = "push failed: authentication error"
		}
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"output": string(out)})
}

// GitSyncWorkspace fetches from remote and rebases the current branch onto its upstream.
func (h *Handler) GitSyncWorkspace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Workspace string `json:"workspace"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if !h.isAllowedWorkspace(req.Workspace) {
		http.Error(w, "workspace not configured", http.StatusBadRequest)
		return
	}

	logger.Git.Info("sync workspace", "workspace", req.Workspace)

	if out, err := exec.CommandContext(r.Context(), "git", "-C", req.Workspace, "fetch").CombinedOutput(); err != nil {
		logger.Git.Error("fetch failed", "workspace", req.Workspace, "error", err, "output", string(out))
		http.Error(w, "fetch failed", http.StatusInternalServerError)
		return
	}

	out, err := exec.CommandContext(r.Context(), "git", "-C", req.Workspace, "rebase", "@{u}").CombinedOutput()
	if err != nil {
		exec.Command("git", "-C", req.Workspace, "rebase", "--abort").Run()
		logger.Git.Error("sync rebase failed", "workspace", req.Workspace, "error", err)
		if gitutil.IsConflictOutput(string(out)) {
			http.Error(w, "rebase conflict: resolve manually in "+req.Workspace, http.StatusConflict)
			return
		}
		http.Error(w, "rebase failed", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"output": string(out)})
}

// TaskDiff returns the git diff for a task's worktrees versus the default branch.
func (h *Handler) TaskDiff(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	task, err := h.store.GetTask(r.Context(), id)
	if err != nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}
	if len(task.WorktreePaths) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"diff": "", "behind_counts": map[string]int{}})
		return
	}

	var combined strings.Builder
	behindCounts := make(map[string]int)

	for repoPath, worktreePath := range task.WorktreePaths {
		// If the worktree directory no longer exists, fall back to stored commit hashes.
		if _, statErr := os.Stat(worktreePath); statErr != nil {
			commitHash := task.CommitHashes[repoPath]
			var out []byte
			if commitHash != "" {
				if baseHash := task.BaseCommitHashes[repoPath]; baseHash != "" {
					out, _ = exec.CommandContext(r.Context(), "git", "-C", repoPath,
						"diff", baseHash, commitHash).Output()
				} else {
					out, _ = exec.CommandContext(r.Context(), "git", "-C", repoPath,
						"show", commitHash).Output()
				}
			} else if task.BranchName != "" {
				if defBranch, err := gitutil.DefaultBranch(repoPath); err == nil {
					// Use merge-base so we only see changes introduced on the task
					// branch, not the inverse of commits that advanced main.
					if base, mbErr := gitutil.MergeBase(repoPath, defBranch, task.BranchName); mbErr == nil {
						out, _ = exec.CommandContext(r.Context(), "git", "-C", repoPath,
							"diff", base, task.BranchName).Output()
					} else {
						out, _ = exec.CommandContext(r.Context(), "git", "-C", repoPath,
							"diff", defBranch+".."+task.BranchName).Output()
					}
				}
			}
			if len(out) > 0 {
				if len(task.WorktreePaths) > 1 {
					fmt.Fprintf(&combined, "=== %s ===\n", filepath.Base(repoPath))
				}
				combined.Write(out)
			}
			continue
		}

		defBranch, err := gitutil.DefaultBranch(repoPath)
		if err != nil {
			continue
		}
		// Use merge-base to diff only this task's changes since it diverged,
		// ignoring any commits that advanced the default branch from other tasks.
		// Fall back to diffing against the default branch tip if merge-base fails.
		base, err := gitutil.MergeBase(worktreePath, "HEAD", defBranch)
		if err != nil {
			base = defBranch
		}
		out, _ := exec.CommandContext(r.Context(), "git", "-C", worktreePath, "diff", base).Output()

		// Include untracked files via --no-index diffs.
		if untrackedRaw, err := exec.CommandContext(r.Context(), "git", "-C", worktreePath,
			"ls-files", "--others", "--exclude-standard").Output(); err == nil {
			for _, file := range strings.Split(strings.TrimSpace(string(untrackedRaw)), "\n") {
				if file == "" {
					continue
				}
				fd, _ := exec.CommandContext(r.Context(), "git", "-C", worktreePath,
					"diff", "--no-index", "/dev/null", file).Output()
				out = append(out, fd...)
			}
		}

		if len(out) > 0 {
			if len(task.WorktreePaths) > 1 {
				fmt.Fprintf(&combined, "=== %s ===\n", filepath.Base(repoPath))
			}
			combined.Write(out)
		}
		if n, err := gitutil.CommitsBehind(repoPath, worktreePath); err == nil && n > 0 {
			behindCounts[filepath.Base(repoPath)] = n
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"diff":          combined.String(),
		"behind_counts": behindCounts,
	})
}

// isAllowedWorkspace checks that the workspace path is one the server was started with.
func (h *Handler) isAllowedWorkspace(ws string) bool {
	for _, configured := range h.runner.Workspaces() {
		if configured == ws {
			return true
		}
	}
	return false
}
