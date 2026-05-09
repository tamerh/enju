package enjugit

// Bigfiles directory resolution — the per-project, per-branch
// location where action:compute tasks land their declared-untracked
// outputs (writes_artifacts entries with track:false). Lives next
// to .clone/ and .bare.git/ inside <project>/enju/, so the data
// never enters the worktree and git literally can't see it.

import (
	"fmt"
	"os"
	"path/filepath"

	corelayout "github.com/enju-ai/enju/internal/common/layout"
)

// BigfilesEnv is the env var citizens can set to redirect bigfiles
// storage to a shared filesystem (NFS / Lustre / etc) so multiple
// operators on a cluster share the same artifact bytes without
// per-operator copies. Unset = local-only, in the project tree.
//
// When set, the value is the SHARED ROOT — per-project /
// per-branch subdirs are appended underneath. Unset, the
// equivalent layout lives at <project>/enju/bigfiles/.
//
// Same name on the script-side env (the wrapper passes it
// through verbatim) so a recipe author can read it directly
// when they need an absolute path for tools that won't do
// relative resolution.
const BigfilesEnv = "ENJU_BIGFILES"

// ResolveBigfilesDir returns the absolute directory where this
// project + branch's untracked artifacts live.
//
//   - When ENJU_BIGFILES is set in env, the path is
//     <env>/<project-slug>-<id>/<branch>/  — same shape as
//     SharedArtifactPath today, so an operator who set
//     ENJU_BIGFILES on a shared mount sees per-project,
//     per-branch sharding for free.
//
//   - When unset, the path is
//     <projectRoot>/enju/bigfiles/<branch>/ — a sibling of
//     .clone/ and .bare.git/, inside the project tree but
//     outside the worktree.
//
// Either way the caller can `os.MkdirAll(dir, 0755)` and write
// `<dir>/<repoRelPath>` for any declared track:false output
// path.
//
// projectRoot is the enclosing <workspace>/<project>/ dir.
// projectName is the user-facing slug (used only in the
// shared-mount case for the per-project subdir name).
// projectID is the coordinator's numeric id, suffixed onto
// the slug so two projects with the same name don't collide.
// branch falls back to "main" via BigfilesBranchDir.
func ResolveBigfilesDir(projectRoot string, projectID int64, projectName, branch string) string {
	if branch == "" {
		branch = "main"
	}
	if shared := os.Getenv(BigfilesEnv); shared != "" {
		abs, err := filepath.Abs(shared)
		if err == nil {
			return filepath.Join(abs, projectDirSegment(projectID, projectName), branch)
		}
	}
	// Local default: sibling of .clone/ inside the project tree.
	if projectRoot == "" {
		return ""
	}
	return filepath.Join(projectRoot, corelayout.BigfilesBranchDir(branch))
}

// projectDirSegment renders the <slug>-<id> path component used
// to namespace one project's bigfiles within a shared root.
// Empty projectName falls back to numeric-id naming, parity with
// Workspace.projectDir on the local layout.
func projectDirSegment(projectID int64, projectName string) string {
	if projectName == "" {
		return fmt.Sprintf("%d", projectID)
	}
	slug := slugify(projectName)
	if slug == "" {
		return fmt.Sprintf("%d", projectID)
	}
	return fmt.Sprintf("%s-%d", slug, projectID)
}
