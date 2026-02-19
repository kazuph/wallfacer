SHELL   := /bin/bash
PODMAN  := /opt/podman/bin/podman
IMAGE   := wallfacer:latest
NAME    := wallfacer
TRACE_DIR := observability

# Load .env if it exists
-include sandbox/.env
export

# Space-separated list of folders to mount under /workspace/<basename>
WORKSPACES ?= $(CURDIR)

# Generate -v flags: /path/to/foo -> -v /path/to/foo:/workspace/foo:z
VOLUME_MOUNTS := $(foreach ws,$(WORKSPACES),-v $(ws):/workspace/$(notdir $(ws)):z)

.PHONY: build run interactive shell stop clean traces

# Build the sandbox image
build:
	$(PODMAN) build -t $(IMAGE) -f sandbox/Dockerfile sandbox/

# Headless mode: make run PROMPT="fix the failing tests"
run:
ifndef PROMPT
	$(error PROMPT is required. Usage: make run PROMPT="your task here")
endif
	@mkdir -p $(TRACE_DIR)
	$(eval TRACE_FILE := $(TRACE_DIR)/$(shell date +%Y%m%d_%H%M%S)_$(shell echo "$(PROMPT)" | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9]/-/g' | sed 's/--*/-/g' | sed 's/^-//;s/-$$//' | cut -c1-50).jsonl)
	@$(PODMAN) run --rm -it \
		--name $(NAME) \
		-e ANTHROPIC_API_KEY -e CLAUDE_CODE_OAUTH_TOKEN \
		$(VOLUME_MOUNTS) \
		-v claude-config:/home/claude/.claude \
		-w /workspace \
		$(IMAGE) -p "$(PROMPT)" --verbose --output-format stream-json \
		| tee $(TRACE_FILE) ; \
		rc=$${PIPESTATUS[0]} ; \
		echo "\nTrace saved to $(TRACE_FILE)" ; \
		exit $$rc

# Interactive TUI mode
interactive:
	$(PODMAN) run --rm -it \
		--name $(NAME) \
		-e ANTHROPIC_API_KEY -e CLAUDE_CODE_OAUTH_TOKEN \
		$(VOLUME_MOUNTS) \
		-v claude-config:/home/claude/.claude \
		-w /workspace \
		$(IMAGE)

# Debug shell
shell:
	$(PODMAN) run --rm -it \
		--name $(NAME) \
		-e ANTHROPIC_API_KEY -e CLAUDE_CODE_OAUTH_TOKEN \
		$(VOLUME_MOUNTS) \
		-v claude-config:/home/claude/.claude \
		-w /workspace \
		--entrypoint /bin/bash \
		$(IMAGE)

stop:
	-$(PODMAN) stop $(NAME)

clean:
	-$(PODMAN) stop $(NAME)
	-$(PODMAN) rm $(NAME)
	-$(PODMAN) volume rm claude-config
	-$(PODMAN) rmi $(IMAGE)

# List saved execution traces
traces:
	@ls -lt $(TRACE_DIR)/*.jsonl 2>/dev/null || echo "No traces found."
