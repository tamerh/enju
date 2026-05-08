package enjugit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	corelayout "github.com/enju-ai/enju/internal/common/layout"
	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/fatclient/enjugit/internal/git"
)

// templates.go — template discovery + bundle reading on the
// fat-client side. Source-of-truth invariant: every template
// read goes through the default-branch tree, not the worktree
// filesystem. So enju_list_templates returns the same set
// regardless of which run branch the workspace is checked out
// on, and per-run snapshots are deterministic.
//
// Workflow holds no project-level state (default branch,
// configured templates roots) — service passes those per call.
// That keeps the Workflow type stateless w.r.t. project config:
// service owns the project record and forwards as needed.

// TemplateSummary is the lightweight shape returned by
// ListTemplates — enough for an LLM to pick a template from a
// menu without parsing the full YAML. ParseError populated when
// a discovered template's YAML failed to decode/validate, so
// the menu shows it as broken instead of silently dropping it.
type TemplateSummary struct {
	Path        string         `json:"path"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Params      []ParamSummary `json:"params,omitempty"`
	ParseError  string         `json:"parse_error,omitempty"`
}

// ParamSummary is the per-param shape embedded in a TemplateSummary.
// Compressed view of the YAML ParamDef — just the fields the LLM
// needs when deciding whether a template fits a request.
type ParamSummary struct {
	Name        string      `json:"name"`
	Type        string      `json:"type"`
	Required    bool        `json:"required,omitempty"`
	Default     interface{} `json:"default,omitempty"`
	Description string      `json:"description,omitempty"`
}

// LoadedTemplate is the full parsed view of a template bundle.
// Path is the manifest YAML's repo-relative path; BundleDir is
// the enclosing dir, used by snapshot-on-instantiate to enumerate
// every file to copy.
type LoadedTemplate struct {
	Path      string
	BundleDir string
	Raw       []byte
	Summary   TemplateSummary
}

// resolvedBundle pairs a classified manifest path with its
// bytes. Internal to the load path.
type resolvedBundle struct {
	bundleDir    string
	manifestPath string
	manifest     []byte
}

// maxTemplateBundleBytes caps a single bundle's total blob size.
// Templates aren't a place for large data; runaway snapshots
// would bloat every subsequent run commit.
const maxTemplateBundleBytes = 10 * 1024 * 1024

// ListTemplates scans the default template root on the default
// branch and returns one summary per directory bundle. The root
// is corelayout.DefaultTemplatesDir; the branch is the workflow's
// default (set via SetDefaultBranch, falling back to
// convs.DefaultRunBranch).
//
// Directories without an enju.yaml are skipped silently. A
// bundle whose enju.yaml fails to parse appears with ParseError
// populated. Loose `.yaml` files directly under a templates
// root produce a migration-hint entry so the author isn't left
// with an empty menu.
func (w *Workflow) ListTemplates() ([]TemplateSummary, error) {
	roots := []string{corelayout.DefaultTemplatesDir}
	sha, err := w.resolveDefaultBranchSHA(w.DefaultBranch())
	if err != nil {
		return nil, err
	}
	if sha == "" {
		// Default branch has no commits yet — not an error,
		// just nothing to list.
		return nil, nil
	}
	var out []TemplateSummary
	seen := map[string]bool{}
	for _, root := range roots {
		items, err := w.scanTemplateRoot(sha, root)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if seen[it.Path] {
				continue
			}
			seen[it.Path] = true
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// scanTemplateRoot walks one template root in the default-branch
// tree. Splits to keep ListTemplates' fan-over-multiple-roots
// loop readable.
func (w *Workflow) scanTemplateRoot(sha, root string) ([]TemplateSummary, error) {
	entries, ok, err := w.git.ReadTreeEntriesAtCommit(sha, root)
	if err != nil {
		return nil, fmt.Errorf("reading templates dir %s: %w", root, err)
	}
	if !ok {
		return nil, nil
	}
	var out []TemplateSummary
	for _, e := range entries {
		if e.IsDir {
			// Possible bundle — check for the manifest inside.
			subPath := filepath.ToSlash(filepath.Join(root, e.Name))
			manifestPath := subPath + "/" + corelayout.BundleManifestName
			data, found, ferr := w.git.ReadFileAtCommit(sha, manifestPath)
			if ferr != nil {
				return nil, fmt.Errorf("reading template manifest %s: %w", manifestPath, ferr)
			}
			if !found {
				// Subdir without a manifest — not a bundle, skip.
				continue
			}
			summary := summaryFromManifestBytes(manifestPath, data)
			out = append(out, summary)
			continue
		}
		// File at the root level — the legacy single-file shape.
		// Surface ONE migration-hint per offender so the author
		// doesn't see an empty menu.
		if strings.HasSuffix(e.Name, ".yaml") || strings.HasSuffix(e.Name, ".yml") {
			legacyRel := filepath.ToSlash(filepath.Join(root, e.Name))
			base := strings.TrimSuffix(strings.TrimSuffix(e.Name, ".yaml"), ".yml")
			out = append(out, TemplateSummary{
				Path: legacyRel,
				ParseError: fmt.Sprintf(
					"legacy single-file template layout — move %s to %s/%s/%s",
					legacyRel, root, base, corelayout.BundleManifestName),
			})
		}
	}
	return out, nil
}

// summaryFromManifestBytes parses a manifest blob and returns
// either a populated TemplateSummary or one with ParseError set.
// Never returns an error — bad templates appear in the menu as
// broken so the author can find the typo.
func summaryFromManifestBytes(path string, data []byte) TemplateSummary {
	parsed, err := enjuYaml.Parse(data)
	if err != nil {
		return TemplateSummary{Path: path, ParseError: err.Error()}
	}
	return TemplateSummary{
		Path:        path,
		Name:        parsed.Run.Name,
		Description: parsed.Run.Description,
		Params:      paramSummaries(parsed.Run.Params),
	}
}

func paramSummaries(ps []enjuYaml.ParamDef) []ParamSummary {
	out := make([]ParamSummary, 0, len(ps))
	for _, pp := range ps {
		out = append(out, ParamSummary{
			Name:        pp.Name,
			Type:        pp.Type,
			Required:    pp.Required,
			Default:     pp.Default,
			Description: pp.Description,
		})
	}
	return out
}

// LoadTemplate reads a template bundle by either:
//   - its directory path (e.g. "enju/templates/gwas-analysis")
//   - the full path to its manifest (e.g. "enju/templates/gwas-analysis/enju.yaml")
//
// Tree-first, worktree-fallback: primary source is the
// default-branch tree (so LoadTemplate works from any workspace
// branch). The worktree fallback supports the author-on-disk
// UX — a user writes enju.yaml into the worktree without
// committing, then create_run's EnsureBundleOnDefault auto-commits.
//
// Default branch and workdir are read from workflow state —
// set the default branch via SetDefaultBranch after Workflow
// construction. Templates always live under
// corelayout.DefaultTemplatesDir on the default branch.
func (w *Workflow) LoadTemplate(repoRelPath string) (*LoadedTemplate, error) {
	roots := []string{corelayout.DefaultTemplatesDir}
	clean := filepath.ToSlash(filepath.Clean(repoRelPath))
	if strings.Contains(clean, "../") || clean != repoRelPath {
		return nil, fmt.Errorf("template path %q contains disallowed path components", repoRelPath)
	}
	if err := assertUnderRoots(repoRelPath, roots); err != nil {
		return nil, err
	}
	bundleDir, manifestPath, err := resolveBundlePathShape(repoRelPath, roots)
	if err != nil {
		return nil, err
	}
	rb, err := w.readBundleManifest(w.DefaultBranch(), bundleDir, manifestPath, w.WorkDir())
	if err != nil {
		return nil, err
	}
	parsed, err := enjuYaml.Parse(rb.manifest)
	if err != nil {
		return nil, fmt.Errorf("parsing template %s: %w", rb.manifestPath, err)
	}
	return &LoadedTemplate{
		Path:      rb.manifestPath,
		BundleDir: rb.bundleDir,
		Raw:       rb.manifest,
		Summary: TemplateSummary{
			Path:        rb.manifestPath,
			Name:        parsed.Run.Name,
			Description: parsed.Run.Description,
			Params:      paramSummaries(parsed.Run.Params),
		},
	}, nil
}

// readBundleManifest tries the default-branch tree first, then
// the worktree filesystem (when workdir is non-empty). Returns
// a populated resolvedBundle or a clear "not found" error.
func (w *Workflow) readBundleManifest(defaultBranch, bundleDir, manifestPath, workdir string) (*resolvedBundle, error) {
	sha, err := w.resolveDefaultBranchSHA(defaultBranch)
	if err != nil {
		return nil, err
	}
	if sha != "" {
		data, found, rerr := w.git.ReadFileAtCommit(sha, manifestPath)
		if rerr != nil && !errors.Is(rerr, git.ErrCommitNotFound) {
			return nil, fmt.Errorf("reading template %s: %w", manifestPath, rerr)
		}
		if found {
			return &resolvedBundle{
				bundleDir:    bundleDir,
				manifestPath: manifestPath,
				manifest:     data,
			}, nil
		}
	}
	// Worktree fallback for pre-commit authoring UX.
	if workdir != "" {
		data, fsErr := os.ReadFile(filepath.Join(workdir, manifestPath))
		if fsErr == nil {
			return &resolvedBundle{
				bundleDir:    bundleDir,
				manifestPath: manifestPath,
				manifest:     data,
			}, nil
		}
		if !os.IsNotExist(fsErr) {
			return nil, fmt.Errorf("reading template %s from worktree: %w", manifestPath, fsErr)
		}
	}
	return nil, fmt.Errorf("template %q not found on default branch or in worktree — check `enju_list_templates` for available recipes", manifestPath)
}

// ReadBundleFiles walks the bundle dir on the default branch and
// returns every regular blob as a FileWrite, with paths rebased
// to targetDir. Used by handleCreateRun for snapshot-on-instantiate
// — committing the bundle into the run's snapshot area locks the
// recipe + scripts to the moment the run was created.
//
// Errors with a friendly message when the default branch has no
// commits yet (callers must EnsureBundleOnDefault first).
//
// Size guard: > maxTemplateBundleBytes returns an error. Hidden
// segments (.git, .DS_Store, etc.) are skipped automatically by
// the underlying tree walker.
func (w *Workflow) ReadBundleFiles(bundleDir, targetDir string) ([]FileWrite, error) {
	sha, err := w.resolveDefaultBranchSHA(w.DefaultBranch())
	if err != nil {
		return nil, err
	}
	if sha == "" {
		return nil, fmt.Errorf("default branch has no commits; commit the template bundle before instantiating")
	}
	var totalBytes int64
	var files []FileWrite
	walkErr := w.git.WalkSubtreeBlobsAtCommit(sha, bundleDir, func(rel string, mode os.FileMode, content []byte) error {
		totalBytes += int64(len(content))
		if totalBytes > maxTemplateBundleBytes {
			return fmt.Errorf("bundle %q exceeds %d-byte size limit; templates shouldn't carry large data blobs", bundleDir, maxTemplateBundleBytes)
		}
		// Default mode 0o644; preserve +x when source had it.
		fmode := os.FileMode(0o644)
		if mode&0o111 != 0 {
			fmode = 0o755
		}
		files = append(files, FileWrite{
			RepoRelPath: filepath.ToSlash(filepath.Join(targetDir, rel)),
			Content:     content,
			Mode:        fmode,
		})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("bundle %q not found or empty on default branch", bundleDir)
	}
	return files, nil
}

// EnsureBundleOnDefault pins a template bundle directory to the
// project's default branch by walking the workspace filesystem
// under bundleDir and committing each file via CommitArbitraryFiles.
// This is the template-as-recipe invariant: templates live on the
// default branch so runs on any other branch can read them via
// the default-branch tree.
//
// Returns the post-operation HEAD SHA on default — whether a
// new commit landed or the bundle was already clean — so callers
// can persist it as source_commit_sha.
//
// Idempotent: a no-op (CommitArbitraryFiles' NoOp path) when
// every bundle file matches what's already on default.
func (w *Workflow) EnsureBundleOnDefault(bundleDir, authorName, authorEmail, modelName string) (string, error) {
	workDir := w.WorkDir()
	if workDir == "" {
		return "", fmt.Errorf("enjugit: EnsureBundleOnDefault: workspace has no work dir")
	}
	absBundle := filepath.Join(workDir, bundleDir)
	info, err := os.Stat(absBundle)
	if err != nil {
		return "", fmt.Errorf("enjugit: EnsureBundleOnDefault: bundle dir %q not found: %w", bundleDir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("enjugit: EnsureBundleOnDefault: %q is not a directory", bundleDir)
	}
	var files []FileWrite
	walkErr := filepath.Walk(absBundle, func(path string, fi os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if fi.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(workDir, path)
		if rerr != nil {
			return rerr
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return fmt.Errorf("read %s: %w", rel, rerr)
		}
		mode := os.FileMode(0o644)
		if fi.Mode()&0o111 != 0 {
			mode = 0o755
		}
		files = append(files, FileWrite{
			RepoRelPath: filepath.ToSlash(rel),
			Content:     body,
			Mode:        mode,
		})
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}
	if len(files) == 0 {
		// Bundle dir was empty on disk — caller still wants the
		// default-branch HEAD as source_commit_sha.
		sha, sErr := w.resolveDefaultBranchSHA(w.DefaultBranch())
		if sErr != nil {
			// No default-branch HEAD yet either; nothing to
			// return. Caller treats empty SHA as "no provenance".
			return "", nil
		}
		return sha, nil
	}
	res, err := w.CommitArbitraryFiles(CommitArbitraryFilesRequest{
		Files:       files,
		Branch:      "", // empty → default branch
		Subject:     "Commit template bundle " + bundleDir,
		AuthorName:  authorName,
		AuthorEmail: authorEmail,
		ModelName:   modelName,
	})
	if err != nil {
		return "", err
	}
	if res.NoOp {
		// Bundle files matched what's already on default.
		// Resolve the current head as source_commit_sha.
		sha, sErr := w.resolveDefaultBranchSHA(w.DefaultBranch())
		if sErr != nil {
			return res.CommitSHA, nil
		}
		return sha, nil
	}
	return res.CommitSHA, nil
}

// resolveDefaultBranchSHA returns the SHA of the default branch
// tip, preferring local refs/heads/<branch>, falling back to
// refs/remotes/origin/<branch>. Returns ("", nil) when neither
// resolves — caller treats that as "no templates visible" rather
// than an error.
func (w *Workflow) resolveDefaultBranchSHA(branch string) (string, error) {
	if branch == "" {
		branch = w.convs.DefaultRunBranch
	}
	// Local first.
	sha, err := w.git.ResolveRef("refs/heads/" + branch)
	if err == nil {
		return sha, nil
	}
	if !errors.Is(err, git.ErrRefNotFound) {
		return "", fmt.Errorf("resolving default branch %s: %w", branch, err)
	}
	// Origin tracking.
	sha, err = w.git.ResolveRef("refs/remotes/origin/" + branch)
	if err == nil {
		return sha, nil
	}
	if errors.Is(err, git.ErrRefNotFound) {
		return "", nil
	}
	return "", fmt.Errorf("resolving default branch %s: %w", branch, err)
}

// assertUnderRoots checks the path lives under at least one
// configured root. Surfaces a single clear error so a caller
// who typos doesn't get a "not found" that hides the real fix.
func assertUnderRoots(repoRelPath string, roots []string) error {
	for _, r := range roots {
		if repoRelPath == r || strings.HasPrefix(repoRelPath, r+"/") {
			return nil
		}
	}
	return fmt.Errorf("template path %q must live under one of: %s", repoRelPath, strings.Join(roots, ", "))
}

// resolveBundlePathShape classifies a caller-supplied path into
// (bundleDir, manifestPath) on shape alone — no tree or
// filesystem check. So both tree and filesystem fallback paths
// share the same classification logic.
func resolveBundlePathShape(repoRelPath string, roots []string) (bundleDir, manifestPath string, err error) {
	pth := strings.TrimSuffix(repoRelPath, "/")
	// Manifest form: ends in /<BundleManifestName>.
	if strings.HasSuffix(pth, "/"+corelayout.BundleManifestName) {
		bundleDir = strings.TrimSuffix(pth, "/"+corelayout.BundleManifestName)
		// Disallow manifest sitting directly in a templates root.
		for _, r := range roots {
			if bundleDir == r {
				return "", "", fmt.Errorf("template manifest must live inside a bundle subdirectory, e.g. %s/NAME/%s", r, corelayout.BundleManifestName)
			}
		}
		return bundleDir, pth, nil
	}
	// Legacy single-file `.yaml` in the root → emit migration hint.
	if strings.HasSuffix(pth, ".yaml") || strings.HasSuffix(pth, ".yml") {
		base := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(pth), ".yaml"), ".yml")
		parentDir := filepath.ToSlash(filepath.Dir(pth))
		return "", "", fmt.Errorf(
			"legacy single-file template path %q — templates are now directory bundles. "+
				"Move %s to %s/%s/%s and reference it as %s/%s (or the full manifest path)",
			repoRelPath, repoRelPath, parentDir, base, corelayout.BundleManifestName, parentDir, base)
	}
	// Dir form.
	return pth, pth + "/" + corelayout.BundleManifestName, nil
}
