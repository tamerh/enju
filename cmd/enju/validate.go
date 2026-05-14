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
//   0  YAML parsed cleanly (warnings ignored unless --strict)
//   2  bad usage / missing args
//   4  parse failed, or --strict and warnings present
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
}

// validateOne returns true when the YAML at path is acceptable
// under the chosen strictness. Side-effects are stdout writes
// (human or JSON depending on asJSON); the bool is for the
// caller's exit-code aggregation.
func validateOne(path string, strict, asJSON bool) bool {
	rep := validateReport{Path: path}

	data, readErr := os.ReadFile(path)
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
	fmt.Printf("✓ %s\n", rel)
	for _, w := range r.Warnings {
		fmt.Printf("  ⚠ %s\n", w)
	}
}
