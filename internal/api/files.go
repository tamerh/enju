package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	enjuGit "github.com/enju-ai/enju/internal/git"
)

// resultDir constructs the directory path for a task's result files.
// runIDPath is the path segment — for hierarchical storage it's "projectID/runSeq".
func resultDir(runIDPath, instanceKey, taskDefID string) string {
	if instanceKey != "" {
		return filepath.Join("projects", runIDPath, instanceKey, taskDefID)
	}
	return filepath.Join("projects", runIDPath, taskDefID)
}

// contentExtension returns the file extension based on result type.
func contentExtension(resultType string) string {
	switch resultType {
	case "json":
		return ".json"
	default:
		return ".md"
	}
}

// writeResult writes the content and metadata as separate files.
// Returns the result directory path (stored in DB as result_path).
func writeResult(gw *enjuGit.Writer, runID, instanceKey, taskDefID string, content string, resultType string, metadata map[string]interface{}) (string, error) {
	dir := resultDir(runID, instanceKey, taskDefID)

	// Write content file — raw, no JSON wrapping, no escaping
	ext := contentExtension(resultType)
	contentPath := filepath.Join(dir, "result"+ext)
	if err := gw.WriteFile(contentPath, []byte(content)); err != nil {
		return "", fmt.Errorf("writing content: %w", err)
	}

	// Write metadata file
	metaPath := filepath.Join(dir, "metadata.json")
	metaBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshaling metadata: %w", err)
	}
	if err := gw.WriteFile(metaPath, metaBytes); err != nil {
		return "", fmt.Errorf("writing metadata: %w", err)
	}

	return dir, nil
}

// writeMultiFileResult writes named outputs as separate files based on the output schema.
// Each output name → file is declared in the schema; the values map contains the content.
// Returns the result directory path.
func writeMultiFileResult(gw *enjuGit.Writer, runID, instanceKey, taskDefID string, schema map[string]outputFileSpec, values map[string]string, metadata map[string]interface{}) (string, error) {
	dir := resultDir(runID, instanceKey, taskDefID)

	fileIndex := map[string]string{} // output name -> file path

	for name, value := range values {
		spec, hasSpec := schema[name]

		var fileName string
		if hasSpec && spec.File != "" {
			fileName = spec.File
		} else {
			// No file declared — use name + format extension
			format := "md"
			if hasSpec && spec.Format != "" {
				format = spec.Format
			}
			fileName = name + "." + format
		}

		fullPath := filepath.Join(dir, fileName)
		if err := gw.WriteFile(fullPath, []byte(value)); err != nil {
			return "", fmt.Errorf("writing %s: %w", fileName, err)
		}
		fileIndex[name] = fileName
	}

	// Add file index to metadata
	metadata["output_files"] = fileIndex

	metaPath := filepath.Join(dir, "metadata.json")
	metaBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshaling metadata: %w", err)
	}
	if err := gw.WriteFile(metaPath, metaBytes); err != nil {
		return "", fmt.Errorf("writing metadata: %w", err)
	}

	return dir, nil
}

// outputFileSpec is a minimal version of yaml.OutputSpec for local use.
type outputFileSpec struct {
	Description string `json:"description"`
	File        string `json:"file"`
	Format      string `json:"format"`
}

// readResultContent reads the content file for a task result.
func readResultContent(gw *enjuGit.Writer, resultPath string) (string, error) {
	// resultPath is the directory — find the content file
	// Try result.md first, then result.json
	for _, ext := range []string{".md", ".json"} {
		contentPath := filepath.Join(resultPath, "result"+ext)
		data, err := gw.ReadFile(contentPath)
		if err == nil {
			return string(data), nil
		}
	}
	return "", fmt.Errorf("no content file found in %s", resultPath)
}

// readResultForTemplate reads a task result and returns it in a format
// suitable for template resolution.
func readResultForTemplate(gw *enjuGit.Writer, resultPath, taskDefID string) (map[string]interface{}, error) {
	result := map[string]interface{}{
		"task_id": taskDefID,
	}

	// Check if this is a multi-file result by reading metadata first
	metaPath := filepath.Join(resultPath, "metadata.json")
	metaData, metaErr := gw.ReadFile(metaPath)
	if metaErr == nil {
		var meta map[string]interface{}
		if json.Unmarshal(metaData, &meta) == nil {
			result["metadata"] = meta

			// If we have output_files, this is a multi-file result
			if outputFiles, ok := meta["output_files"].(map[string]interface{}); ok && len(outputFiles) > 0 {
				contentMap := make(map[string]interface{})
				for name, fileName := range outputFiles {
					fname, _ := fileName.(string)
					if fname == "" {
						continue
					}
					data, err := gw.ReadFile(filepath.Join(resultPath, fname))
					if err != nil {
						continue
					}
					contentMap[name] = string(data)
				}
				result["content"] = contentMap
				return result, nil
			}
		}
	}

	// Fall back to single-file reading
	content, err := readResultContent(gw, resultPath)
	if err != nil {
		return nil, err
	}
	result["content"] = content

	// If content is JSON, try to parse it for named output access
	if strings.HasSuffix(resultPath, ".json") || isJSON(content) {
		var parsed interface{}
		if json.Unmarshal([]byte(content), &parsed) == nil {
			result["content"] = parsed
		}
	}

	return result, nil
}

func isJSON(s string) bool {
	s = strings.TrimSpace(s)
	return (strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}")) ||
		(strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]"))
}

// Legacy functions kept for compatibility — remove after full migration
func buildResultPath(runID, instanceKey, taskDefID string) string {
	return resultDir(runID, instanceKey, taskDefID)
}

func readResultFile(gitDir, resultPath string) (map[string]interface{}, error) {
	// Try new format (directory with result.md + metadata.json)
	contentPath := filepath.Join(gitDir, resultPath, "result.md")
	if data, err := os.ReadFile(contentPath); err == nil {
		return map[string]interface{}{
			"content": string(data),
		}, nil
	}

	// Fall back to old format (single .json file)
	fullPath := filepath.Join(gitDir, resultPath)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", fullPath, err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parsing: %w", err)
	}
	return result, nil
}
