package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// cmdDag renders a run's DAG. Three output formats:
//
//   mermaid — flowchart TD source for pasting into mermaid.live,
//             README files, the preprint. Reuses the same
//             format.RenderMermaidBody renderer the MCP
//             enju_run_status --format=mermaid path uses, so
//             output stays byte-identical between transports.
//   json    — { run, tasks, diagram_mermaid } from
//             FatClient.GetRun. Useful for piping into other
//             tools (jq, gh) that want structured shape.
//   default — short summary: id, state, task count, branch, +
//             the mermaid block as a hint. Operator-friendly
//             when you just want a quick look without an
//             external renderer.
//
// Name choice (dag vs graph): the codebase consistently calls
// these things DAGs — internal/common/dag/, "the DAG" in docs,
// Snakemake/Nextflow use the same term. `graph` is overloaded
// (call graph, dep graph, social graph); `dag` reads as the
// specific thing.
//
// ASCII rendering is deferred; the existing internal/common/dag
// package doesn't ship one, and the mermaid path covers the
// "show me the shape" need today.
func cmdDag(args []string) {
	fs := flag.NewFlagSet("dag", flag.ExitOnError)
	projectID := fs.Int64("project", 0, "Override project resolution (numeric id)")
	format := fs.String("format", "default",
		"Output format: default, mermaid, or json. mermaid emits raw flowchart TD; see the default-mode output for render hints (mermaid.live / mmdc / GitHub).")
	coordOverride := fs.String("coordinator", "", "Coordinator URL (default: from credentials.json)")
	// Go's flag package stops parsing at the first non-flag arg,
	// so `enju dag 15 --format json` (the command's own
	// documented positional-first syntax) would leave --format at
	// its default and fail NArg. Hoist flags ahead of positionals
	// before Parse so flag order doesn't matter — the documented
	// invocation just works (bug hunt B-6).
	fs.Parse(hoistFlagsBeforePositionals(args))

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: enju dag <run-seq> [--project N] [--format mermaid|json]")
		os.Exit(2)
	}
	runSeqStr := fs.Arg(0)
	runSeq, err := strconv.Atoi(runSeqStr)
	if err != nil || runSeq <= 0 {
		fmt.Fprintf(os.Stderr, "dag: run seq must be a positive integer, got %q\n", runSeqStr)
		os.Exit(2)
	}

	sess := openCLISession(*coordOverride)
	ctx := context.Background()

	entry, err := resolveActiveProject(sess, *projectID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dag: %v\n", err)
		os.Exit(2)
	}

	detail, err := sess.FC.GetRun(ctx, entry.ID, runSeq)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dag: %v\n", err)
		os.Exit(1)
	}

	switch *format {
	case "mermaid":
		// Raw mermaid source — pipe-friendly. No outer header,
		// no trailing prompt, no fences. Operator who pipes to
		// mermaid-cli or a file gets exactly what they need.
		fmt.Println(detail.DiagramMermaid)
	case "json":
		b, _ := json.MarshalIndent(detail, "", "  ")
		fmt.Println(string(b))
	case "default", "":
		fmt.Printf("Run #%d  %s  state=%s  tasks=%d  branch=%s\n",
			detail.Run.Seq, detail.Run.Name, detail.Run.State,
			detail.Run.TaskCount, detail.Run.Branch)
		if detail.DiagramMermaid != "" {
			fmt.Println("\nDAG (mermaid):")
			fmt.Println("```mermaid")
			fmt.Println(detail.DiagramMermaid)
			fmt.Println("```")
			// Render hint. The operator who runs `enju dag N`
			// for orientation may not know what to do with a
			// mermaid block — point at the cheapest path
			// (mermaid.live, no install) and the local-render
			// option (mmdc) for those who'd rather pipe.
			fmt.Println("\nRender: paste at https://mermaid.live, or pipe with `enju dag", detail.Run.Seq, "--format=mermaid | mmdc -i - -o dag.png` (npm i -g @mermaid-js/mermaid-cli)")
		}
	default:
		fmt.Fprintf(os.Stderr, "dag: unknown --format %q (use default, mermaid, or json)\n", *format)
		os.Exit(2)
	}
}

// hoistFlagsBeforePositionals reorders argv so every flag (and
// the value it consumes) precedes every positional, leaving the
// relative order within each group intact. This makes Go's
// stop-at-first-positional flag parser accept the documented
// positional-first invocation `enju dag <seq> --format json`
// (bug hunt B-6) — without it, any flag after the run-seq is
// silently dropped and the command exits 2 on its own usage
// string.
//
// Value-vs-bool: dag's flags (--project, --format, --coordinator)
// all take a value, so a bare `--format` consumes the next token.
// The list is kept here (not derived from the FlagSet) so the
// rule is obvious at the call site and this helper stays a pure
// arg-slice transform with no flag-package coupling — important
// because cmd/enju/main.go's own arg handling is owned by another
// concern; this stays a local, dag-scoped reorder.
//
// `--` ends flag processing per convention: everything after it
// is treated as a positional verbatim (so a literal arg that
// looks like a flag still works).
func hoistFlagsBeforePositionals(args []string) []string {
	valueFlags := map[string]bool{
		"-project": true, "--project": true,
		"-format": true, "--format": true,
		"-coordinator": true, "--coordinator": true,
	}
	var flagsPart, posPart []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			// End of flags by convention: everything after is a
			// positional, verbatim.
			posPart = append(posPart, args[i+1:]...)
			break
		}
		if len(a) > 1 && a[0] == '-' {
			flagsPart = append(flagsPart, a)
			// `--flag=value` is self-contained. A bare
			// value-taking flag consumes the following token.
			if !strings.Contains(a, "=") && valueFlags[a] && i+1 < len(args) {
				flagsPart = append(flagsPart, args[i+1])
				i++
			}
			continue
		}
		posPart = append(posPart, a)
	}
	return append(flagsPart, posPart...)
}
