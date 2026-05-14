package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
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
	fs.Parse(args)

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

	entry, err := resolveStatusProject(sess, *projectID)
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
			fmt.Println("\nRender: paste at https://mermaid.live, or pipe with `enju dag", detail.Run.Seq, "-format=mermaid | mmdc -i - -o dag.png` (npm i -g @mermaid-js/mermaid-cli)")
		}
	default:
		fmt.Fprintf(os.Stderr, "dag: unknown --format %q (use default, mermaid, or json)\n", *format)
		os.Exit(2)
	}
}
