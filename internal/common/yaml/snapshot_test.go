package yaml

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSnapshot(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, snapshotManifestName)
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("writing snapshot: %v", err)
	}
	return dir
}

func TestLoadTaskDefFromSnapshotHit(t *testing.T) {
	dir := writeSnapshot(t, `name: "test"
version: 1
tasks:
  - id: fetch
    action: compute
    script: scripts/fetch.sh
    container: alpine:3.19
    writes_artifacts: ["out/a.txt"]
  - id: build
    action: compute
    script: scripts/build.sh
    container: ghcr.io/org/builder:1.0
    depends_on: [fetch]
    writes_artifacts: ["out/b.txt"]
`)
	got, err := LoadTaskDefFromSnapshot(dir, "build")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "build" {
		t.Errorf("ID = %q, want build", got.ID)
	}
	if got.Container != "ghcr.io/org/builder:1.0" {
		t.Errorf("Container = %q, want ghcr.io/org/builder:1.0", got.Container)
	}
}

func TestLoadTaskDefFromSnapshotMiss(t *testing.T) {
	dir := writeSnapshot(t, `name: "test"
version: 1
tasks:
  - id: fetch
    action: compute
    script: scripts/fetch.sh
    writes_artifacts: ["out/a.txt"]
`)
	_, err := LoadTaskDefFromSnapshot(dir, "nonexistent")
	if err == nil {
		t.Fatal("expected not-found error, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error should name the missing task: %v", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should say not found: %v", err)
	}
}

func TestLoadTaskDefFromSnapshotMalformed(t *testing.T) {
	dir := writeSnapshot(t, "this: is: not: valid: yaml: ::::")
	_, err := LoadTaskDefFromSnapshot(dir, "anything")
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

func TestLoadTaskDefFromSnapshotMissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadTaskDefFromSnapshot(dir, "anything")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoadTaskDefFromSnapshotEmptyArgs(t *testing.T) {
	if _, err := LoadTaskDefFromSnapshot("", "foo"); err == nil {
		t.Error("expected error for empty templateDir")
	}
	if _, err := LoadTaskDefFromSnapshot("/tmp", ""); err == nil {
		t.Error("expected error for empty taskDefID")
	}
}
