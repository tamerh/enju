package api

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	enjuGit "github.com/enju-ai/enju/internal/git"
	"github.com/enju-ai/enju/internal/store"
)

// resultDir constructs the repo-relative directory path for a task's
// result files. The returned path is relative to the project's git repo
// root (which the caller already has via the per-project Writer).
//
// Layout:
//   runs/{runSeq}/{taskDefID}/                     (no for_each)
//   runs/{runSeq}/{instanceKey}/{taskDefID}/       (with for_each)
func resultDir(runSeq int, instanceKey, taskDefID string) string {
	base := filepath.Join("runs", fmt.Sprintf("%d", runSeq))
	if instanceKey != "" {
		return filepath.Join(base, instanceKey, taskDefID)
	}
	return filepath.Join(base, taskDefID)
}

// contentExtension returns the file extension based on result type.
func contentExtension(resultType string) string {
	switch resultType {
	case "json":
		return ".json"
	default:
		return ".md"
	}
}

// writeResult writes the content and metadata as separate files.
// Returns the repo-relative result directory path (stored in DB as result_path).
// gw must be the writer for the project that owns the run.
func writeResult(gw *enjuGit.Writer, runSeq int, instanceKey, taskDefID string, content string, resultType string, metadata map[string]interface{}) (string, error) {
	dir := resultDir(runSeq, instanceKey, taskDefID)

	// Write content file — raw, no JSON wrapping, no escaping
	ext := contentExtension(resultType)
	contentPath := filepath.Join(dir, "result"+ext)
	if err := gw.WriteFile(contentPath, []byte(content)); err != nil {
		return "", fmt.Errorf("writing content: %w", err)
	}

	// Write metadata file
	metaPath := filepath.Join(dir, "metadata.json")
	metaBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshaling metadata: %w", err)
	}
	if err := gw.WriteFile(metaPath, metaBytes); err != nil {
		return "", fmt.Errorf("writing metadata: %w", err)
	}

	return dir, nil
}

// writeMultiFileResult writes named outputs as separate files based on the output schema.
// Each output name → file is declared in the schema; the values map contains the content.
// Returns the repo-relative result directory path.
func writeMultiFileResult(gw *enjuGit.Writer, runSeq int, instanceKey, taskDefID string, schema map[string]outputFileSpec, values map[string]string, metadata map[string]interface{}) (string, error) {
	dir := resultDir(runSeq, instanceKey, taskDefID)

	fileIndex := map[string]string{} // output name -> file path

	for name, value := range values {
		spec, hasSpec := schema[name]

		var fileName string
		if hasSpec && spec.File != "" {
			fileName = spec.File
		} else {
			// No file declared — use name + format extension
			format := "md"
			if hasSpec && spec.Format != "" {
				format = spec.Format
			}
			fileName = name + "." + format
		}

		fullPath := filepath.Join(dir, fileName)
		if err := gw.WriteFile(fullPath, []byte(value)); err != nil {
			return "", fmt.Errorf("writing %s: %w", fileName, err)
		}
		fileIndex[name] = fileName
	}

	// Add file index to metadata
	metadata["output_files"] = fileIndex

	metaPath := filepath.Join(dir, "metadata.json")
	metaBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshaling metadata: %w", err)
	}
	if err := gw.WriteFile(metaPath, metaBytes); err != nil {
		return "", fmt.Errorf("writing metadata: %w", err)
	}

	return dir, nil
}

// outputFileSpec is a minimal version of yaml.OutputSpec for local use.
type outputFileSpec struct {
	Description string `json:"description"`
	File        string `json:"file"`
	Format      string `json:"format"`
}

// readResultContent reads the content file for a task result.
func readResultContent(gw *enjuGit.Writer, resultPath string) (string, error) {
	// resultPath is the directory — find the content file
	// Try result.md first, then result.json
	for _, ext := range []string{".md", ".json"} {
		contentPath := filepath.Join(resultPath, "result"+ext)
		data, err := gw.ReadFile(contentPath)
		if err == nil {
			return string(data), nil
		}
	}
	return "", fmt.Errorf("no content file found in %s", resultPath)
}

// readResultForTemplate reads a task result and returns it in a format
// suitable for template resolution.
func readResultForTemplate(gw *enjuGit.Writer, resultPath, taskDefID string) (map[string]interface{}, error) {
	result := map[string]interface{}{
		"task_id": taskDefID,
	}

	// Check if this is a multi-file result by reading metadata first
	metaPath := filepath.Join(resultPath, "metadata.json")
	metaData, metaErr := gw.ReadFile(metaPath)
	if metaErr == nil {
		var meta map[string]interface{}
		if json.Unmarshal(metaData, &meta) == nil {
			result["metadata"] = meta

			// If we have output_files, this is a multi-file result
			if outputFiles, ok := meta["output_files"].(map[string]interface{}); ok && len(outputFiles) > 0 {
				contentMap := make(map[string]interface{})
				for name, fileName := range outputFiles {
					fname, _ := fileName.(string)
					if fname == "" {
						continue
					}
					data, err := gw.ReadFile(filepath.Join(resultPath, fname))
					if err != nil {
						continue
					}
					contentMap[name] = string(data)
				}
				result["content"] = contentMap
				return result, nil
			}
		}
	}

	// Fall back to single-file reading
	content, err := readResultContent(gw, resultPath)
	if err != nil {
		return nil, err
	}
	result["content"] = content

	// If content is JSON, try to parse it for named output access
	if strings.HasSuffix(resultPath, ".json") || isJSON(content) {
		var parsed interface{}
		if json.Unmarshal([]byte(content), &parsed) == nil {
			result["content"] = parsed
		}
	}

	return result, nil
}

func isJSON(s string) bool {
	s = strings.TrimSpace(s)
	return (strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}")) ||
		(strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]"))
}

// --- Artifact helpers (Phase C) ---

// artifactDirPrefix is the directory inside each project repo that holds
// all artifacts. Authors write paths relative to this prefix (e.g.
// "src/analyze.py") and the system prepends the prefix when touching the
// disk.
const artifactDirPrefix = "artifacts"

// validateArtifactPath enforces the rules from the Phase C plan:
//   - non-empty
//   - relative (no leading slash)
//   - no .. traversal
//   - no .git escape hatch
//   - valid UTF-8 (always true for Go strings, but checked for clarity)
//   - doesn't end with /
//
// Path is the user-facing form (without the "artifacts/" prefix).
func validateArtifactPath(p string) error {
	if p == "" {
		return fmt.Errorf("path is empty")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("path must be relative")
	}
	if strings.HasSuffix(p, "/") {
		return fmt.Errorf("path must not end with /")
	}
	cleaned := filepath.ToSlash(filepath.Clean(p))
	if cleaned != p {
		return fmt.Errorf("path is not in canonical form (got %q, want %q)", p, cleaned)
	}
	if strings.HasPrefix(cleaned, "../") || cleaned == ".." || strings.Contains(cleaned, "/../") {
		return fmt.Errorf("path traversal not allowed")
	}
	if cleaned == ".git" || strings.HasPrefix(cleaned, ".git/") {
		return fmt.Errorf(".git is reserved")
	}
	return nil
}

// artifactRepoPath returns the repo-relative path (under artifacts/) for
// a user-facing artifact path. The caller has already validated `p`.
func artifactRepoPath(p string) string {
	return filepath.Join(artifactDirPrefix, p)
}

// readArtifact reads the current content of an artifact from a project's
// git repo. Returns (content, true) if found, ("", false) if missing.
func readArtifact(gw *enjuGit.Writer, p string) (string, bool, error) {
	data, err := gw.ReadFile(artifactRepoPath(p))
	if err != nil {
		// Distinguish "missing" from "real error" — for now, treat any
		// read error as missing. The git package returns an os error
		// either way and we don't have a typed not-found.
		return "", false, nil
	}
	return string(data), true, nil
}

// writeArtifact writes new content for an artifact via the per-project
// git writer. The caller MUST hold the writer's lock.
func writeArtifact(gw *enjuGit.Writer, p string, content []byte) error {
	return gw.WriteFile(artifactRepoPath(p), content)
}

// marshalStringSlice serializes a []string for storage in a TEXT column.
// Empty/nil slices become "" (so the DEFAULT '' constraint holds).
func marshalStringSlice(xs []string) string {
	if len(xs) == 0 {
		return ""
	}
	b, err := json.Marshal(xs)
	if err != nil {
		return ""
	}
	return string(b)
}

// unmarshalStringSlice parses the storage form back to a slice. An empty
// string yields nil (no entries).
func unmarshalStringSlice(s string) []string {
	if s == "" {
		return nil
	}
	var xs []string
	if err := json.Unmarshal([]byte(s), &xs); err != nil {
		return nil
	}
	return xs
}

// --- Artifact rollback on invalidation ---
//
// When a task with writes_artifacts is invalidated, the artifact
// content on disk still reflects what that task wrote. Without a
// rollback step, the re-claimed task would see its own (invalidated)
// output in its reads_artifacts, which breaks re-runnability.
//
// The fix walks git history for each artifact path, finds the most
// recent commit by a task that is NOT in the invalidated set, and
// restores the file to that state. If no prior writer exists, the
// file is deleted. The rollback is committed as one git commit
// before the DB state transition runs.

// commitTaskSubjectRe matches the subject line of commits we generate
// for task submissions. The format is:
//
//     Task {taskID} by @{username}: result (+ N artifact(s))
//
// See handleSubmitResult for the source of truth on the commit format.
var commitTaskSubjectRe = regexp.MustCompile(`^Task (\S+) by @(\S+):`)

// parseTaskCommitMessage extracts the task ID and username from a
// commit message subject. Returns empty strings if the commit wasn't
// produced by a task submission (e.g., the initial project README
// commit, or a rollback commit).
func parseTaskCommitMessage(msg string) (taskID, username string) {
	// Take only the first line of the message.
	if idx := strings.Index(msg, "\n"); idx >= 0 {
		msg = msg[:idx]
	}
	m := commitTaskSubjectRe.FindStringSubmatch(msg)
	if m == nil {
		return "", ""
	}
	return m[1], m[2]
}

// artifactRollback describes a single path's rollback outcome. Used to
// update the artifacts index after the git-level restore.
type artifactRollback struct {
	Path          string // user-facing artifact path (no "artifacts/" prefix)
	Deleted       bool   // true if no prior writer was found — file removed
	RestoredHash  string // git commit SHA we restored from (if !Deleted)
	RestoredTask  string // fully-qualified task ID of the restored commit
	RestoredOwner string // username of the restored commit's author (from message)
}

// rollbackArtifactsForInvalidation walks git history for each artifact
// path in `paths` and restores it to the most recent version written
// by a task that is:
//
//   (a) NOT in `invalidatedTaskIDs` (the tasks being invalidated right
//       now), AND
//   (b) currently in ACCEPTED state in the DB.
//
// The second check matters because a task that was invalidated in a
// previous round is now in READY state — its committed version of the
// file is a ghost revision and must not be used as a rollback target.
// This bug surfaced during iteration 3.1 poke testing.
//
// If no valid rollback target exists for a path, the file is deleted
// from the working tree.
//
// The caller MUST hold the per-project writer lock and is responsible
// for committing the resulting changes.
func rollbackArtifactsForInvalidation(
	gw *enjuGit.Writer,
	st *store.Store,
	invalidatedTaskIDs map[string]bool,
	paths []string,
) ([]artifactRollback, error) {
	out := make([]artifactRollback, 0, len(paths))
	for _, path := range paths {
		repoPath := artifactRepoPath(path)
		history, err := gw.LogFile(repoPath)
		if err != nil {
			return nil, fmt.Errorf("reading git log for %s: %w", path, err)
		}

		// Find the first commit whose author task is:
		//  - parseable (skip rollback commits and the initial README)
		//  - not in the invalidated-right-now set
		//  - currently ACCEPTED in the DB (skip previously-invalidated
		//    tasks still in READY)
		var (
			restoreHash  string
			restoreTask  string
			restoreOwner string
		)
		for _, c := range history {
			taskID, owner := parseTaskCommitMessage(c.Message)
			if taskID == "" {
				continue
			}
			if invalidatedTaskIDs[taskID] {
				continue
			}
			t, err := st.GetTask(taskID)
			if err != nil || t == nil {
				continue
			}
			if t.State != store.TaskAccepted {
				// Previously invalidated, currently re-running, or in
				// any other non-accepted state — this commit is a
				// ghost revision from the walker's perspective.
				continue
			}
			restoreHash = c.Hash
			restoreTask = taskID
			restoreOwner = owner
			break
		}

		if restoreHash == "" {
			// No prior writer found. The invalidated task created this
			// artifact; roll back by deleting it.
			if err := gw.RemoveFile(repoPath); err != nil {
				return nil, fmt.Errorf("deleting %s: %w", path, err)
			}
			out = append(out, artifactRollback{
				Path:    path,
				Deleted: true,
			})
			continue
		}

		// Restore the file to its state at the earlier commit.
		content, exists, err := gw.ReadFileAtCommit(restoreHash, repoPath)
		if err != nil {
			return nil, fmt.Errorf("reading %s at %s: %w", path, restoreHash, err)
		}
		if !exists {
			// Defensive: LogFile returned a commit but the file wasn't
			// in its tree. Shouldn't happen — treat as delete.
			if err := gw.RemoveFile(repoPath); err != nil {
				return nil, fmt.Errorf("deleting %s: %w", path, err)
			}
			out = append(out, artifactRollback{
				Path:    path,
				Deleted: true,
			})
			continue
		}

		if err := gw.WriteFile(repoPath, content); err != nil {
			return nil, fmt.Errorf("restoring %s: %w", path, err)
		}
		out = append(out, artifactRollback{
			Path:          path,
			RestoredHash:  restoreHash,
			RestoredTask:  restoreTask,
			RestoredOwner: restoreOwner,
		})
	}
	return out, nil
}
