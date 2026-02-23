package runner

import (
	"context"
	"strings"
	"time"

	"changkun.de/wallfacer/internal/logger"
	"github.com/google/uuid"
)

// GenerateTitle runs a lightweight one-shot sandbox to produce a 2-5 word title
// summarising the task prompt, then persists it via the store.
// Errors are logged and silently dropped so callers can fire-and-forget.
func (r *Runner) GenerateTitle(taskID uuid.UUID, prompt string) {
	// Skip if the task already has a title.
	if t, err := r.store.GetTask(context.Background(), taskID); err == nil && t.Title != "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	name := "wf-t-" + taskID.String()[:8]

	titlePrompt := "Respond with ONLY a 2-5 word title that captures the main goal of the following task. " +
		"No punctuation, no quotes, no explanation — just the title.\n\nTask:\n" + prompt

	output, err := r.runOneShotSandbox(ctx, name, titlePrompt, nil)
	if err != nil {
		logger.Runner.Warn("title generation failed", "task", taskID, "error", err)
		return
	}

	title := strings.TrimSpace(output.Result)
	title = strings.Trim(title, `"'`)
	title = strings.TrimSpace(title)
	if title == "" {
		logger.Runner.Warn("title generation: blank result", "task", taskID)
		return
	}

	if err := r.store.UpdateTaskTitle(context.Background(), taskID, title); err != nil {
		logger.Runner.Warn("title generation: store update failed", "task", taskID, "error", err)
	}
}
