package main

// upgrade.go — `enju upgrade`: replace the running binary with a
// newer release from GitHub. Linux + macOS only; Windows users get
// pointed at the releases page.
//
// Trust model: the binary identifies its own source (upgradeRepo
// constant below), so an upgrade always pulls from the same place
// the original install came from. There's no --repo flag — switching
// forks is a fresh install, not an "upgrade."
//
// Replacement model: write the new binary next to the old one as
// `enju.upgrade-new`, then os.Rename it over the old path. On Linux/
// macOS this is atomic on the same filesystem AND safe for the
// running process — the kernel keeps the old inode alive until this
// process exits, while future invocations see the new file at the
// path. The fix-up of the enju-coord hardlink (build.sh sets one up
// next to enju; install.sh mirrors that) happens after the swap,
// best-effort.

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// upgradeRepo is the GitHub repo this binary upgrades from. Pinned
// at build time, not configurable — see file header.
const upgradeRepo = "tamerh/enju"

// upgradeOpts holds parsed CLI flags. Kept as a struct so the
// helpers below take one argument instead of five, and so the
// dry-run path is one boolean rather than threaded through every
// call.
type upgradeOpts struct {
	targetVersion string
	force         bool
	dryRun        bool
}

func cmdUpgrade(args []string) {
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	targetVersion := fs.String("version", "", "Specific version to install (e.g. v0.1.0). Default = latest GitHub release.")
	force := fs.Bool("force", false, "Re-install even if the current version already matches the target.")
	dryRun := fs.Bool("dry-run", false, "Print what would happen, but don't modify anything on disk.")
	fs.Parse(args)

	opts := upgradeOpts{
		targetVersion: strings.TrimSpace(*targetVersion),
		force:         *force,
		dryRun:        *dryRun,
	}
	if err := runUpgrade(opts); err != nil {
		fmt.Fprintf(os.Stderr, "enju upgrade: %v\n", err)
		os.Exit(1)
	}
}

func runUpgrade(opts upgradeOpts) error {
	// Refuse platforms we don't ship a tarball for. The release
	// pipeline produces .zip for Windows, not .tar.gz, so the
	// extractor here wouldn't work even if everything else did.
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	if goos == "windows" {
		return fmt.Errorf("Windows is not supported by `enju upgrade`. Download the new .zip from https://github.com/%s/releases and extract manually.", upgradeRepo)
	}
	if goos != "linux" && goos != "darwin" {
		return fmt.Errorf("unsupported OS %q; `enju upgrade` supports linux and darwin only", goos)
	}
	if goarch != "amd64" && goarch != "arm64" {
		return fmt.Errorf("no pre-built binary for %s/%s; build from source", goos, goarch)
	}

	// Locate the running binary. EvalSymlinks resolves a typical
	// "symlink in ~/bin → real file in ~/.local/bin" setup so we
	// replace the file the symlink points at, not the symlink
	// itself.
	selfRaw, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating current executable: %w", err)
	}
	self, err := filepath.EvalSymlinks(selfRaw)
	if err != nil {
		return fmt.Errorf("resolving symlinks on %s: %w", selfRaw, err)
	}

	// Resolve target version. --version pin wins; otherwise ask
	// GitHub for the latest tag. Unlike install.sh we have NO
	// fallback constant: an upgrade that can't verify "what's
	// latest" should fail loudly, not silently install a known-
	// stale version on top of a user's working setup.
	if opts.targetVersion == "" {
		latest, err := fetchLatestTag()
		if err != nil {
			return fmt.Errorf("resolving latest release (try --version vX.Y.Z to pin): %w", err)
		}
		opts.targetVersion = latest
	} else if !strings.HasPrefix(opts.targetVersion, "v") {
		opts.targetVersion = "v" + opts.targetVersion
	}

	// Compare with what we're running now. Version is the
	// ldflag-stamped global in main.go; "dev" means a local build.
	if Version == opts.targetVersion && !opts.force {
		fmt.Printf("Already on %s. Use --force to re-install.\n", Version)
		return nil
	}
	if Version == "dev" {
		fmt.Printf("Current build is 'dev' (locally compiled). Upgrading to %s ...\n", opts.targetVersion)
	} else {
		fmt.Printf("Upgrading enju %s → %s ...\n", Version, opts.targetVersion)
	}

	// URL construction matches the release pipeline's archive
	// naming (build.sh cmd_release). Keep these two in sync — any
	// rename there has to be reflected here.
	archive := fmt.Sprintf("enju-%s-%s.tar.gz", goos, goarch)
	base := fmt.Sprintf("https://github.com/%s/releases/download/%s", upgradeRepo, opts.targetVersion)
	archiveURL := base + "/" + archive
	sumsURL := base + "/SHA256SUMS"

	td, err := os.MkdirTemp("", "enju-upgrade-")
	if err != nil {
		return fmt.Errorf("creating tempdir: %w", err)
	}
	defer os.RemoveAll(td)

	archivePath := filepath.Join(td, archive)
	fmt.Printf("  Downloading %s ...\n", archive)
	if err := downloadFile(archiveURL, archivePath); err != nil {
		return fmt.Errorf("downloading %s: %w", archive, err)
	}

	// Verify against SHA256SUMS. Unlike install.sh's best-effort
	// posture (warn + proceed when SHA256SUMS is missing on old
	// releases), upgrade MUST verify. A user with a working binary
	// loses nothing by aborting an unverifiable upgrade.
	fmt.Println("  Verifying checksum ...")
	sumsPath := filepath.Join(td, "SHA256SUMS")
	if err := downloadFile(sumsURL, sumsPath); err != nil {
		return fmt.Errorf("downloading SHA256SUMS (required for upgrade verification): %w", err)
	}
	expected, err := lookupSum(sumsPath, archive)
	if err != nil {
		return err
	}
	actual, err := sha256File(archivePath)
	if err != nil {
		return fmt.Errorf("hashing downloaded archive: %w", err)
	}
	if expected != actual {
		return fmt.Errorf("checksum mismatch on %s:\n  expected: %s\n  actual:   %s", archive, expected, actual)
	}

	// Extract the enju binary out of the tarball into the same
	// tempdir. extractEnjuBinary is intentionally narrow: it
	// pulls only the file named "enju" and ignores anything else
	// in the archive.
	fmt.Println("  Extracting ...")
	newBin, err := extractEnjuBinary(archivePath, td)
	if err != nil {
		return fmt.Errorf("extracting archive: %w", err)
	}

	// Sanity-run the new binary before swapping. Catches the
	// archive-was-wrong-arch / corrupted-download cases that
	// somehow squeezed past the checksum (e.g. CDN serving an
	// HTML error page that happens to hash correctly against
	// another error page in SHA256SUMS — unlikely but cheap to
	// guard against). 5-second timeout: `enju version` is a
	// printf, anything longer means the binary is hung.
	fmt.Println("  Sanity-checking new binary ...")
	if err := sanityRunNewBinary(newBin); err != nil {
		return fmt.Errorf("downloaded binary failed self-check, refusing to install: %w", err)
	}

	if opts.dryRun {
		fmt.Printf("\nDry-run: would replace %s with the new binary.\n", self)
		return nil
	}

	// Stage + atomic swap. Write to "<self>.upgrade-new" then
	// rename over the old path. Same-directory rename is atomic
	// on a single filesystem; since "self" and the staging path
	// share a parent dir, that's guaranteed.
	fmt.Printf("  Replacing %s ...\n", self)
	stagePath := self + ".upgrade-new"
	if err := copyFile(newBin, stagePath, 0o755); err != nil {
		return fmt.Errorf("staging new binary at %s (permission denied? try with sudo if the binary is in a system dir): %w", stagePath, err)
	}
	if err := os.Rename(stagePath, self); err != nil {
		// Clean up the orphaned staging file before propagating.
		_ = os.Remove(stagePath)
		return fmt.Errorf("atomic swap: %w", err)
	}

	// Refresh the enju-coord hardlink if either (a) one already
	// exists next to the binary (preserve operator's pkill recipe
	// after upgrade) or (b) the binary is named "enju" (install
	// convention — give them the hardlink even on first upgrade).
	// Best-effort: failure here is a warning, not a hard error.
	refreshCoordHardlink(self)

	// Strip macOS quarantine xattr so the first run after upgrade
	// doesn't hit Gatekeeper. Best-effort: missing attr is fine,
	// xattr command being absent (unlikely on macOS) is fine.
	if goos == "darwin" {
		_ = exec.Command("xattr", "-d", "com.apple.quarantine", self).Run()
	}

	fmt.Printf("\n✓ enju upgraded to %s\n", opts.targetVersion)
	return nil
}

// fetchLatestTag queries GitHub's releases/latest endpoint for the
// upgradeRepo and returns the tag_name field. 10s timeout covers
// the common transient slow-DNS case without hanging an interactive
// command. No auth header → 60 req/hr per IP rate limit applies,
// which is fine for a human-invoked verb.
func fetchLatestTag() (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", upgradeRepo)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "enju-upgrade")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 403 {
		return "", fmt.Errorf("GitHub API rate-limited (403); retry later or pin with --version")
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.TagName == "" {
		return "", fmt.Errorf("GitHub API response missing tag_name (no published releases yet?)")
	}
	return body.TagName, nil
}

// downloadFile fetches url to dest using a 5-minute timeout —
// generous enough for slow links on a ~25 MB binary tarball without
// hanging forever on a stalled connection. Returns a non-nil error
// on non-200 status so the caller can present a clean failure mode.
func downloadFile(url, dest string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "enju-upgrade")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s → %d", url, resp.StatusCode)
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

// sha256File returns the hex-encoded SHA-256 of path. Matches the
// format `sha256sum` / `shasum -a 256` produce in SHA256SUMS so a
// direct string compare suffices for verification.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// lookupSum parses the SHA256SUMS file at sumsPath and returns the
// expected hex digest for archiveName. Line format is the universal
// `sha256sum` shape: "<hex>  <filename>" (two spaces, BSD-compatible
// when written by `shasum -a 256`). Returns a clear error when the
// file doesn't list our archive.
func lookupSum(sumsPath, archiveName string) (string, error) {
	data, err := os.ReadFile(sumsPath)
	if err != nil {
		return "", fmt.Errorf("reading SHA256SUMS: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		// fields[0] is the digest; fields[1] is the archive
		// filename, sometimes prefixed with "*" in BSD mode.
		name := strings.TrimPrefix(fields[1], "*")
		if name == archiveName {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS does not list %s — the release may be incomplete", archiveName)
}

// extractEnjuBinary streams archivePath (.tar.gz) and writes the
// single file named "enju" out to destDir/enju.new. Returns the
// extracted file's path on success. Ignores every other tar entry
// (directories, sibling files) — we only care about the binary.
//
// The archive layout matches build.sh cmd_release: a top-level
// directory `enju-<os>-<arch>/` containing the binary. We don't
// rely on the dir name; just scan for any regular file whose
// basename is "enju".
func extractEnjuBinary(archivePath, destDir string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if filepath.Base(hdr.Name) != "enju" {
			continue
		}
		outPath := filepath.Join(destDir, "enju.new")
		out, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return "", err
		}
		if err := out.Close(); err != nil {
			return "", err
		}
		return outPath, nil
	}
	return "", fmt.Errorf("archive does not contain an `enju` file")
}

// sanityRunNewBinary executes `<bin> version` and checks the output
// looks plausible. Pre-swap insurance against a wrong-arch download
// or a corrupted extract that somehow matched SHA256SUMS (unlikely
// in practice, but the cost of this check is ~10ms).
func sanityRunNewBinary(bin string) error {
	cmd := exec.Command(bin, "version")
	// Discard stderr so a noisy startup line doesn't bleed into
	// the upgrade output; we only consume stdout to check the
	// version line.
	cmd.Stderr = io.Discard
	timer := time.AfterFunc(5*time.Second, func() {
		_ = cmd.Process.Kill()
	})
	defer timer.Stop()
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("running `enju version`: %w", err)
	}
	if !strings.Contains(string(out), "enju") {
		return fmt.Errorf("`enju version` produced unexpected output: %q", strings.TrimSpace(string(out)))
	}
	return nil
}

// copyFile copies src → dst with the given mode. Used to stage the
// extracted binary next to the running one (across-filesystems
// safe; os.Rename would not be if the tempdir and install dir
// happen to be on different mounts).
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// refreshCoordHardlink ensures `enju-coord` is a hardlink to the
// new `enju` binary, matching what build.sh / install.sh set up.
// Two cases:
//
//  1. enju-coord already exists alongside self → remove + relink so
//     it picks up the new inode (otherwise it'd still point at the
//     pre-upgrade binary).
//  2. enju-coord doesn't exist, but self is named "enju" → create
//     it. First-time upgrade of an install that predates the
//     hardlink convention; cheap to add.
//
// Best-effort: any failure is logged to stderr but the upgrade is
// still considered successful. The hardlink is a `pkill -f enju-
// coord` convenience, not load-bearing.
func refreshCoordHardlink(self string) {
	dir := filepath.Dir(self)
	coordPath := filepath.Join(dir, "enju-coord")
	if _, err := os.Lstat(coordPath); err == nil {
		if rmErr := os.Remove(coordPath); rmErr != nil {
			fmt.Fprintf(os.Stderr, "  warning: could not remove stale enju-coord hardlink: %v\n", rmErr)
			return
		}
		if linkErr := os.Link(self, coordPath); linkErr != nil {
			fmt.Fprintf(os.Stderr, "  warning: could not refresh enju-coord hardlink: %v\n", linkErr)
		}
		return
	}
	// No existing enju-coord; only auto-create when self is the
	// canonical "enju" filename. Custom-renamed binaries
	// (someone running an `enju-test` build) shouldn't get a
	// surprise sibling.
	if filepath.Base(self) == "enju" {
		_ = os.Link(self, coordPath)
	}
}
