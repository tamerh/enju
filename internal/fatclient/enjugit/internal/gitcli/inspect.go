package gitcli

// inspect.go — package-level helpers for project-adoption flows
// (the enju_create_project / EagerInitProjectClone path).
//
// Distinct from Clone-method ops because these are called
// BEFORE a Workspace/Workflow exists — they inspect or
// initialize a bare directory on disk. Replaces the
// project_ops.go go-git/v5 call-sites so production code never
// imports go-git directly.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GitDirInfo is the result of InspectGitDir: the origin URL
// (empty when no origin) and the current branch name
// (refs/heads/<short>) (empty when HEAD is detached or
// unresolvable).
type GitDirInfo struct {
	OriginURL     string
	DefaultBranch string
}

// InspectGitDir reads the on-disk state of a non-bare git
// repo: origin URL + HEAD branch name. Used by project-
// adoption to decide whether a path has a usable git repo
// already and what its current configuration is.
//
// Returns an error only when the path can't be opened as a
// git repo at all (no .git, corrupt config). Missing origin
// or detached HEAD are non-errors — the returned struct just
// has empty strings for those fields.
func InspectGitDir(path string) (GitDirInfo, error) {
	info := GitDirInfo{}
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		return info, fmt.Errorf("git: stat %s/.git: %w", path, err)
	}
	// Origin URL — missing origin is fine, just leave empty.
	if out, err := runGit(path, []string{"remote", "get-url", "origin"}, runOpts{}); err == nil {
		info.OriginURL = trimTrailingNewline(out)
	}
	// HEAD branch — detached / unborn HEAD is fine, just empty.
	if out, err := runGit(path, []string{"symbolic-ref", "--quiet", "--short", "HEAD"}, runOpts{}); err == nil {
		info.DefaultBranch = trimTrailingNewline(out)
	}
	return info, nil
}

// HeadResolvesToCommit returns true when HEAD in the repo at
// path resolves to a real commit (i.e. the repo has at least
// one commit). False when no git repo, unborn HEAD, or any
// other unresolvable state. Used by adoption's
// "is this a populated unrelated repo?" probe.
func HeadResolvesToCommit(path string) bool {
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		return false
	}
	_, err := runGit(path, []string{"rev-parse", "--verify", "-q", "HEAD"}, runOpts{})
	return err == nil
}

// InitLocalAdoptExisting runs `git init` on a directory that
// already has files in it, ensures the enju/ scaffold is
// present, then stages every file + commits with the Enju
// identity. Returns the short branch name of HEAD after the
// commit.
//
// Distinct from InitLocal (which writes a fresh README + commits
// only the scaffold). This one is called when the operator's
// directory already has content — `git add .` captures it all
// as the project's initial commit, instead of shadowing it
// with a generic README.
//
// templatesRelDir is the repo-relative path to the templates
// directory (typically "enju/templates"). The function ensures
// templatesRelDir/.gitkeep exists before staging so the
// scaffold lands on commit #1 along with the existing files.
func InitLocalAdoptExisting(workDir, templatesRelDir string) (defaultBranch string, err error) {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return "", fmt.Errorf("git: mkdir %s: %w", workDir, err)
	}
	if _, err := runGit(workDir, []string{"init", "-b", "main"}, runOpts{}); err != nil {
		return "", fmt.Errorf("git init: %w", err)
	}
	// Ensure the enju/ scaffold exists. Idempotent — pre-existing
	// .gitkeep is fine.
	if templatesRelDir != "" {
		templatesDir := filepath.Join(workDir, templatesRelDir)
		if _, err := os.Stat(templatesDir); os.IsNotExist(err) {
			if err := os.MkdirAll(templatesDir, 0o755); err != nil {
				return "", fmt.Errorf("create templates dir: %w", err)
			}
			gitkeep := filepath.Join(templatesDir, ".gitkeep")
			if err := os.WriteFile(gitkeep, nil, 0o644); err != nil {
				return "", fmt.Errorf("write .gitkeep: %w", err)
			}
		}
	}
	// Stage everything (`git add .`). gitignore-aware: anything
	// the user has gitignored in a prior init won't get staged
	// here — git doesn't know about pre-existing .gitignore
	// files since this is the first init, BUT a .gitignore that
	// lives in the directory IS read on this very `add`.
	if _, err := runGit(workDir, []string{"add", "."}, runOpts{}); err != nil {
		return "", fmt.Errorf("staging files: %w", err)
	}
	// Detect no-op: if the only file is .git/ and nothing got
	// staged, commit would error. Probe via diff --cached.
	if _, err := runGit(workDir, []string{"diff", "--cached", "--quiet"}, runOpts{}); err == nil {
		// Nothing staged — return early without committing.
		return "main", nil
	}
	env := authorEnvVars("Enju", "enju@localhost", time.Now())
	if _, err := runGit(workDir, []string{"commit", "-m", "Initialize Enju orchestration"}, runOpts{extraEnv: env}); err != nil {
		return "", fmt.Errorf("initial commit: %w", err)
	}
	// Read current branch.
	if out, err := runGit(workDir, []string{"symbolic-ref", "--quiet", "--short", "HEAD"}, runOpts{}); err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	return "main", nil
}
