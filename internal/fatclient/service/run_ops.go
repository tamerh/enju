package service

// Per-run workspace orchestration that handlers call into.
// Covers the substantive bodies of enju_create_run (template
// load + EnsureBundleOnDefault + per-run snapshot commit),
// enju_export_diagram, enju_export_run_events, and
// enju_export_run. Thin coord-forwarder handlers (list/pause/
// resume/spawn/cycle_budget/show_events) stay in mcphandlers
// — they're already at minimum size.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/enju-ai/enju/internal/common/format"
	corelayout "github.com/enju-ai/enju/internal/common/layout"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
)

// EnsureRunBranch materializes the run branch in the local
// workspace + on origin so subsequent claim/submit verbs can rely
// on the run branch being a real git ref instead of just a
// coordinator-side string.
//
// Idempotent — see Workflow.EnsureRunBranch for the case-by-case
// semantics. Errors are returned as a non-empty warning string
// (the run already exists on the coordinator side; only the
// workspace side is in question), matching the snapshot-commit
// path's "soft-fail with surface-to-user" pattern.
//
// Returns "" on success or no-op (branch already present, etc.);
// non-empty when the operator should know something didn't
// quite land.
func (s *FatClient) EnsureRunBranch(ctx context.Context, projectID int64, runData []byte) string {
	if s.enjugit == nil {
		return ""
	}
	var run map[string]interface{}
	if err := json.Unmarshal(runData, &run); err != nil {
		return ""
	}
	branch, _ := run["branch"].(string)
	if branch == "" {
		return ""
	}
	defaultBranch, _ := run["default_branch"].(string)

	wf, _, _, _, err := s.OpenWorkflow(ctx, projectID)
	if err != nil || wf == nil {
		return fmt.Sprintf("ensure run branch skipped: %v", err)
	}
	if err := wf.EnsureRunBranch(branch, defaultBranch); err != nil {
		return fmt.Sprintf("ensure run branch %q failed: %v", branch, err)
	}
	return ""
}

// EnsureProjectDefaultBranch materializes the project's new
// default branch in the local workspace + on origin. Called from
// handleSetProjectDefaultBranch after the coord-side update
// lands, so the git side doesn't drift from the coord setting.
//
// Idempotent — see Workflow.EnsureRunBranch for the case-by-case
// semantics. Errors are returned as a non-empty warning string
// (the coord already updated; only the workspace side is in
// question), matching the run-branch path's "soft-fail" pattern.
//
// oldDefault is consulted only when the new branch is brand-new.
// Falls back to "main" when oldDefault is empty (project meta
// fetch failed); that's the same fallback the coordinator uses.
func (s *FatClient) EnsureProjectDefaultBranch(ctx context.Context, projectID int64, newBranch, oldDefault string) string {
	if s.enjugit == nil || newBranch == "" {
		return ""
	}
	if oldDefault == "" {
		oldDefault = "main"
	}
	wf, _, _, _, err := s.OpenWorkflow(ctx, projectID)
	if err != nil || wf == nil {
		return fmt.Sprintf("ensure default branch skipped: %v", err)
	}
	if err := wf.EnsureRunBranch(newBranch, oldDefault); err != nil {
		return fmt.Sprintf("ensure default branch %q failed: %v", newBranch, err)
	}
	return ""
}

// RunBranchFromData pulls the `branch` field out of a run JSON
// payload as returned by GET /runs/{seq} or POST /runs. Empty
// when the payload is malformed or missing — callers pass the
// empty string through to CommitFiles, which falls back to the
// project default.
func RunBranchFromData(runData []byte) string {
	var run map[string]interface{}
	if err := json.Unmarshal(runData, &run); err != nil {
		return ""
	}
	if b, ok := run["branch"].(string); ok {
		return b
	}
	return ""
}

// RunSlugFromData extracts the run's filesystem slug (the tail
// of enju/runs/{seq}-{slug}/) from a coordinator run-detail
// payload. Empty means "fall back to the engine default" —
// callers pass the empty string to corelayout.RunDir, which
// treats it as "run".
func RunSlugFromData(runData []byte) string {
	var run map[string]interface{}
	if err := json.Unmarshal(runData, &run); err != nil {
		return ""
	}
	if s, ok := run["slug"].(string); ok {
		return s
	}
	return ""
}

// RunTemplatePrep is the state FatClient.PrepareRunTemplate
// returns and FatClient.CommitRunTemplateSnapshot consumes. Holds
// the opened workflow + loaded bundle so the post-create snapshot
// commit doesn't have to re-load anything.
type RunTemplatePrep struct {
	Workflow       *enjugit.Workflow
	LoadedTemplate *enjugit.LoadedTemplate
	YAMLContent    string
	SourceCommit   string
}

// PrepareRunTemplate handles the template-mode prefix of
// enju_create_run: open the project, best-effort pull, load the
// bundle, and pin it onto the project's default branch (auto-
// commit if the bundle files aren't tracked there yet — see
// docs/runs-and-branches.md § Templates for why this matters).
//
// Returns the YAML body (so the caller can POST to /runs), the
// source-commit SHA for provenance, and a prep handle the
// caller passes to CommitRunTemplateSnapshot after the
// coordinator assigns the run's seq.
func (s *FatClient) PrepareRunTemplate(ctx context.Context, projectID int64, templatePath, authorName, authorEmail string) (*RunTemplatePrep, error) {
	if s.enjugit == nil {
		return nil, fmt.Errorf("enju_create_run with 'path' requires a local workspace (MCP client mode)")
	}
	wf, _, _, _, err := s.OpenWorkflow(ctx, projectID)
	if err != nil {
		return nil, err
	}
	// Best-effort pull. If the remote is unreachable or has
	// diverged, fall through and scan whatever's on disk — the
	// loader will surface a clear "template not found" if the
	// file truly isn't there yet.
	_ = wf.PullBranch("")

	loaded, err := wf.LoadTemplate(templatePath)
	if err != nil {
		return nil, err
	}

	// Template-as-recipe invariant: templates live on the
	// project's default branch. If the bundle files aren't
	// tracked there yet, auto-commit to default before the run
	// branches off. Without this, the snapshot+branch-create
	// flow would sweep untracked template files onto the run's
	// branch only, leaving the template unreachable on the
	// default branch.
	committedSHA, bundleErr := wf.EnsureBundleOnDefault(loaded.BundleDir, authorName, authorEmail, s.modelName)
	if bundleErr != nil {
		return nil, fmt.Errorf("pinning template to default branch: %w", bundleErr)
	}

	prep := &RunTemplatePrep{
		Workflow:       wf,
		LoadedTemplate: loaded,
		YAMLContent:    string(loaded.Raw),
	}
	if committedSHA != "" {
		prep.SourceCommit = committedSHA
	} else if head, herr := wf.LocalBranchHash(""); herr == nil {
		prep.SourceCommit = head
	}
	return prep, nil
}

// CommitRunTemplateSnapshot freezes the loaded bundle into the
// new run's enju/runs/{seq}-{slug}/template-snapshot/ directory
// so a live template edit after this point cannot retroactively
// change the run's behavior — the executor resolves `script:`
// paths from the snapshot.
//
// Errors are returned as a warning message (non-fatal): the run
// already exists on the coordinator side; only the snapshot
// commit is in question. Empty string means success or no-op.
func (s *FatClient) CommitRunTemplateSnapshot(prep *RunTemplatePrep, runData []byte, templatePath, authorName, authorEmail string) string {
	if prep == nil || prep.Workflow == nil || prep.LoadedTemplate == nil {
		return ""
	}
	var created map[string]interface{}
	if err := json.Unmarshal(runData, &created); err != nil {
		return ""
	}
	seqF, ok := created["seq"].(float64)
	if !ok {
		return ""
	}
	seq := int(seqF)

	// The run's branch — pass to CommitArbitraryFiles so the
	// snapshot lands on THIS run's branch (not whatever branch
	// the worktree is currently on).
	runBranch, _ := created["branch"].(string)

	// Use the server-computed slug so the snapshot target
	// matches the run's result-dir prefix. Falls back to
	// client-side computation if the coordinator response
	// predates the slug field.
	runSlug, _ := created["slug"].(string)
	if runSlug == "" {
		runSlug = corelayout.ComputeRunSlug(templatePath, "")
	}
	snapshotTarget := corelayout.RunTemplateSnapshotDir(seq, runSlug)
	files, ferr := prep.Workflow.ReadBundleFiles(prep.LoadedTemplate.BundleDir, snapshotTarget)
	if ferr != nil {
		return fmt.Sprintf("snapshot skipped: %v", ferr)
	}
	if len(files) == 0 {
		return ""
	}
	// Plumbing path: no checkout, no worktree mutation. Critical
	// for concurrency — multiple parallel enju_create_run calls
	// each land their snapshot on a distinct run branch
	// simultaneously, and execute_run later materializes per-run
	// snapshots from git history rather than reading from the
	// shared worktree. See the matching execute_run change in
	// execute.go that switches the read side to
	// ReadSnapshotFromBranch / WalkSubtreeBlobsAtCommit.
	_, cerr := prep.Workflow.CommitArbitraryFilesPlumbing(enjugit.CommitArbitraryFilesRequest{
		Files:       files,
		Branch:      runBranch,
		Subject:     fmt.Sprintf("Snapshot template %s into run %d", prep.LoadedTemplate.BundleDir, seq),
		AuthorName:  authorName,
		AuthorEmail: authorEmail,
		ModelName:   s.modelName,
	})
	if cerr != nil {
		return fmt.Sprintf("snapshot commit failed: %v", cerr)
	}
	return ""
}

// CommitRunInlineSnapshot freezes a single-file copy of an
// inline run's YAML into enju/runs/{seq}-{slug}/template-snapshot/enju.yaml
// so the fatclient can resolve per-task execution-policy fields
// (container image, container runtime) from disk at execute time
// without round-tripping through the coordinator DB.
//
// Mirrors CommitRunTemplateSnapshot's contract for the inline
// case (yaml: ... at create_run with no bundle behind it): yaml
// body is the only artifact written, and the snapshot directory
// name remains template-snapshot/ even for inline runs so the
// on-disk layout is uniform across both create_run shapes.
//
// Returns "" on success or no-op (no workspace, malformed run
// payload, empty yaml); a warning string when the commit failed
// — non-fatal, the run already exists on the coordinator side.
func (s *FatClient) CommitRunInlineSnapshot(ctx context.Context, projectID int64, yamlContent string, runData []byte, authorName, authorEmail string) string {
	if s.enjugit == nil || yamlContent == "" {
		return ""
	}
	var created map[string]interface{}
	if err := json.Unmarshal(runData, &created); err != nil {
		return ""
	}
	seqF, ok := created["seq"].(float64)
	if !ok {
		return ""
	}
	seq := int(seqF)
	runBranch, _ := created["branch"].(string)
	runSlug, _ := created["slug"].(string)
	if runSlug == "" {
		runSlug = corelayout.ComputeRunSlug("", "")
	}

	wf, _, _, _, err := s.OpenWorkflow(ctx, projectID)
	if err != nil || wf == nil {
		return fmt.Sprintf("snapshot skipped: %v", err)
	}

	manifestPath := filepath.Join(corelayout.RunTemplateSnapshotDir(seq, runSlug), corelayout.BundleManifestName)
	// Plumbing path for the same reason as CommitRunTemplateSnapshot
	// (above): parallel create_run calls each write to their own
	// run branch concurrently without fighting over the worktree.
	_, cerr := wf.CommitArbitraryFilesPlumbing(enjugit.CommitArbitraryFilesRequest{
		Files: []enjugit.FileWrite{{
			RepoRelPath: manifestPath,
			Content:     []byte(yamlContent),
		}},
		Branch:      runBranch,
		Subject:     fmt.Sprintf("Snapshot inline yaml into run %d", seq),
		AuthorName:  authorName,
		AuthorEmail: authorEmail,
		ModelName:   s.modelName,
	})
	if cerr != nil {
		return fmt.Sprintf("snapshot commit failed: %v", cerr)
	}
	return ""
}

// ExportFileResult is the structured outcome of a "snapshot
// X to git under enju/runs/{seq}/..." operation. Carries the
// repo-relative path and the resulting commit SHA (or NoOp
// when content was byte-identical to what's on disk).
type ExportFileResult struct {
	RepoRelPath string
	CommitSHA   string
	NoOp        bool
}

// ExportDiagramFile renders the run's current DAG as raw
// Mermaid and commits it to enju/runs/{seq}-{slug}/graph/{phase}.mmd.
// Returns the rendered Mermaid body so handlers can both report
// the commit and inline the diagram in the response.
func (s *FatClient) ExportDiagramFile(ctx context.Context, projectID int64, runID int, phase, authorName, authorEmail string) (body string, res *ExportFileResult, err error) {
	if s.enjugit == nil {
		return "", nil, fmt.Errorf("enju_export_diagram requires a local workspace (MCP client mode)")
	}
	base := fmt.Sprintf("/api/v1/projects/%d/runs/%d", projectID, runID)
	runData, err := s.coord.Get(ctx, base)
	if err != nil {
		return "", nil, err
	}
	tasksData, err := s.coord.Get(ctx, base+"/tasks")
	if err != nil {
		return "", nil, err
	}

	// The body is "" when the run lookup failed (coordinator
	// returned an error object) — surface that to the caller
	// rather than committing an empty .mmd.
	body = format.RenderMermaidBody(runData, tasksData)
	if body == "" {
		return "", nil, fmt.Errorf("could not render diagram for run %d:%d (run not found or no tasks yet)", projectID, runID)
	}

	runBranch := RunBranchFromData(runData)
	wf, _, _, _, err := s.OpenWorkflow(ctx, projectID)
	if err != nil {
		return body, nil, err
	}
	repoPath := filepath.Join(corelayout.RunDir(runID, RunSlugFromData(runData)), "graph", fmt.Sprintf("%s.mmd", phase))

	cres, err := wf.CommitArbitraryFiles(enjugit.CommitArbitraryFilesRequest{
		Files: []enjugit.FileWrite{{
			RepoRelPath: repoPath,
			Content:     []byte(body),
		}},
		Branch:      runBranch,
		Subject:     fmt.Sprintf("Export diagram: run %d:%d phase %s", projectID, runID, phase),
		AuthorName:  authorName,
		AuthorEmail: authorEmail,
		ModelName:   s.modelName,
	})
	if err != nil {
		return body, nil, fmt.Errorf("writing diagram to clone: %w", err)
	}
	return body, &ExportFileResult{
		RepoRelPath: repoPath,
		CommitSHA:   cres.CommitSHA,
		NoOp:        cres.NoOp,
	}, nil
}

// ExportRunEventsFile pulls the coordinator's synthesized event
// timeline for a run and commits it as JSONL under
// enju/runs/{seq}-{slug}/events/{phase}.jsonl. Returns the
// decoded event slice so handlers can render an inline preview.
func (s *FatClient) ExportRunEventsFile(ctx context.Context, projectID int64, runID int, phase, authorName, authorEmail string) (events []map[string]interface{}, res *ExportFileResult, err error) {
	if s.enjugit == nil {
		return nil, nil, fmt.Errorf("enju_export_run_events requires a local workspace (MCP client mode)")
	}
	// Fetch the run record first so the events commit lands on
	// the run's branch, not the worktree's current HEAD.
	runData, err := s.coord.Get(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d", projectID, runID))
	if err != nil {
		return nil, nil, err
	}
	runBranch := RunBranchFromData(runData)
	runSlug := RunSlugFromData(runData)

	eventsData, err := s.coord.Get(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d/events", projectID, runID))
	if err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal(eventsData, &events); err != nil {
		return nil, nil, fmt.Errorf("parsing events response: %w", err)
	}

	// JSONL = one compact JSON object per line. json.Marshal
	// (not MarshalIndent) keeps each event on a single line,
	// which is the contract downstream consumers expect.
	var body bytes.Buffer
	for _, e := range events {
		line, merr := json.Marshal(e)
		if merr != nil {
			continue
		}
		body.Write(line)
		body.WriteByte('\n')
	}

	wf, _, _, _, err := s.OpenWorkflow(ctx, projectID)
	if err != nil {
		return events, nil, err
	}
	repoPath := filepath.Join(corelayout.RunDir(runID, runSlug), "events", fmt.Sprintf("%s.jsonl", phase))

	cres, err := wf.CommitArbitraryFiles(enjugit.CommitArbitraryFilesRequest{
		Files: []enjugit.FileWrite{{
			RepoRelPath: repoPath,
			Content:     body.Bytes(),
		}},
		Branch:      runBranch,
		Subject:     fmt.Sprintf("Export run events: run %d:%d phase %s (%d events)", projectID, runID, phase, len(events)),
		AuthorName:  authorName,
		AuthorEmail: authorEmail,
		ModelName:   s.modelName,
	})
	if err != nil {
		return events, nil, fmt.Errorf("writing events to clone: %w", err)
	}
	return events, &ExportFileResult{
		RepoRelPath: repoPath,
		CommitSHA:   cres.CommitSHA,
		NoOp:        cres.NoOp,
	}, nil
}

// ExportRunMarkdown assembles every accepted task's result into
// one markdown document. Reads results from the local clone at
// each task's commit_sha so the export captures exactly what was
// accepted (not a current-HEAD overlay).
func (s *FatClient) ExportRunMarkdown(ctx context.Context, projectID int64, runSeq int) (string, error) {
	runData, err := s.coord.Get(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d", projectID, runSeq))
	if err != nil {
		return "", err
	}
	var run map[string]interface{}
	json.Unmarshal(runData, &run)
	if errMsg, _ := run["error"].(string); errMsg != "" {
		return "", fmt.Errorf("%s", errMsg)
	}

	tasksData, err := s.coord.Get(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d/tasks", projectID, runSeq))
	if err != nil {
		return "", err
	}
	var tasks []map[string]interface{}
	json.Unmarshal(tasksData, &tasks)

	var remoteURL string
	if s.enjugit != nil {
		if u, _, err := s.FetchProjectMetaFull(ctx, projectID); err == nil {
			remoteURL = u
		}
	}

	var b strings.Builder
	runName, _ := run["name"].(string)
	runState, _ := run["state"].(string)
	b.WriteString(fmt.Sprintf("# Run: %s\n\n", runName))
	b.WriteString(fmt.Sprintf("Project: #%d, Run: #%d, State: %s, Tasks: %d\n\n", projectID, runSeq, runState, len(tasks)))
	b.WriteString("---\n\n")

	for _, t := range tasks {
		tid, _ := t["id"].(string)
		tstate, _ := t["state"].(string)
		action, _ := t["action"].(string)
		prompt, _ := t["prompt"].(string)
		commitSHA, _ := t["commit_sha"].(string)
		resultPath, _ := t["result_path"].(string)
		claimedBy, _ := t["claimed_by"].(string)
		defID, _ := t["task_def_id"].(string)

		b.WriteString(fmt.Sprintf("## %s\n\n", tid))
		b.WriteString(fmt.Sprintf("Action: %s | State: %s", action, tstate))
		if claimedBy != "" {
			b.WriteString(fmt.Sprintf(" | By: @%s", claimedBy))
		}
		b.WriteString("\n\n")

		// Read result from git first — for the preprint, the
		// output is what matters. Show the prompt only as
		// context below the result.
		resultShown := false
		if tstate == "accepted" && commitSHA != "" && s.enjugit != nil && remoteURL != "" {
			if wf, err := s.enjugit.ForProject(projectID, remoteURL); err == nil {
				resultFile := resultPath + "/result.md"
				if defID != "" && resultPath != "" {
					content, found, rerr := wf.ReadFileAtCommit(commitSHA, resultFile)
					if rerr == nil && found && len(content) > 0 {
						b.WriteString(string(content) + "\n\n")
						resultShown = true
					}
				}
			}
		}
		if tstate == "skipped" {
			b.WriteString("*(skipped — losing branch of a vote)*\n\n")
		}
		if !resultShown && prompt != "" {
			b.WriteString("**Prompt:** " + prompt + "\n\n")
		}
		b.WriteString("---\n\n")
	}

	return b.String(), nil
}

// PauseRun moves a run to the `paused` state. Idempotent on
// already-paused runs; coord refuses on terminal runs and
// returns an error. Member-gated server-side.
//
// Pure HTTP wrapper — same shape as ReleaseTask. Exists so
// in-process consumers (web UI) can pause without importing
// the coord client directly.
func (s *FatClient) PauseRun(ctx context.Context, projectID int64, runSeq int) error {
	return s.runStateAction(ctx, projectID, runSeq, "pause", nil)
}

// ResumeRun moves a paused run back to active or idle (coord
// picks based on whether ready work exists). No-op on already-
// alive runs; refuses on terminal.
func (s *FatClient) ResumeRun(ctx context.Context, projectID int64, runSeq int) error {
	return s.runStateAction(ctx, projectID, runSeq, "resume", nil)
}

// TerminateRun is the irreversible "human pulled the plug"
// action: cascade-skips every non-terminal task, abandons
// every open claim, transitions the run to `terminated`.
// Reason is optional; coord caps it server-side.
func (s *FatClient) TerminateRun(ctx context.Context, projectID int64, runSeq int, reason string) error {
	body := map[string]string{}
	if reason != "" {
		body["reason"] = reason
	}
	return s.runStateAction(ctx, projectID, runSeq, "terminate", body)
}

// runStateAction is the common pattern for pause/resume/
// terminate — same path shape, same error decode, same return
// contract. Centralized so a future "abort", "force-fail", etc.
// can hang off the same helper.
func (s *FatClient) runStateAction(ctx context.Context, projectID int64, runSeq int, action string, body interface{}) error {
	if projectID <= 0 || runSeq <= 0 {
		return fmt.Errorf("project_id and run_seq are required")
	}
	if body == nil {
		body = map[string]string{}
	}
	path := fmt.Sprintf("/api/v1/projects/%d/runs/%d/%s", projectID, runSeq, action)
	data, err := s.coord.Post(ctx, path, body)
	if err != nil {
		return err
	}
	var result map[string]interface{}
	if json.Unmarshal(data, &result) == nil {
		if msg, ok := result["error"].(string); ok && msg != "" {
			return fmt.Errorf("%s", msg)
		}
	}
	return nil
}
