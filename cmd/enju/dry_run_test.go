package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
)

func TestPromptHead(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"hello", "hello"},
		{"   leading whitespace   ", "leading whitespace"},
		{"first line\nsecond line", "first line"},
		{"\n\n\nfourth line", "fourth line"},
		{strings.Repeat("x", 100), strings.Repeat("x", 80) + "…"},
	}
	for _, c := range cases {
		if got := promptHead(c.in); got != c.want {
			t.Errorf("promptHead(%q): got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildDryRunReport_HasTasksAndWarnings(t *testing.T) {
	dir := t.TempDir()
	wf := filepath.Join(dir, "wf.yaml")
	yml := `
name: test
version: 1
tasks:
  - id: alpha
    action: answer
    prompt: |
      Do alpha.
  - id: beta
    action: answer
    depends_on: [alpha]
    prompt: "Use {{alpha.content}} to produce beta."
`
	if err := os.WriteFile(wf, []byte(yml), 0644); err != nil {
		t.Fatal(err)
	}
	parsed, err := enjuYaml.Parse([]byte(yml))
	if err != nil {
		t.Fatal(err)
	}
	rep := buildDryRunReport(wf, parsed)

	if rep.Name != "test" {
		t.Errorf("Name: got %q", rep.Name)
	}
	if len(rep.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d: %+v", len(rep.Tasks), rep.Tasks)
	}
	// Task order should be stable (alpha before beta because
	// they're in the same instance-key bucket — buildDryRunReport
	// preserves slice order within a bucket).
	if rep.Tasks[0].ID != "alpha" || rep.Tasks[1].ID != "beta" {
		t.Errorf("task order: got %s,%s", rep.Tasks[0].ID, rep.Tasks[1].ID)
	}
	if len(rep.Tasks[1].DependsOn) == 0 || rep.Tasks[1].DependsOn[0] != "alpha" {
		t.Errorf("beta should depend on alpha: %v", rep.Tasks[1].DependsOn)
	}
	if rep.Tasks[0].PromptHead != "Do alpha." {
		t.Errorf("alpha prompt head: got %q", rep.Tasks[0].PromptHead)
	}
}
