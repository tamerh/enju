package mcpserver

// Small helpers shared across the MCP handler files. Each is
// a few lines, used in 2+ places, and has no home in any
// particular feature file.

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
)

// extractErrorString pulls an `error` field out of a JSON
// response if present — used to surface coordinator error
// bodies through handlers that don't do full response
// parsing.
func extractErrorString(data []byte) string {
	var raw map[string]interface{}
	if json.Unmarshal(data, &raw) != nil {
		return ""
	}
	if s, ok := raw["error"].(string); ok {
		return s
	}
	return ""
}

// sortStringsStable is a tiny in-place insertion sort so the
// handler files don't need their own sort import for one
// call.
func sortStringsStable(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}

// indexOfNewline returns the byte index of the first newline
// in s, or -1 if none. Used by the artifact-history formatter
// to trim commit message bodies down to their subject lines.
func indexOfNewline(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return i
		}
	}
	return -1
}

// commitTaskSubjectRe matches the first line of commit
// messages the enju client writes, so get_artifact_history
// can enrich each entry with the submitting task_id and
// owner. Kept in sync with mcpgit.buildCommitMessage's
// format. A non-match means the commit wasn't produced by a
// task submission (project init, rollback, manual commit),
// in which case the entry's task_id / owner fields stay
// empty.
var commitTaskSubjectRe = regexp.MustCompile(`^Task (\S+) by @(\S+):`)

// parseTaskCommitMessage extracts the task ID and username
// from a commit subject. Returns empty strings if the commit
// didn't come from an enju task submission.
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

// updateLocalCredentials merges the caller-provided identity
// fields into ~/.enju/credentials.json via a read-modify-write
// pass that preserves unknown fields. Fields not provided by
// the caller (haveName/haveEmail false) are left untouched, so
// update_profile(name=X) doesn't silently clear a previously-
// set email on disk.
func updateLocalCredentials(haveName bool, name string, haveEmail bool, email string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := home + "/.enju/credentials.json"
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var creds map[string]interface{}
	if json.Unmarshal(data, &creds) != nil {
		return
	}
	if haveName {
		creds["name"] = name
	}
	if haveEmail {
		creds["email"] = email
	}
	updated, _ := json.MarshalIndent(creds, "", "  ")
	os.WriteFile(path, updated, 0600)
}

// formatJSON pretty-prints a JSON byte blob with 2-space
// indentation. Used by tool handlers that pass through raw
// coordinator responses.
func formatJSON(data []byte) string {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, data, "", "  "); err != nil {
		return string(data)
	}
	return pretty.String()
}
