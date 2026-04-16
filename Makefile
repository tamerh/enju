.PHONY: build test test-llm clean run

# Build
build:
	go build -o enju ./cmd/enju/

# Unit + integration tests (fast, no LLM)
test:
	go test ./... -count=1

# Unit + integration tests with verbose output
test-v:
	go test ./... -v -count=1

# Full test suite including LLM tests (uses claude -p, costs tokens)
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
	@echo "==> Wiping DB + git-dir + workspaces (keeping credentials)..."
	rm -rf ~/.enju/enju.db ~/.enju/git-dir ~/.enju/repos
	rm -rf ~/.enju/workspaces
	mkdir -p ~/.enju/workspaces
	@echo "==> Starting coordinator on port $(PORT)..."
	./enju serve -db ~/.enju/enju.db -port $(PORT) > /tmp/enju-serve.log 2>&1 &
	@sleep 1
	@curl -sf http://localhost:$(PORT)/api/v1/projects > /dev/null && echo "==> Coordinator running on port $(PORT)" || echo "==> ERROR: coordinator failed to start (check /tmp/enju-serve.log)"
