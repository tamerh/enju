package gitcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Default timeouts. Local ops (rev-parse, log, etc.) are
// CPU-bound and should complete in milliseconds; the generous
// 60s ceiling exists only to catch wedged-process anomalies.
// Network ops (fetch, push, ls-remote) are sized for slow
// connections to remote SSH/HTTPS hosts.
const (
	defaultLocalTimeout   = 60 * time.Second
	defaultNetworkTimeout = 5 * time.Minute
)

// MinGitMajor / MinGitMinor — the floor we require from the
// system `git` binary. Set by the most-recent feature we
// depend on: `git merge-tree --write-tree --name-only` (used
// by MergeWithCommit) needs git 2.40 (Mar 2023). Older builds
// (RHEL 7 ships 1.8; Ubuntu 18.04 ships 2.17; old macOS Xcode
// ships 2.30-ish) will fail the conflict-detection verb with
// "unknown option --name-only" and no other useful signal.
//
// Documenting the minimum here AND checking it at startup
// turns a cryptic exec failure into a clear "upgrade git"
// message.
const (
	MinGitMajor = 2
	MinGitMinor = 40
)

// CheckMinVersion runs `git --version` and verifies the binary
// is at least MinGitMajor.MinGitMinor. Returns a human-readable
// error suitable for the operator's stderr — names the minimum
// and the verb that requires it so they know why.
//
// Designed to be called once at startup by every binary that
// exercises gitcli verbs (enju daemon, fat-client commands,
// webui). One probe, fail-fast, no surprises later.
func CheckMinVersion() error {
	out, err := runGit("", []string{"--version"}, runOpts{})
	if err != nil {
		return fmt.Errorf("gitcli: cannot run `git --version` (need git ≥ %d.%d on PATH): %w",
			MinGitMajor, MinGitMinor, err)
	}
	major, minor, parseErr := parseGitVersion(string(out))
	if parseErr != nil {
		return fmt.Errorf("gitcli: unable to parse git version output %q: %w", out, parseErr)
	}
	if major < MinGitMajor || (major == MinGitMajor && minor < MinGitMinor) {
		return fmt.Errorf("gitcli: git %d.%d found, need ≥ %d.%d (required by merge-tree --write-tree --name-only, used for conflict-aware merges)",
			major, minor, MinGitMajor, MinGitMinor)
	}
	return nil
}

// parseGitVersion extracts the major + minor from `git
// --version` output. Format is famously stable:
//
//	"git version 2.43.0"
//	"git version 2.30.2 (Apple Git-129)"   // macOS suffix
//	"git version 2.39.2.windows.1"         // Windows suffix
//
// We split on whitespace, take the third token (after "git
// version"), then split on "." for major + minor. Patch /
// suffix bits are ignored.
func parseGitVersion(out string) (major, minor int, err error) {
	fields := strings.Fields(out)
	if len(fields) < 3 || fields[0] != "git" || fields[1] != "version" {
		return 0, 0, fmt.Errorf("unexpected format")
	}
	parts := strings.SplitN(fields[2], ".", 3)
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("version string %q lacks minor", fields[2])
	}
	major, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("major: %w", err)
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("minor: %w", err)
	}
	return major, minor, nil
}

// runOpts carries the optional invocation-time knobs for runGit.
// Zero value is the default: workDir as CWD, no extra env, local
// timeout, no stdin.
type runOpts struct {
	// extraEnv is appended to os.Environ() (later wins, so callers
	// can override). Use this for GIT_AUTHOR_*/GIT_COMMITTER_*/
	// GIT_DIR / GIT_WORK_TREE etc.
	extraEnv []string

	// stdin is piped to git's stdin (e.g. update-ref --stdin,
	// hash-object -w --stdin). nil = no stdin.
	//
	// Type is []byte (not io.Reader) because every current
	// caller already has its content in memory: PlumbingCommit
	// passes FileWrite.Content which the caller built up from
	// task-result artifacts (typically <1 MB). A multi-GB
	// hash-object stream would materialize the whole input in
	// memory here, but that's not a workload enju produces. If
	// streaming ever becomes necessary, switch this field to
	// io.Reader and use cmd.Stdin = r directly — the rest of
	// runGit is reader-friendly.
	stdin []byte

	// timeout overrides the per-call timeout. Zero = default.
	timeout time.Duration

	// network=true picks defaultNetworkTimeout when timeout is
	// zero. No-op when timeout is set explicitly.
	network bool
}

// runGit is the single subprocess chokepoint for gitcli. Every
// git invocation lands here, which is why timeout setup, env-var
// plumbing, stdin handling, and stderr → typed-error mapping all
// live in this one place.
//
// workDir is passed via `git -C <workDir>` rather than os.Chdir
// or cmd.Dir, so concurrent calls from different goroutines on
// the same process can safely target different repos without
// fighting over the process's CWD. cmd.Dir = workDir is also set
// (belt-and-suspenders for git versions that ignore -C in some
// contexts).
//
// Returns stdout on success. On failure, returns a wrapped error
// that may be one of the typed sentinels in errors.go (matched
// via classifyStderr); the raw stderr is appended for diagnostics
// so callers can log a useful message without further parsing.
//
// Concurrency: runGit holds no shared state; multiple goroutines
// can call it concurrently on the same Clone. The Clone's lock
// (when held) is around higher-level verbs, not around runGit
// itself.
func runGit(workDir string, args []string, opts runOpts) ([]byte, error) {
	timeout := opts.timeout
	if timeout == 0 {
		if opts.network {
			timeout = defaultNetworkTimeout
		} else {
			timeout = defaultLocalTimeout
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Build argv: `git -C <workDir> <args...>`. The -C flag tells
	// git to chdir before doing anything, so all relative-path
	// behavior matches what an operator running the command from
	// workDir would see.
	full := make([]string, 0, len(args)+2)
	if workDir != "" {
		full = append(full, "-C", workDir)
	}
	full = append(full, args...)

	cmd := exec.CommandContext(ctx, "git", full...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	// Force C locale for every invocation. classifyStderr does
	// English substring matching on git's error output;
	// LANG=fr_FR.UTF-8 or other locales translate those messages
	// and break classification silently. LC_ALL overrides every
	// LC_* var including LANG, so this is the bulletproof
	// version. Caller-supplied extraEnv is appended after, so
	// callers can still override (e.g. tests that want a
	// localized environment for explicit i18n testing).
	baseEnv := append(cmd.Environ(), "LC_ALL=C", "LANG=C")
	if len(opts.extraEnv) > 0 {
		cmd.Env = append(baseEnv, opts.extraEnv...)
	} else {
		cmd.Env = baseEnv
	}
	if opts.stdin != nil {
		cmd.Stdin = bytes.NewReader(opts.stdin)
	}
	// WaitDelay: when ctx fires and the process is SIGKILL'd,
	// give cmd.Run's I/O-copy goroutines 5s to wind down before
	// Run returns. Without this, a hung pipe (e.g. a network
	// stall on the stderr drain) can leave Run blocked forever
	// after the process is dead. Same hardening pattern used by
	// the claude-subprocess wrapper elsewhere in the codebase.
	cmd.WaitDelay = 5 * time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}

	// On failure we STILL return stdout alongside the error.
	// Some git verbs write meaningful output even on non-zero
	// exit (notably `merge-tree --write-tree` on conflict: it
	// exits 1 with the merged-tree SHA + conflict paths on
	// stdout). Callers that need to inspect partial output are
	// careful with the err — typical callers just see nil bytes
	// because they early-return on err != nil.
	out := stdout.Bytes()

	// Timeout takes priority over exit-status classification —
	// the exit-code came from the killed process, not from git's
	// own error path.
	if ctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("git: %s timed out after %s",
			joinForLog(full), timeout)
	}

	// stderr → typed error. Returns nil when no pattern matched,
	// in which case we fall through to a generic wrap.
	if typed := classifyStderr(stderr.String()); typed != nil {
		return out, fmt.Errorf("%w: git %s: %s",
			typed, joinForLog(full), trimStderr(stderr.String()))
	}

	// Unmatched failure — surface the raw stderr. Wrap with %w
	// so callers that need the underlying exit code (e.g.
	// merge.go distinguishing exit-1 "conflict" from exit-128
	// "real error") can recover *exec.ExitError via errors.As.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out, fmt.Errorf("git %s exit %d: %s: %w",
			joinForLog(full), exitErr.ExitCode(), trimStderr(stderr.String()), err)
	}
	return out, fmt.Errorf("git %s: %w (%s)",
		joinForLog(full), err, trimStderr(stderr.String()))
}

// classifyStderr maps known git error messages to typed sentinels.
// Returns nil when no pattern matched — the caller wraps the raw
// stderr generically. Patterns are case-insensitive and matched
// as substrings; git's error messages are famously stable, so
// substring matching is robust in practice.
//
// New patterns get added here, not scattered into individual verbs.
// Keep verb code clean: the verb decides which sentinel it cares
// about, runGit just hands them up.
func classifyStderr(stderr string) error {
	s := strings.ToLower(stderr)
	switch {
	case (strings.Contains(s, "[rejected]") &&
		(strings.Contains(s, "fetch first") ||
			strings.Contains(s, "non-fast-forward"))),
		strings.Contains(s, "non-fast-forward"),
		strings.Contains(s, "non fast-forward"):
		// Modern git rejects non-FF pushes with "[rejected]
		// main -> main (fetch first)"; older messages use the
		// literal "non-fast-forward". Distinct from hook
		// rejects, which produce "[remote rejected]" with
		// custom reason text — those fall through to the
		// generic error wrap.
		return ErrPushNonFF
	case strings.Contains(s, "unknown revision or path not in the working tree"),
		strings.Contains(s, "bad revision"),
		strings.Contains(s, "not a valid object name"),
		strings.Contains(s, "bad object "),
		strings.Contains(s, "no such commit"):
		// rev-parse / show / cat-file on a missing SHA all surface
		// one of these. Caller (ReadFileAtCommit, ResolveRef)
		// decides whether to treat as commit-not-found or
		// ref-not-found based on the input shape.
		return ErrCommitNotFound
	case strings.Contains(s, "pathspec") && strings.Contains(s, "did not match"):
		return ErrRefNotFound
	case strings.Contains(s, "reference already exists"):
		// git update-ref refs/heads/X <sha> "" on an existing ref
		// surfaces as this — exactly the CreateBranchAt "refuse
		// duplicate" path.
		return ErrBranchExists
	case strings.Contains(s, "does not appear to be a git repository"),
		strings.Contains(s, "repository not found"),
		strings.Contains(s, "could not read from remote repository"):
		// Remote URL invalid / unreachable / not a real repo.
		// All three messages mean the same thing operationally
		// (no upstream to talk to) and map to the same typed
		// sentinel.
		return ErrRemoteNotFound
	}
	return nil
}

// trimStderr collapses a multi-line stderr blob into one line for
// log output. Preserves the first non-empty line (where git puts
// the human-readable cause) and elides the rest with "[...]".
func trimStderr(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return "(no stderr)"
	}
	if idx := strings.IndexByte(stderr, '\n'); idx >= 0 {
		return stderr[:idx] + " [...]"
	}
	return stderr
}

// joinForLog renders argv as a roughly-shell-quoted string for
// error messages. Not safe for actual shell execution — we never
// shell out via /bin/sh — but readable for diagnostics.
func joinForLog(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if strings.ContainsAny(a, " \t\n\"'") {
			parts[i] = fmt.Sprintf("%q", a)
		} else {
			parts[i] = a
		}
	}
	return strings.Join(parts, " ")
}
