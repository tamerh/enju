package git

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestInitAndCommit(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	w, err := NewWriter(dir, "", logger)
	if err != nil {
		t.Fatal(err)
	}

	// Write a file
	err = w.WriteFile("results/test/task1.json", []byte(`{"content": "hello"}`))
	if err != nil {
		t.Fatal(err)
	}

	// Verify file exists
	data, err := w.ReadFile("results/test/task1.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"content": "hello"}` {
		t.Fatalf("unexpected content: %s", data)
	}

	// Commit
	err = w.Commit("Add task1 result")
	if err != nil {
		t.Fatal(err)
	}

	// Write another file and commit again
	err = w.WriteFile("results/test/task2.json", []byte(`{"content": "world"}`))
	if err != nil {
		t.Fatal(err)
	}

	err = w.Commit("Add task2 result")
	if err != nil {
		t.Fatal(err)
	}

	// Empty commit should be a no-op
	err = w.Commit("Nothing changed")
	if err != nil {
		t.Fatal(err)
	}
}

func TestReopenRepo(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	// Create and write
	w1, err := NewWriter(dir, "", logger)
	if err != nil {
		t.Fatal(err)
	}
	w1.WriteFile("test.txt", []byte("hello"))
	w1.Commit("initial")

	// Reopen same directory
	w2, err := NewWriter(dir, "", logger)
	if err != nil {
		t.Fatal(err)
	}

	data, err := w2.ReadFile("test.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("unexpected content after reopen: %s", data)
	}
}

func TestWorkDir(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "myrepo")
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	w, err := NewWriter(subdir, "", logger)
	if err != nil {
		t.Fatal(err)
	}

	if w.WorkDir() != subdir {
		t.Fatalf("expected workdir %s, got %s", subdir, w.WorkDir())
	}
}

func TestPushNoRemote(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	w, err := NewWriter(dir, "", logger)
	if err != nil {
		t.Fatal(err)
	}

	// Push with no remote should not error
	err = w.Push()
	if err != nil {
		t.Fatalf("push with no remote should succeed silently, got: %v", err)
	}
}
