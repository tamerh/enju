package service

// Artifact-read orchestration. Each method is the body of a
// per-tool handler that pairs a coordinator metadata fetch with
// local workspace reads (git content at commit, git log,
// optional shared-symlink materialization). Handlers stay
// responsible for MCP arg parsing + final text formatting.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/enju-ai/enju/internal/fatclient/enjugit"
)

// ArtifactResponse is one artifact index row's wire shape,
// mirroring the coord-side ArtifactResponse field-for-field
// (JSON tags load-bearing). Webui consumes this directly via
// Session.ListArtifacts; MCP handlers parse the same shape
// from the byte stream.
type ArtifactResponse struct {
	Path       string `json:"path"`
	LastWriter string `json:"last_writer,omitempty"`
	LastTaskID string `json:"last_task_id,omitempty"`
	LastRunID  int64  `json:"last_run_id,omitempty"`
	CommitSHA  string `json:"commit_sha,omitempty"`
	// Tracked is *bool so the false case round-trips through
	// JSON without omitempty dropping it. Empty-pointer means
	// "not specified by coord," which today implies tracked=true.
	Tracked   *bool  `json:"tracked,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

// ListArtifactsOpts mirrors the coord query string for
// /api/v1/projects/{pid}/artifacts. Empty Branch falls back to
// the project's default; empty Prefix returns every artifact.
type ListArtifactsOpts struct {
	Branch string
	Prefix string
}

// ListArtifacts returns the project's artifact index rows.
// Membership-gated server-side. Pure HTTP pass-through —
// content + history live elsewhere (GetArtifactContent /
// GetArtifactHistory).
func (s *FatClient) ListArtifacts(ctx context.Context, projectID int64, opts ListArtifactsOpts) ([]ArtifactResponse, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("project_id is required")
	}
	q := ""
	if opts.Branch != "" {
		q += "branch=" + opts.Branch
	}
	if opts.Prefix != "" {
		if q != "" {
			q += "&"
		}
		q += "prefix=" + opts.Prefix
	}
	path := fmt.Sprintf("/api/v1/projects/%d/artifacts", projectID)
	if q != "" {
		path += "?" + q
	}
	data, err := s.coord.Get(ctx, path)
	if err != nil {
		return nil, err
	}
	if msg := errorMsg(data); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	var out []ArtifactResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode artifacts: %w", err)
	}
	return out, nil
}

// commitTaskSubjectRe matches the first line of commit messages
// the enju client writes, so artifact-history annotations can
// enrich each entry with the submitting task_id and owner.
// Kept in sync with enjugit's commit-subject format
// (see enjugit/producing.go: "Task %s by @%s: %s").
// A non-match means the commit wasn't produced by a task
// submission (project init, rollback, manual commit), in which
// case the entry's task_id / owner fields stay empty.
var commitTaskSubjectRe = regexp.MustCompile(`^Task (\S+) by @(\S+):`)

// parseTaskCommitMessage extracts the task ID and username from
// a commit subject. Returns empty strings if the commit didn't
// come from an enju task submission.
func parseTaskCommitMessage(msg string) (taskID, username string) {
	if idx := indexOfNewline(msg); idx >= 0 {
		msg = msg[:idx]
	}
	m := commitTaskSubjectRe.FindStringSubmatch(msg)
	if m == nil {
		return "", ""
	}
	return m[1], m[2]
}

// indexOfNewline returns the byte index of the first newline in
// s, or -1 if none.
func indexOfNewline(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return i
		}
	}
	return -1
}

// GetArtifactContent reads an artifact's metadata from the
// coordinator's index, then reads the file bytes from the local
// clone — at the indexed commit SHA when one is recorded so the
// content matches what the index points at, or from the working
// tree as a fallback. Returns the marshaled JSON ready for
// format.ArtifactDetail.
//
// SHA-based reads use the workflow's lazy-fetch — if the local
// clone doesn't have the commit, the git layer fetches once and
// retries. So no explicit pre-pull is needed for the common
// case. The worktree-fallback path (commit_sha empty) reads
// whatever the on-disk clone currently shows.
func (s *FatClient) GetArtifactContent(ctx context.Context, projectID int64, path, branch string) ([]byte, error) {
	if s.enjugit == nil {
		return nil, fmt.Errorf("get_artifact requires a local workspace (MCP client mode)")
	}
	coordPath := fmt.Sprintf("/api/v1/projects/%d/artifacts/%s", projectID, path)
	if branch != "" {
		coordPath += "?branch=" + url.QueryEscape(branch)
	}
	metaRaw, err := s.coord.Get(ctx, coordPath)
	if err != nil {
		return nil, err
	}
	var meta map[string]interface{}
	_ = json.Unmarshal(metaRaw, &meta)
	if meta == nil {
		meta = map[string]interface{}{}
	}
	if errMsg, ok := meta["error"].(string); ok {
		return nil, fmt.Errorf("%s", errMsg)
	}

	wf, _, _, _, err := s.OpenWorkflow(ctx, projectID)
	if err != nil {
		return nil, err
	}
	commitSHA, _ := meta["commit_sha"].(string)
	repoPath := enjugit.ArtifactPath(path)
	var content []byte
	if commitSHA != "" {
		data, ok, rerr := wf.ReadFileAtCommit(commitSHA, repoPath)
		if rerr != nil || !ok {
			return nil, fmt.Errorf("artifact %q not found at commit %s", path, commitSHA)
		}
		content = data
	} else {
		// Worktree fallback (rare — coord usually supplies a SHA).
		data, rerr := os.ReadFile(filepath.Join(wf.WorkDir(), repoPath))
		if rerr != nil {
			return nil, fmt.Errorf("reading artifact from working tree: not found")
		}
		content = data
	}
	meta["path"] = path
	meta["content"] = string(content)
	out, _ := json.Marshal(meta)
	return out, nil
}

// GetArtifactHistory walks the local clone's git log for a path
// and enriches each commit with current-pointer / invalidated /
// superseded annotations by cross-referencing the coordinator's
// artifact index and the task state machine. Returns the
// marshaled JSON ready for format.ArtifactHistory.
func (s *FatClient) GetArtifactHistory(ctx context.Context, projectID int64, path, branch string) ([]byte, error) {
	if s.enjugit == nil {
		return nil, fmt.Errorf("get_artifact_history requires a local workspace (MCP client mode)")
	}
	wf, _, _, _, err := s.OpenWorkflow(ctx, projectID)
	if err != nil {
		return nil, err
	}
	// Best-effort fetch so the log walk sees commits another
	// citizen pushed since our last sync. A network blip leaves
	// us with whatever local state we have — still useful.
	_ = wf.FetchAllRefs()

	history, err := wf.LogFile(enjugit.ArtifactPath(path), branch)
	if err != nil {
		return nil, fmt.Errorf("reading git history: %w", err)
	}

	coordPath := fmt.Sprintf("/api/v1/projects/%d/artifacts/%s", projectID, path)
	if branch != "" {
		coordPath += "?branch=" + url.QueryEscape(branch)
	}
	currentCommitSHA := ""
	if artData, err := s.coord.Get(ctx, coordPath); err == nil {
		var art map[string]interface{}
		if json.Unmarshal(artData, &art) == nil {
			if str, ok := art["commit_sha"].(string); ok {
				currentCommitSHA = str
			}
		}
	}

	type historyTaskMeta struct {
		state     string
		commitSHA string
	}
	taskMetas := map[string]historyTaskMeta{}
	for _, commit := range history {
		taskID, _ := parseTaskCommitMessage(commit.Message)
		if taskID == "" {
			continue
		}
		if _, have := taskMetas[taskID]; have {
			continue
		}
		if tdata, err := s.coord.Get(ctx, "/api/v1/tasks/"+taskID); err == nil {
			var t map[string]interface{}
			if json.Unmarshal(tdata, &t) == nil {
				m := historyTaskMeta{}
				if st, ok := t["state"].(string); ok {
					m.state = st
				}
				if cs, ok := t["commit_sha"].(string); ok {
					m.commitSHA = cs
				}
				taskMetas[taskID] = m
			}
		}
	}

	entries := make([]map[string]interface{}, 0, len(history))
	for _, commit := range history {
		subject := commit.Message
		if i := indexOfNewline(subject); i >= 0 {
			subject = subject[:i]
		}
		taskID, owner := parseTaskCommitMessage(commit.Message)

		// Annotation classification, in order of precedence:
		//   1. current pointer — commit's SHA matches the artifact
		//      index's current value. The version the coordinator
		//      treats as live.
		//   2. invalidated — commit's task is currently in a
		//      non-ACCEPTED state. The task's result is being re-done.
		//   3. superseded — commit's task is ACCEPTED but its hash
		//      doesn't match the task's current commit SHA (task
		//      was invalidated, artifact reverted, then re-submitted
		//      with a new version).
		//   4. (none) — accepted commit whose hash matches its
		//      task's current commit SHA but isn't the artifact's
		//      current pointer.
		annotation := ""
		tm, haveTaskMeta := taskMetas[taskID]
		switch {
		case commit.Hash == currentCommitSHA && taskID != "":
			annotation = "current pointer"
		case haveTaskMeta && taskID != "" && tm.state != "accepted":
			annotation = "invalidated — task " + taskID + " now " + tm.state
		case haveTaskMeta && taskID != "" && tm.state == "accepted" && tm.commitSHA != "" && tm.commitSHA != commit.Hash:
			short := tm.commitSHA
			if len(short) > 8 {
				short = short[:8]
			}
			annotation = "superseded — task re-submitted as " + short
		}

		entry := map[string]interface{}{
			"hash":    commit.Hash,
			"subject": subject,
			"time":    commit.Time.Format(time.RFC3339),
			"task_id": taskID,
			"owner":   owner,
		}
		if annotation != "" {
			entry["annotation"] = annotation
		}
		entries = append(entries, entry)
	}
	out, _ := json.Marshal(map[string]interface{}{
		"path":    path,
		"history": entries,
	})
	return out, nil
}

// UntrackedArtifactRow is one entry in the
// list_untracked_artifacts report. LocalState ∈
// {"present", "missing", "(workspace unavailable)", "error: ..."}.
type UntrackedArtifactRow struct {
	Path       string
	Producer   string
	LocalState string
	Target     string // symlink target if applicable
}

// UntrackedArtifactReport bundles the structured data the
// formatter renders for enju_list_untracked_artifacts.
type UntrackedArtifactReport struct {
	Rows         []UntrackedArtifactRow
	ResolvedBranch string
	SharedRoot     string
}

// ListUntrackedArtifacts filters the coordinator's artifact
// index down to entries with tracked=false and reports their
// local-workspace visibility. For each entry, runs the
// shared-symlink materializer (best-effort — a missing
// ENJU_SHARED_ROOT makes it a noop) so calling this tool can
// fix downstream "untracked artifact missing" claim errors
// in-place when shared storage is available.
func (s *FatClient) ListUntrackedArtifacts(ctx context.Context, projectID int64, branch string) (*UntrackedArtifactReport, error) {
	if s.enjugit == nil {
		return nil, fmt.Errorf("enju_list_untracked_artifacts requires a local workspace (MCP client mode)")
	}
	// Resolve project metadata once — default_branch is load-
	// bearing for the symlink materializer (the producer's bytes
	// live at $SHARED/<slug>/<project-default-branch>/<path>,
	// not at a hard-coded "main").
	remoteURL, projName, defaultBranch, _ := s.FetchProjectMetaExpanded(ctx, projectID)
	if branch == "" {
		branch = defaultBranch
	}
	listPath := fmt.Sprintf("/api/v1/projects/%d/artifacts", projectID)
	if branch != "" {
		listPath += "?branch=" + url.QueryEscape(branch)
	}
	data, err := s.coord.Get(ctx, listPath)
	if err != nil {
		return nil, err
	}
	var rows []map[string]interface{}
	if json.Unmarshal(data, &rows) != nil {
		return nil, fmt.Errorf("unable to parse artifact index")
	}

	// Resolve the bigfiles dir for this project + branch so we
	// can stat untracked entries. Non-fatal: if workspace open
	// fails we still report what the index says (LocalState =
	// "(workspace unavailable)" per row).
	var projectRoot string
	if wf, werr := s.enjugit.ForProject(projectID, remoteURL); werr == nil {
		projectRoot = wf.ProjectRoot()
	}
	bigfilesDir := enjugit.ResolveBigfilesDir(projectRoot, projectID, projName, branch)

	var out []UntrackedArtifactRow
	for _, r := range rows {
		if t, ok := r["tracked"].(bool); !ok || t {
			continue
		}
		path, _ := r["path"].(string)
		if path == "" {
			continue
		}
		ur := UntrackedArtifactRow{
			Path:     path,
			Producer: firstNonEmptyMapValue(r, "last_writer", "last_task_id"),
		}
		if bigfilesDir == "" {
			ur.LocalState = "(workspace unavailable)"
			out = append(out, ur)
			continue
		}
		// Untracked artifacts live in bigfilesDir, gitignored.
		// Layout: <project>/.enju/bigfiles/<branch>/<path>.
		full := filepath.Join(bigfilesDir, enjugit.ArtifactPath(path))
		fi, serr := os.Lstat(full)
		if os.IsNotExist(serr) {
			ur.LocalState = "missing"
		} else if serr != nil {
			ur.LocalState = fmt.Sprintf("error: %v", serr)
		} else {
			if fi.Mode()&os.ModeSymlink != 0 {
				tgt, _ := os.Readlink(full)
				ur.Target = tgt
			}
			ur.LocalState = "present"
		}
		out = append(out, ur)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })

	return &UntrackedArtifactReport{
		Rows:           out,
		ResolvedBranch: branch,
		SharedRoot:     enjugit.SharedRoot(),
	}, nil
}

// firstNonEmptyMapValue returns the first non-empty string
// value among the given keys. Used for producer provenance
// where the artifact-index row has both last_writer
// (username) and last_task_id — either form is informative.
func firstNonEmptyMapValue(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, _ := m[k].(string); v != "" {
			return v
		}
	}
	return ""
}
