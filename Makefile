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
