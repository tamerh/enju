package git

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// RebaseOnRemote runs `git pull --rebase --autostash origin <branch>`
// via the system git binary so divergent histories are merged
// without discarding local commits. go-git's rebase support is
// too limited for divergent-history replays which is exactly the
// case we hit when the user has committed between submits.
//
// --autostash protects against an edge case where the caller left
// uncommitted changes in the worktree; they get stashed and
// re-applied around the rebase so we never surprise the user with
// a dirty-tree rejection.
//
// Empty branch falls through to the caller's responsibility (does
// not resolve to default here — callers at Workflow level pre-
// resolve via Conventions).
//
// No-op when remoteURL is empty (local-only project — there's
// nothing to rebase against).
//
// Caller MUST hold the project lock.
func (c *Clone) RebaseOnRemote(branch string) error {
	defer c.lock()()
	if c.remoteURL == "" {
		return nil
	}
	if branch == "" {
		return fmt.Errorf("git: rebase-on-remote: branch is required")
	}
	cmd := exec.Command("git", "-C", c.workDir, "pull", "--rebase", "--autostash", "origin", branch)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git pull --rebase origin %s: %s (%w)", branch, strings.TrimSpace(string(out)), err)
	}
	return nil
}
