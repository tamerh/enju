package git

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// sshAuthMethod returns an SSH auth method for the given remote
// URL. Tries the SSH agent first (SSH_AUTH_SOCK), falls back to
// common key file paths. Returns nil for non-SSH URLs (http/https,
// local paths) — go-git handles those without explicit auth.
func sshAuthMethod(remoteURL string) transport.AuthMethod {
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
// (ssh:// or git@host:path form). Used by sshAuthMethod and by
// callers that need to decide whether to set up SSH credentials.
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
// Used by callers deciding whether to treat a path as a "remote"
// (file:// implicitly) for solo projects.
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
