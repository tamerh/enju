package main

import (
	"os/exec"
	"strings"
)

// resolveIdentity fills name/email/username from git global config for
// any field that wasn't explicitly passed and credentials.json doesn't
// already have. Called before registration in every command that needs
// a citizen identity, so the MCP config and CLI flags can stay minimal:
//
//	{"command": "enju", "args": ["mcp", "-coordinator", "..."]}
//
// is sufficient when `git config --global user.name` and `user.email`
// are set — no -name or -email flags required.
func resolveIdentity(name, email, username *string) {
	if *name == "" {
		*name = gitGlobalConfig("user.name")
	}
	if *email == "" {
		*email = gitGlobalConfig("user.email")
	}
	// username is left empty when unset — the coordinator generates
	// one from the display name if the caller doesn't supply it.
}

// gitGlobalConfig reads a single git global config key by shelling out
// to `git config --global <key>`. Returns empty string on any error so
// callers can treat empty as "not set" without special-casing errors.
func gitGlobalConfig(key string) string {
	out, err := exec.Command("git", "config", "--global", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
