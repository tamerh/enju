package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// resolveIdentity fills name/email from git global config for any field
// that is currently empty. Called before registration so the MCP config
// and CLI flags can stay minimal:
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

// hasFullIdentity reports whether name and email are both non-empty.
// The coordinator requires both: email is the globally-unique key for
// human citizens; name is required for display. A partial config
// (name-only or email-only) would reach registerCitizen and fail with
// a 400, giving the user a generic error instead of actionable guidance.
func hasFullIdentity(name, email string) bool {
	return name != "" && email != ""
}

// autoRegister attempts to register the user against coordURL using
// name and email from git global config. Saves credentials to credsPath
// on success. Returns true if registration succeeded, false otherwise.
// Callers use the return value to suppress follow-up messages when this
// function has already printed the reason for failure.
func autoRegister(coordURL, credsPath string) bool {
	name := gitGlobalConfig("user.name")
	email := gitGlobalConfig("user.email")

	if !hasFullIdentity(name, email) {
		fmt.Fprintln(os.Stderr, "Auto-register skipped — git config user.name and user.email are both required.")
		fmt.Fprintln(os.Stderr, "  git config --global user.name  \"Your Name\"")
		fmt.Fprintln(os.Stderr, "  git config --global user.email \"you@example.com\"")
		fmt.Fprintln(os.Stderr, "  Then restart: enju stop && enju start")
		return false
	}

	username, token, err := registerCitizen(coordURL, name, "", email)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Auto-register failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "  Run manually: enju mcp --coordinator %s\n", coordURL)
		return false
	}
	saveCredentialsAt(coordURL, username, name, email, token, credsPath)
	fmt.Fprintf(os.Stderr, "Registered as @%s (%s)\n", username, name)
	return true
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
