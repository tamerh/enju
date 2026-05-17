package mcphandlers

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/bots"
	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/mark3labs/mcp-go/mcp"
)

// writeFakeBinary creates a shell-script stand-in for `enju
// bot run`. Echoes its full argv to stdout, then reads stdin
// until EOF. Same shape as bots/supervisor_test.go's helper —
// duplicated here so this package's test doesn't depend on
// non-exported helpers in another package.
func writeFakeBinary(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("supervisor manifest→argv test uses shell-script fake binary; POSIX only")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-enju")
	script := `#!/bin/sh
echo "fake-enju args: $@"
while IFS= read -r line; do : ; done
`
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeWorkflowForBot drops a minimal workflow YAML at a fresh
// temp dir with one inline bot whose mcp_tools.allow is the
// given allowlist. Returns the workflow file's absolute path —
// callers pass it as the "workflow" arg to handleBotStart.
func writeWorkflowForBot(t *testing.T, botName string, allow []string) string {
	t.Helper()
	dir := t.TempDir()
	allowYaml := ""
	if allow != nil {
		allowYaml = "    mcp_tools:\n      allow: [" + strings.Join(allow, ", ") + "]\n"
	}
	wf := "name: test-workflow\nbase_branch: main\nagents:\n  - name: " + botName + "\n    model: claude-sonnet-4-6\n    args: [\"-p\"]\n" + allowYaml + "tasks:\n  - id: noop\n    action: answer\n    prompt: ok\n"
	path := filepath.Join(dir, "workflow.yaml")
	if err := os.WriteFile(path, []byte(wf), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// newClientWithSupervisor builds an apiClient with a pre-injected
// Supervisor pointing at a fake binary. The coord client is a
// bare placeholder — handleBotStart only reads BaseURL() from
// it, never calls a real coord during this test.
func newClientWithSupervisor(t *testing.T, fakeBin, pidDir, logDir string) *apiClient {
	t.Helper()
	sup := bots.NewSupervisorForTest(fakeBin, pidDir, logDir)
	coordClient := coord.New(coord.Config{
		BaseURL:     "http://test-unused:0",
		Username:    "test-operator",
		CitizenName: "Test Operator",
		AuthToken:   "test-token",
	})
	fc := service.New(service.Config{Coord: coordClient})
	return &apiClient{fc: fc, supervisor: sup}
}

// TestHandleBotStart_ForwardsManifestAllowlistToDaemon pins
// the Phase 1 trust-model claim end-to-end at the
// fatclient→daemon boundary: manifest declares
// mcp_tools.allow, handleBotStart reads it, supervisor passes
// it as --allow-tools=Read,Edit,... to the spawned daemon.
//
// The daemon (a fake-binary shell script) echoes its argv to
// the log file; the test scans the log for the expected flag.
func TestHandleBotStart_ForwardsManifestAllowlistToDaemon(t *testing.T) {
	fakeBin := writeFakeBinary(t)
	pidDir := t.TempDir()
	logDir := t.TempDir()
	c := newClientWithSupervisor(t, fakeBin, pidDir, logDir)
	defer c.supervisor.StopAll(context.Background())

	wfPath := writeWorkflowForBot(t, "scoped-bot", []string{"Read", "Grep", "Glob"})

	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "enju_agent_start",
			Arguments: map[string]any{
				"agent":     "scoped-bot",
				"workflow": wfPath,
			},
		},
	}
	res, err := c.handleBotStart(context.Background(), req)
	if err != nil {
		t.Fatalf("handleBotStart: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleBotStart returned error result: %s", textOf(res))
	}

	// Wait for the fake binary to write its argv echo to the
	// log file.
	logPath := filepath.Join(logDir, "scoped-bot.log")
	deadline := time.Now().Add(2 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(logPath)
		if err == nil && strings.Contains(string(body), "--allow-tools=") {
			got = string(body)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got == "" {
		body, _ := os.ReadFile(logPath)
		t.Fatalf("--allow-tools never reached daemon argv. log:\n%s", body)
	}
	want := "--allow-tools=Read,Grep,Glob"
	if !strings.Contains(got, want) {
		t.Errorf("daemon argv: missing %q\nlog:\n%s", want, got)
	}
}

// TestHandleBotStart_OmittedManifestAllowlistMeansAllTools
// pins the backwards-compat default: a manifest entry without
// mcp_tools.allow doesn't pass --allow-tools at all (daemon
// gets the full toolbox). Distinguishes from the explicit-
// empty-allowlist case which Validate rejects.
func TestHandleBotStart_OmittedManifestAllowlistMeansAllTools(t *testing.T) {
	fakeBin := writeFakeBinary(t)
	pidDir := t.TempDir()
	logDir := t.TempDir()
	c := newClientWithSupervisor(t, fakeBin, pidDir, logDir)
	defer c.supervisor.StopAll(context.Background())

	wfPath := writeWorkflowForBot(t, "open-bot", nil) // no mcp_tools.allow

	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "enju_agent_start",
			Arguments: map[string]any{
				"agent":     "open-bot",
				"workflow": wfPath,
			},
		},
	}
	res, err := c.handleBotStart(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("handleBotStart: err=%v res=%s", err, textOf(res))
	}

	logPath := filepath.Join(logDir, "open-bot.log")
	deadline := time.Now().Add(2 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(logPath)
		if err == nil && strings.Contains(string(body), "fake-enju args:") {
			got = string(body)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got == "" {
		t.Fatalf("daemon never echoed args to log %s", logPath)
	}
	if strings.Contains(got, "--allow-tools=") {
		t.Errorf("expected NO --allow-tools flag (manifest omitted mcp_tools), got argv:\n%s", got)
	}
}

// TestHandleBotStart_AutoDiscoversWhenSingleBotInManifest pins
// the UX win: omitting `bot` succeeds when the manifest has
// exactly one entry. Cuts redundant typing for the dominant
// solo-project case.
func TestHandleBotStart_AutoDiscoversWhenSingleBotInManifest(t *testing.T) {
	fakeBin := writeFakeBinary(t)
	pidDir := t.TempDir()
	logDir := t.TempDir()
	c := newClientWithSupervisor(t, fakeBin, pidDir, logDir)
	defer c.supervisor.StopAll(context.Background())

	wfPath := writeWorkflowForBot(t, "lonely-bot", nil)
	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "enju_agent_start",
			Arguments: map[string]any{
				"workflow": wfPath, // no `bot` field
			},
		},
	}
	res, err := c.handleBotStart(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("handleBotStart: err=%v res=%s", err, textOf(res))
	}
	if !strings.Contains(textOf(res), "lonely-bot") {
		t.Errorf("expected response to mention auto-discovered bot lonely-bot, got %s", textOf(res))
	}
}

// TestHandleBotStart_RefusesAmbiguousAutoDiscover pins the
// safety: when the manifest has two or more bots, omitting
// `bot` errors with a list of names so the operator can pick.
// Without this guard the supervisor would silently start the
// first bot — surprising and impossible to reason about.
func TestHandleBotStart_RefusesAmbiguousAutoDiscover(t *testing.T) {
	fakeBin := writeFakeBinary(t)
	pidDir := t.TempDir()
	logDir := t.TempDir()
	c := newClientWithSupervisor(t, fakeBin, pidDir, logDir)
	defer c.supervisor.StopAll(context.Background())

	dir := t.TempDir()
	wf := "name: multi-bot\nbase_branch: main\nagents:\n  - name: alice\n    model: claude-sonnet-4-6\n    args: [\"-p\"]\n  - name: bob\n    model: claude-sonnet-4-6\n    args: [\"-p\"]\ntasks:\n  - id: noop\n    action: answer\n    prompt: ok\n"
	wfPath := filepath.Join(dir, "workflow.yaml")
	if err := os.WriteFile(wfPath, []byte(wf), 0644); err != nil {
		t.Fatal(err)
	}

	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "enju_agent_start",
			Arguments: map[string]any{
				"workflow": wfPath,
			},
		},
	}
	res, err := c.handleBotStart(context.Background(), req)
	if err != nil {
		t.Fatalf("handleBotStart: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected error result on ambiguous auto-discover; got success: %s", textOf(res))
	}
	msg := textOf(res)
	if !strings.Contains(msg, "alice") || !strings.Contains(msg, "bob") {
		t.Errorf("error must list both candidate names; got %s", msg)
	}
}

// textOf extracts the first text content from a CallToolResult
// for error messages. Matches the inlined helper in mcp-go's
// own tests.
func textOf(r *mcp.CallToolResult) string {
	if r == nil {
		return "<nil>"
	}
	for _, c := range r.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}
