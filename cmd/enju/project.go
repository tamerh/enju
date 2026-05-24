package main

import (
	"context"
	"flag"
	"fmt"
	"os"
)

// cmdProject groups project-settings verbs that were previously
// reachable only over MCP. Subcommands resolve the active project
// (--project id, or the project owning the current directory) and
// drive the same FatClient methods the MCP handlers call, so a
// CLI-only operator isn't forced through an MCP host for routine
// project administration.
func cmdProject(args []string) {
	if len(args) < 1 {
		printProjectUsage()
		os.Exit(1)
	}
	switch args[0] {
	case "default-branch":
		cmdProjectDefaultBranch(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown project subcommand: %s\n", args[0])
		printProjectUsage()
		os.Exit(1)
	}
}

func printProjectUsage() {
	fmt.Println(`Usage: enju project <subcommand> [args]

Subcommands:
  default-branch <branch>   Set the project's default branch (the branch runs
                            fork from). Owner-only. Mirrors the MCP tool
                            enju_set_project_default_branch.

Flags (default-branch):
  --project <id>   Operate on a specific project id (default: the project
                   that owns the current directory).
  --coordinator <url>   Coordinator URL (default: from credentials.json).`)
}

// cmdProjectDefaultBranch is `enju project default-branch <branch>`.
// Thin CLI mirror of enju_set_project_default_branch:
// FatClient.SetProjectDefaultBranch updates the coordinator-side
// default and materializes the branch locally so subsequent runs can
// fork from it. Owner-only is enforced coordinator-side.
func cmdProjectDefaultBranch(args []string) {
	fs := flag.NewFlagSet("project default-branch", flag.ExitOnError)
	projectID := fs.Int64("project", 0, "Override project resolution (numeric id)")
	coordOverride := fs.String("coordinator", "", "Coordinator URL (default: from credentials.json)")
	fs.Parse(args)

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: enju project default-branch <branch> [--project id]")
		os.Exit(2)
	}
	branch := fs.Arg(0)

	sess := openCLISession(*coordOverride)

	projID, warning, err := runProjectDefaultBranch(sess, *projectID, branch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "project default-branch: %v\n", err)
		// Resolution failures (bad/unregistered --project, no
		// registry) are usage errors (exit 2); a coord-side failure
		// is exit 1. resolveActiveProject is the only exit-2 source.
		if projID == 0 {
			os.Exit(2)
		}
		os.Exit(1)
	}
	fmt.Printf("✓ project %d default branch set to %q\n", projID, branch)
	if warning != "" {
		// Non-fatal: the coord update landed; only the local
		// materialize step had trouble (e.g. forking a brand-new
		// branch). Surface it so the operator knows the local clone
		// may need attention before the next run forks cleanly.
		fmt.Fprintf(os.Stderr, "  ⚠ %s\n", warning)
	}
}

// runProjectDefaultBranch resolves the target project and sets its
// default branch. Split from cmdProjectDefaultBranch (which owns flag
// parsing, printing, and exit codes) so the resolve-then-set logic is
// testable without os.Exit. Returns the resolved project id (0 when
// resolution itself failed — the usage-error vs coord-error
// discriminator the caller uses for its exit code).
func runProjectDefaultBranch(sess *cliSession, projectID int64, branch string) (int64, string, error) {
	entry, err := resolveActiveProject(sess, projectID)
	if err != nil {
		return 0, "", err
	}
	warning, err := sess.FC.SetProjectDefaultBranch(context.Background(), entry.ID, branch)
	return entry.ID, warning, err
}
