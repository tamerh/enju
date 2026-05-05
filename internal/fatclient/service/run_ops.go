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
	"github.com/enju-ai/enju/internal/fatclient/workspace"
)

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
// the opened project + loaded bundle so the post-create snapshot
// commit doesn't have to re-load anything.
type RunTemplatePrep struct {
	Project        *workspace.Project
	LoadedTemplate *workspace.LoadedTemplate
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
	if s.workspace == nil {
		return nil, fmt.Errorf("enju_create_run with 'path' requires a local workspace (MCP client mode)")
	}
	proj, _, _, _, err := s.OpenProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	// Best-effort pull. If the remote is unreachable or has
	// diverged, fall through and scan whatever's on disk — the
	// loader will surface a clear "template not found" if the
	// file truly isn't there yet.
	proj.Lock()
	_ = proj.Pull()
	proj.Unlock()

	loaded, err := proj.LoadTemplate(templatePath)
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
	proj.Lock()
	committedSHA, bundleErr := proj.EnsureBundleOnDefault(loaded.BundleDir, authorName, authorEmail, s.modelName)
	proj.Unlock()
	if bundleErr != nil {
		return nil, fmt.Errorf("pinning template to default branch: %w", bundleErr)
	}

	prep := &RunTemplatePrep{
		Project:        proj,
		LoadedTemplate: loaded,
		YAMLContent:    string(loaded.Raw),
	}
	if committedSHA != "" {
		prep.SourceCommit = committedSHA
	} else if head, herr := proj.HeadHash(); herr == nil {
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
	if prep == nil || prep.Project == nil || prep.LoadedTemplate == nil {
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

	// The run's branch — pass to CommitFiles so the template
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
	files, ferr := prep.Project.ReadBundleFiles(prep.LoadedTemplate.BundleDir, snapshotTarget)
	if ferr != nil {
		return fmt.Sprintf("snapshot skipped: %v", ferr)
	}
	if len(files) == 0 {
		return ""
	}
	prep.Project.Lock()
	_, cerr := prep.Project.CommitFiles(workspace.CommitFilesRequest{
		Files:       files,
		CommitMsg:   fmt.Sprintf("Snapshot template %s into run %d", prep.LoadedTemplate.BundleDir, seq),
		AuthorName:  authorName,
		AuthorEmail: authorEmail,
		ModelName:   s.modelName,
		Branch:      runBranch,
	})
	prep.Project.Unlock()
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
	if s.workspace == nil {
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
	proj, _, _, _, err := s.OpenProject(ctx, projectID)
	if err != nil {
		return body, nil, err
	}
	repoPath := filepath.Join(corelayout.RunDir(runID, RunSlugFromData(runData)), "graph", fmt.Sprintf("%s.mmd", phase))

	proj.Lock()
	cres, err := proj.CommitFiles(workspace.CommitFilesRequest{
		Files: []workspace.FileWrite{{
			RepoRelPath: repoPath,
			Content:     []byte(body),
		}},
		CommitMsg:   fmt.Sprintf("Export diagram: run %d:%d phase %s", projectID, runID, phase),
		AuthorName:  authorName,
		AuthorEmail: authorEmail,
		ModelName:   s.modelName,
		Branch:      runBranch,
	})
	proj.Unlock()
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
	if s.workspace == nil {
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

	proj, _, _, _, err := s.OpenProject(ctx, projectID)
	if err != nil {
		return events, nil, err
	}
	repoPath := filepath.Join(corelayout.RunDir(runID, runSlug), "events", fmt.Sprintf("%s.jsonl", phase))

	proj.Lock()
	cres, err := proj.CommitFiles(workspace.CommitFilesRequest{
		Files: []workspace.FileWrite{{
			RepoRelPath: repoPath,
			Content:     body.Bytes(),
		}},
		CommitMsg:   fmt.Sprintf("Export run events: run %d:%d phase %s (%d events)", projectID, runID, phase, len(events)),
		AuthorName:  authorName,
		AuthorEmail: authorEmail,
		ModelName:   s.modelName,
		Branch:      runBranch,
	})
	proj.Unlock()
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

	var remoteURL, projName string
	if s.workspace != nil {
		if u, n, err := s.FetchProjectMetaFull(ctx, projectID); err == nil {
			remoteURL = u
			projName = n
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
		if tstate == "accepted" && commitSHA != "" && s.workspace != nil && remoteURL != "" {
			if proj, err := s.workspace.ForProject(projectID, remoteURL, projName); err == nil {
				resultFile := resultPath + "/result.md"
				if defID != "" && resultPath != "" {
					content, found, rerr := proj.ReadFileAtCommit(commitSHA, resultFile)
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
