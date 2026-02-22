package gitutil

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ErrConflict is returned by RebaseOntoDefault when a merge conflict is detected.
var ErrConflict = errors.New("rebase conflict")

// IsGitRepo reports whether path is inside a git repository.
func IsGitRepo(path string) bool {
	return exec.Command("git", "-C", path, "rev-parse", "--git-dir").Run() == nil
}

// DefaultBranch returns the default branch name for a repo (tries origin/HEAD,
// falls back to the current local HEAD branch, then "main").
func DefaultBranch(repoPath string) (string, error) {
	// Try symbolic ref for origin/HEAD first (most reliable for cloned repos).
	out, err := exec.Command("git", "-C", repoPath, "symbolic-ref", "--short", "refs/remotes/origin/HEAD").Output()
	if err == nil {
		// output is e.g. "origin/main" — strip the "origin/" prefix.
		branch := strings.TrimSpace(strings.TrimPrefix(string(out), "origin/"))
		if branch != "" && branch != string(out) {
			return branch, nil
		}
	}
	// Fall back to current HEAD branch name.
	out, err = exec.Command("git", "-C", repoPath, "branch", "--show-current").Output()
	if err != nil {
		return "main", nil
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return "main", nil // detached HEAD
	}
	return branch, nil
}

// GetCommitHash returns the current HEAD commit hash in repoPath.
func GetCommitHash(repoPath string) (string, error) {
	out, err := exec.Command("git", "-C", repoPath, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD in %s: %w", repoPath, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// GetCommitHashForRef returns the commit hash for a specific ref in repoPath.
func GetCommitHashForRef(repoPath, ref string) (string, error) {
	out, err := exec.Command("git", "-C", repoPath, "rev-parse", ref).Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s in %s: %w", ref, repoPath, err)
	}
	return strings.TrimSpace(string(out)), nil
}
