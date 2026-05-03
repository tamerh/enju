.PHONY: build test test-v test-llm clean run check-imports

# Build
build:
	go build -o enju ./cmd/enju/

# Enforce the coordinator/fat-client/core import direction.
# See docs/architecture-boundaries.md.
check-imports:
	@bash tools/check-imports.sh

# Unit + integration tests (fast, no LLM).
test:
	go test ./... -count=1

# Same but verbose.
test-v:
	go test ./... -v -count=1

# Full test suite with the LLM-mode variants enabled (uses
# `claude -p`, costs tokens). Only a small number of tests
# check ENJU_LLM_TEST and branch on it; the rest run as-is.
test-llm:
	ENJU_LLM_TEST=1 go test ./... -v -count=1 -timeout 300s

# Run the coordinator locally
run: build
	./enju serve --port 8000

# Clean build artifacts and test output
clean:
	rm -f enju
	rm -f *.db
	rm -rf /tmp/enju-test-output

# Dev restart: build, wipe state (preserving credentials), restart coordinator.
# Usage: make dev-restart [PORT=8333]
PORT ?= 8333
dev-restart: build
	@echo "==> Stopping existing enju processes..."
	-@pkill -f "enju serve" 2>/dev/null; sleep 1
	@echo "==> Wiping DB + events DB + git-dir + workspaces + notify state (keeping credentials)..."
	rm -f ~/.enju/enju.db ~/.enju/enju.db-wal ~/.enju/enju.db-shm
	rm -f ~/.enju/enju-events.db ~/.enju/enju-events.db-wal ~/.enju/enju-events.db-shm
	# Live notify state is project-scoped (under each clone's
	# enju/events/) and goes with the workspaces wipe below.
	# Defensively clean any legacy ~/.enju/notify-* files left
	# behind from pre-redesign installs — once cleared, notify
	# never recreates them.
	rm -f ~/.enju/notify-state-*.json ~/.enju/notify-active.json ~/.enju/notify.yaml
	rm -rf ~/.enju/git-dir ~/.enju/repos
	rm -rf ~/.enju/workspaces
	mkdir -p ~/.enju/workspaces
	@echo "==> Starting coordinator on port $(PORT)..."
	./enju serve -db ~/.enju/enju.db -port $(PORT) > /tmp/enju-serve.log 2>&1 &
	@sleep 1
	@curl -sf http://localhost:$(PORT)/health > /dev/null && echo "==> Coordinator running on port $(PORT)" || echo "==> ERROR: coordinator failed to start (check /tmp/enju-serve.log)"
