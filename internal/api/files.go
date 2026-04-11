package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// buildResultPath constructs the file path for a task result.
func buildResultPath(problemID, instanceKey, taskDefID string) string {
	if instanceKey != "" {
		return filepath.Join("results", problemID, instanceKey, taskDefID+".json")
	}
	return filepath.Join("results", problemID, taskDefID+".json")
}

// writeResultFile writes a result JSON file to the git working directory.
func writeResultFile(gitDir, resultPath string, data interface{}) error {
	fullPath := filepath.Join(gitDir, resultPath)

	// Ensure directory exists
	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating directory %s: %w", dir, err)
	}

	// Marshal JSON
	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling result: %w", err)
	}

	// Write file
	if err := os.WriteFile(fullPath, jsonData, 0644); err != nil {
		return fmt.Errorf("writing file %s: %w", fullPath, err)
	}

	return nil
}

// readResultFile reads a result JSON file from the git working directory.
func readResultFile(gitDir, resultPath string) (map[string]interface{}, error) {
	fullPath := filepath.Join(gitDir, resultPath)

	data, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, fmt.Errorf("reading file %s: %w", fullPath, err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parsing JSON: %w", err)
	}

	return result, nil
}
