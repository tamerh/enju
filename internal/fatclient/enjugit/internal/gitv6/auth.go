package gitv6

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v6/plumbing/client"
	gitssh "github.com/go-git/go-git/v6/plumbing/transport/ssh"
)

// SSHAuthForRemote returns an SSH auth client for the given
// remote URL. Tries the SSH agent first (SSH_AUTH_SOCK), falls
// back to common key file paths. Returns nil for non-SSH URLs
// (http/https, local paths) — callers should NOT append a
// WithSSHAuth option in that case.
//
// v6 model: auth is an interface implementing
// client.SSHAuth (ClientConfig method) and is passed via
// CloneOptions.ClientOptions / FetchOptions.ClientOptions /
// PushOptions.ClientOptions, NOT via a per-call Auth field
// (which v6 removed). v5's transport.AuthMethod return type
// is gone; concrete types from `transport/ssh` (PublicKeys,
// PublicKeysCallback) implement SSHAuth.
//
// Concrete return type is client.SSHAuth so callers can
// pass the result straight to client.WithSSHAuth without
// caring which underlying mechanism produced it.
func SSHAuthForRemote(remoteURL string) client.SSHAuth {
	if !IsSSHURL(remoteURL) {
		return nil
	}
	if os.Getenv("SSH_AUTH_SOCK") != "" {
		if auth, err := gitssh.NewSSHAgentAuth("git"); err == nil {
			return auth
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	for _, kf := range []string{
		filepath.Join(home, ".ssh", "id_ed25519"),
		filepath.Join(home, ".ssh", "id_rsa"),
		filepath.Join(home, ".ssh", "id_ecdsa"),
	} {
		if _, err := os.Stat(kf); err != nil {
			continue
		}
		auth, err := gitssh.NewPublicKeysFromFile("git", kf, "")
		if err != nil {
			continue // passphrase-protected, skip
		}
		return auth
	}
	return nil
}

// IsSSHURL returns true when the URL looks like an SSH remote
// (ssh:// or git@host:path form).
func IsSSHURL(url string) bool {
	if strings.HasPrefix(url, "ssh://") {
		return true
	}
	if strings.Contains(url, "@") && strings.Contains(url, ":") && !strings.Contains(url, "://") {
		return true
	}
	return false
}

// IsLocalWorkingTree returns true when path is a local directory
// containing a .git subdirectory (a working tree, not a bare).
func IsLocalWorkingTree(path string) bool {
	if !filepath.IsAbs(path) && !strings.HasPrefix(path, ".") {
		return false
	}
	info, err := os.Stat(filepath.Join(path, ".git"))
	if err != nil {
		return false
	}
	return info.IsDir()
}

// clientOptionsFor returns the ClientOptions slice that should
// be passed to network operations against remoteURL. Empty slice
// (not nil) when no auth is needed (HTTPS without auth, local
// paths). Caller passes the result as
// CloneOptions.ClientOptions / PushOptions.ClientOptions / etc.
//
// Centralised here so push/fetch/list-references all inherit
// the same auth setup. v5's per-call Auth field is gone in v6;
// this helper is the replacement seam.
func clientOptionsFor(remoteURL string) []client.Option {
	if auth := SSHAuthForRemote(remoteURL); auth != nil {
		return []client.Option{client.WithSSHAuth(auth)}
	}
	return nil
}
