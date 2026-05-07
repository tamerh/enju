package service

// Named outputs with file specs — the file-layout side of a task's
// `outputs:` schema. Lives in service because the work is pure JSON
// parsing + filename assembly: no git operation, no project state.
// Submit drives both functions to materialize a task's outputs into
// the task result directory and build the `output_files` index that
// downstream `{{task.field}}` references resolve through.
//
// Tasks may declare an `outputs:` schema in their YAML:
//
//   outputs:
//     gene_list:
//       description: "Top-scoring genes as CSV"
//       file: genes.csv
//       format: csv
//     pathways:
//       description: "Pathway graph as JSON"
//       file: pathways.json
//       format: json
//     summary:
//       description: "Human-readable summary"
//
// At submit time each named output gets its own file under the task
// result directory. Outputs with a `file:` spec use that exact
// filename; outputs without one fall back to `{name}.{format}`
// (default format `md`). The metadata.json for the submission gains
// an `output_files` index so downstream template references like
// `{{task.gene_list}}` can find the right file via the index.
//
// Ported from the legacy coordinator-side writeMultiFileResult in
// internal/api/files.go during the iteration A orchestrator rewrite.

import (
	"encoding/json"
	"path/filepath"

	"github.com/enju-ai/enju/internal/fatclient/enjugit"
)

// NamedOutputSpec describes one named output's file layout. Mirrors
// enjuYaml.OutputSpec but lives here so clients don't need to
// import the YAML parser package.
type NamedOutputSpec struct {
	Description string
	File        string
	Format      string
}

// ParseNamedOutputSchema deserializes a task's outputs JSON (as
// stored in tasks.outputs by the coordinator) into a map of name
// to spec. Returns nil for empty or malformed input — callers
// should treat nil as "no schema, fall back to the single-file
// outputs path".
//
// The JSON shape the coordinator persists is:
//
//	{
//	  "gene_list": {"Description": "...", "File": "genes.csv", "Format": "csv"},
//	  "pathways":  {"Description": "...", "File": "pathways.json", "Format": "json"}
//	}
//
// Uppercase field names because Go's encoding/json defaults to the
// struct field name when there are no json tags (the YAML parser
// uses yaml tags but no json tags on OutputSpec).
func ParseNamedOutputSchema(schemaJSON string) map[string]NamedOutputSpec {
	if schemaJSON == "" {
		return nil
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(schemaJSON), &raw); err != nil {
		return nil
	}
	if len(raw) == 0 {
		return nil
	}
	schema := make(map[string]NamedOutputSpec, len(raw))
	for name, v := range raw {
		switch val := v.(type) {
		case string:
			// Short-form outputs (`name: "description"`) — no file
			// spec, no format. Render as name.md by default.
			schema[name] = NamedOutputSpec{Description: val}
		case map[string]interface{}:
			spec := NamedOutputSpec{}
			if d, ok := val["Description"].(string); ok {
				spec.Description = d
			}
			if f, ok := val["File"].(string); ok {
				spec.File = f
			}
			if fmt, ok := val["Format"].(string); ok {
				spec.Format = fmt
			}
			schema[name] = spec
		}
	}
	return schema
}

// BuildNamedOutputFiles constructs the FileWrite list for a named-
// outputs submission. Each output gets its own file under
// `resultDir/`, with the filename chosen from the schema's `file:`
// spec if present or built from `{name}.{format}` otherwise. Also
// returns the `output_files` index (output name → on-disk filename)
// that the caller should embed in metadata.json so downstream tasks
// can resolve `{{task.field_name}}` references via the index.
//
// Does NOT build metadata.json itself — the caller owns the full
// metadata map and should add the returned fileIndex to it under
// the `output_files` key before writing it.
//
// Ported from the legacy coordinator-side writeMultiFileResult.
func BuildNamedOutputFiles(resultDir string, schema map[string]NamedOutputSpec, values map[string]string) (files []enjugit.FileWrite, fileIndex map[string]string) {
	fileIndex = make(map[string]string, len(values))
	for name, value := range values {
		spec, hasSpec := schema[name]
		var fileName string
		if hasSpec && spec.File != "" {
			fileName = spec.File
		} else {
			format := "md"
			if hasSpec && spec.Format != "" {
				format = spec.Format
			}
			fileName = name + "." + format
		}
		files = append(files, enjugit.FileWrite{
			RepoRelPath: filepath.Join(resultDir, fileName),
			Content:     []byte(value),
		})
		fileIndex[name] = fileName
	}
	return files, fileIndex
}
