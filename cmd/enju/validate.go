package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
)

// cmdValidate parses a workflow YAML and reports parse errors +
// author warnings. No coord, no credentials, no side effects —
// safe to run in CI against arbitrary YAML files.
//
// Exit codes:
//
//	0  YAML parsed cleanly (warnings ignored unless --strict)
//	2  bad usage / missing args
//	4  parse failed, or --strict and warnings present
func cmdValidate(args []string) {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	strict := fs.Bool("strict", false, "Treat warnings as failures (CI mode)")
	asJSON := fs.Bool("json", false, "Emit a structured report instead of human output")
	fs.Parse(args)

	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: enju validate <workflow.yaml> [--strict] [--json]")
		os.Exit(2)
	}

	exit := 0
	for _, path := range fs.Args() {
		ok := validateOne(path, *strict, *asJSON)
		if !ok && exit < 4 {
			exit = 4
		}
	}
	os.Exit(exit)
}

type validateReport struct {
	Path     string   `json:"path"`
	OK       bool     `json:"ok"`
	Error    string   `json:"error,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	// RepoAdvisory: non-fatal git-state notes (parked on a run
	// branch, uncommitted workflow). Separate from the workflow
	// verdict — never affects OK or the exit code.
	RepoAdvisory []string `json:"repo_advisory,omitempty"`
}

// validateOne returns true when the YAML at path is acceptable
// under the chosen strictness. Side-effects are stdout writes
// (human or JSON depending on asJSON); the bool is for the
// caller's exit-code aggregation.
func validateOne(path string, strict, asJSON bool) bool {
	rep := validateReport{Path: path}

	data, readErr := enjuYaml.FlattenFile(path)
	if readErr != nil {
		rep.Error = readErr.Error()
		emitReport(rep, asJSON)
		return false
	}

	parsed, parseErr := enjuYaml.Parse(data)
	if parseErr != nil {
		rep.Error = parseErr.Error()
		emitReport(rep, asJSON)
		return false
	}

	rep.Warnings = parsed.Warnings
	hasWarnings := len(rep.Warnings) > 0
	rep.OK = !(strict && hasWarnings)
	// Best-effort env advisory — the workflow is fine; this is
	// purely about the git state a run would execute against.
	// Never feeds rep.OK / the exit code.
	rep.RepoAdvisory = repoAdvisory(path)
	emitReport(rep, asJSON)
	return rep.OK
}

func emitReport(r validateReport, asJSON bool) {
	if asJSON {
		b, _ := json.MarshalIndent(r, "", "  ")
		fmt.Println(string(b))
		return
	}
	rel := r.Path
	if abs, err := filepath.Abs(r.Path); err == nil {
		if cwd, cwdErr := os.Getwd(); cwdErr == nil {
			if rp, relErr := filepath.Rel(cwd, abs); relErr == nil && len(rp) < len(r.Path) {
				rel = rp
			}
		}
	}
	if r.Error != "" {
		fmt.Printf("✗ %s\n  %s\n", rel, r.Error)
		return
	}
	// Glyph tracks the actual verdict (r.OK), not just "did it
	// parse". Under -strict a warning-bearing file FAILS (exit 4
	// and ok:false in -json), so the human glyph must be ✗ too —
	// printing ✓ while exiting non-zero was a silent self-
	// contradiction only visible via `echo $?`.
	if !r.OK {
		fmt.Printf("✗ %s\n", rel)
		for _, w := range r.Warnings {
			fmt.Printf("  ⚠ %s\n", w)
		}
		fmt.Printf("  ✗ failed: -strict treats the warning(s) above as errors\n")
		return
	}
	fmt.Printf("✓ %s\n", rel)
	for _, w := range r.Warnings {
		fmt.Printf("  ⚠ %s\n", w)
	}
	// Environment advisory — visually separated from the workflow
	// verdict above so a clean ✓ is never muddied. Non-fatal.
	if len(r.RepoAdvisory) > 0 {
		fmt.Printf("\n⚠ environment (workflow is valid; this is your git state):\n")
		for _, a := range r.RepoAdvisory {
			fmt.Printf("  - %s\n", a)
		}
	}
}
