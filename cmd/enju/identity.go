package main

import (
	"fmt"
	"os"
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

// autoRegister attempts to register the user against coordURL using
// name and email from git global config. Saves credentials to credsPath
// on success. Prints a clear message either way so the operator knows
// what happened without having to read a log file.
//
// Called by cmdStart after the coordinator comes up — if credentials
// already exist this is never called, so it is safe to call on every
// start without re-registering an existing user.
// autoRegister returns true if registration succeeded (credentials are
// now saved), false otherwise. Callers use the return value to decide
// whether to skip follow-up messages about missing credentials.
func autoRegister(coordURL, credsPath string) bool {
	name := gitGlobalConfig("user.name")
	email := gitGlobalConfig("user.email")

	if name == "" && email == "" {
		fmt.Fprintln(os.Stderr, "Auto-register skipped — git config user.name and user.email are not set.")
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
