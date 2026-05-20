package enjugit

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	corelayout "github.com/enju-ai/enju/internal/common/layout"
	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	git "github.com/enju-ai/enju/internal/fatclient/enjugit/internal/gitcli"
)

// workflows.go — workflow discovery + bundle reading on the
// fat-client side. Source-of-truth invariant: every workflow read
// goes through the default-branch tree, not the worktree filesystem.
// So enju_list_workflows returns the same set regardless of which
// run branch the workspace is checked out on, and per-run snapshots
// are deterministic.
//
// Discovery is intentionally dumb: walk the default-branch tree
// for every *.yaml/*.yml file outside hidden directories. We do
// NOT parse or sniff content to decide whether a file is a workflow
// — that's the operator's call. The MCP description names the
// convention (root-level enju.yaml or workflows/<name>/) without
// enforcing it. A heuristic that opened files to check for `tasks:`
// would make the contract fuzzy ("usually skipped, sometimes not").
//
// Workflow holds no project-level state — service passes the
// default branch per call.

// WorkflowSummary is the path-only shape returned by ListWorkflows.
// Just a path — no name, description, or param schema, because
// list doesn't parse. Use enju_describe_workflow (which loads and
// parses one file) for the richer view.
type WorkflowSummary struct {
	Path string `json:"path"`
}

// TemplateSummary is the legacy listing shape kept as a deprecated
// alias for callers that still expect rich fields (currently the
// webui). The new ListWorkflows path returns WorkflowSummary;
// service.ListTemplates adapts and leaves Name/Description/Params
// empty (no parse on list).
//
// Deprecated: use WorkflowSummary; the rich fields are no longer
// populated by listing.
type TemplateSummary struct {
	Path        string         `json:"path"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Params      []ParamSummary `json:"params,omitempty"`
	ParseError  string         `json:"parse_error,omitempty"`
}

// ParamSummary is the per-param shape embedded in a LoadedWorkflow's
// details. Compressed view of the YAML ParamDef — the fields a
// caller needs when deciding whether a workflow fits a request or
// when building a param-entry form.
type ParamSummary struct {
	Name        string      `json:"name"`
	Type        string      `json:"type"`
	Required    bool        `json:"required,omitempty"`
	Default     interface{} `json:"default,omitempty"`
	Description string      `json:"description,omitempty"`
}

// WorkflowDetails is the rich-shape view of one workflow returned
// by LoadWorkflow / enju_describe_workflow. Unlike WorkflowSummary
// (list shape, path only), this DOES require a parse — it's the
// per-file detail view.
type WorkflowDetails struct {
	Path        string         `json:"path"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Params      []ParamSummary `json:"params,omitempty"`
}

// LoadedWorkflow is the full parsed view of a workflow bundle.
// Path is the manifest YAML's repo-relative path; BundleDir is
// the enclosing dir, used by snapshot-on-instantiate to enumerate
// every file to copy.
//
// Parsed is the result of yaml.Parse on Raw; exposed so callers
// don't re-parse to read fields like the inline `bots:` block.
type LoadedWorkflow struct {
	Path      string
	BundleDir string
	Raw       []byte
	Details   WorkflowDetails
	Parsed    *enjuYaml.ParsedRun
}

// LoadedTemplate is the deprecated alias kept for callers (webui)
// still on the old field name. New code uses LoadedWorkflow.
//
// Deprecated: use LoadedWorkflow.
type LoadedTemplate struct {
	Path      string
	BundleDir string
	Raw       []byte
	Summary   TemplateSummary
	Parsed    *enjuYaml.ParsedRun
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

// ListWorkflows returns every *.yaml / *.yml path in the default-
// branch tree, modulo paths whose any directory component starts
// with a dot (so .git/, .enju/, .github/, .vscode/, etc. never
// appear). Path-only — no content read, no parse. The caller picks
// which file is "actually" a workflow; Enju does not validate.
//
// Convention (not enforced): a workflow YAML lives at the repo
// root as `enju.yaml` or under `workflows/<name>/enju.yaml`. The
// MCP tool description names that convention; users are free to
// store workflows elsewhere.
//
// Returns nil when the default branch has no commits yet — not
// an error, just nothing to list.
func (w *Workflow) ListWorkflows() ([]WorkflowSummary, error) {
	sha, err := w.resolveDefaultBranchSHA(w.DefaultBranch())
	if err != nil {
		return nil, err
	}
	if sha == "" {
		return nil, nil
	}
	paths, err := w.git.ListBlobPathsAtCommit(sha)
	if err != nil {
		return nil, fmt.Errorf("listing workflows: %w", err)
	}
	out := make([]WorkflowSummary, 0, len(paths))
	for _, p := range paths {
		if !hasYAMLExt(p) {
			continue
		}
		if hasHiddenComponent(p) {
			continue
		}
		out = append(out, WorkflowSummary{Path: p})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// hasYAMLExt is the file-extension half of the workflow-list
// filter. Plain suffix check — not case-folded; git paths are
// case-sensitive and the conventional extensions are lowercase.
func hasYAMLExt(p string) bool {
	return strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml")
}

// hasHiddenComponent reports whether any forward-slash-separated
// component of p starts with '.'. Used to exclude .git/, .enju/,
// .github/, .vscode/, and any other dotted directory regardless
// of depth. A leading-dot FILE at the repo root (e.g. `.enju.yaml`)
// is also excluded by this rule — symmetric, no special case.
func hasHiddenComponent(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// detailsFromManifestBytes parses a manifest blob into the rich
// describe-shape. Used by LoadWorkflow (not by the list path).
// Never returns an error here: a parse failure is signalled by
// the empty Name/Description; LoadWorkflow's own parse path
// surfaces the typed parse error to the caller.
func detailsFromManifestBytes(path string, parsed *enjuYaml.ParsedRun) WorkflowDetails {
	return WorkflowDetails{
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

// LoadWorkflow reads a workflow YAML by either:
//   - its directory path (e.g. "workflows/gwas-analysis") —
//     the YAML inside is named enju.yaml by convention
//   - the full path to the YAML file (e.g. "workflows/gwas-analysis/enju.yaml",
//     or any "*.yaml" anywhere in the repo)
//
// Tree-first, worktree-fallback: primary source is the
// default-branch tree (so LoadWorkflow works from any workspace
// branch). The worktree fallback supports the author-on-disk
// UX — a user writes the YAML into the worktree without
// committing, then create_run's EnsureBundleOnDefault auto-commits.
//
// Workflow YAMLs can live anywhere in the repo at any depth;
// LoadWorkflow only enforces a no-traversal path-shape guard.
func (w *Workflow) LoadWorkflow(repoRelPath string) (*LoadedWorkflow, error) {
	clean := filepath.ToSlash(filepath.Clean(repoRelPath))
	if strings.Contains(clean, "../") || clean != repoRelPath {
		return nil, fmt.Errorf("workflow path %q contains disallowed path components", repoRelPath)
	}
	bundleDir, manifestPath, err := resolveBundlePathShape(repoRelPath)
	if err != nil {
		return nil, err
	}
	rb, err := w.readBundleManifest(w.DefaultBranch(), bundleDir, manifestPath, w.WorkDir())
	if err != nil {
		return nil, err
	}
	// Resolve any `include:` directive against the SAME pinned
	// source the manifest came from, so a fragment can't diverge
	// from what executes either. No-include manifests pass through
	// byte-identical, so existing single-file workflows are
	// unaffected.
	sha, _ := w.resolveDefaultBranchSHA(w.DefaultBranch())
	workdir := w.WorkDir()
	raw, err := enjuYaml.FlattenIncludes(rb.manifestPath, func(p string) ([]byte, error) {
		data, found, rerr := w.readPinnedRepoFile(sha, workdir, p)
		if rerr != nil {
			return nil, rerr
		}
		if !found {
			return nil, fmt.Errorf("included file %q not found on the default branch or in the worktree", p)
		}
		return data, nil
	})
	if err != nil {
		return nil, err
	}
	parsed, err := enjuYaml.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing workflow %s: %w", rb.manifestPath, err)
	}
	return &LoadedWorkflow{
		Path:      rb.manifestPath,
		BundleDir: rb.bundleDir,
		Raw:       raw,
		Details:   detailsFromManifestBytes(rb.manifestPath, parsed),
		Parsed:    parsed,
	}, nil
}

// LoadTemplate is the deprecated alias for LoadWorkflow, kept so
// callers still on the old name (currently the webui) compile
// unchanged. Internally returns a LoadedTemplate adapted from the
// LoadedWorkflow result.
//
// Deprecated: use LoadWorkflow.
func (w *Workflow) LoadTemplate(repoRelPath string) (*LoadedTemplate, error) {
	loaded, err := w.LoadWorkflow(repoRelPath)
	if err != nil {
		return nil, err
	}
	return &LoadedTemplate{
		Path:      loaded.Path,
		BundleDir: loaded.BundleDir,
		Raw:       loaded.Raw,
		Summary: TemplateSummary{
			Path:        loaded.Details.Path,
			Name:        loaded.Details.Name,
			Description: loaded.Details.Description,
			Params:      loaded.Details.Params,
		},
		Parsed: loaded.Parsed,
	}, nil
}

// readBundleManifest reads the workflow YAML, prioritizing the
// default-branch tree (the reproducible source) but error-ing
// loudly when the worktree has uncommitted changes to the same
// path. Returns a populated resolvedBundle or a clear "not
// found" / "uncommitted-changes" error.
//
// Why error on divergence: path= mode pins the run branch to
// the default branch's tip at create_run time, and execution
// reads scripts + sibling files from that committed tree. If we
// silently used the committed YAML for validation, then later
// executed against the same committed tree, the user's
// uncommitted enju.yaml edits would be invisible — and they'd
// see "args: is required" or similar bewildering errors because
// THE YAML THEY EDITED ISN'T THE YAML THAT RAN. Pre-fix on
// showcase_v16 surfaced exactly this trap. Fail loudly here so
// the operator commits first (or switches to yaml=<inline>).
func (w *Workflow) readBundleManifest(defaultBranch, bundleDir, manifestPath, workdir string) (*resolvedBundle, error) {
	sha, err := w.resolveDefaultBranchSHA(defaultBranch)
	if err != nil {
		return nil, err
	}
	data, found, rerr := w.readPinnedRepoFile(sha, workdir, manifestPath)
	if rerr != nil {
		return nil, rerr
	}
	if !found {
		return nil, fmt.Errorf("workflow %q not found on default branch or in worktree — check `enju_list_workflows` for available YAML files", manifestPath)
	}
	return &resolvedBundle{
		bundleDir:    bundleDir,
		manifestPath: manifestPath,
		manifest:     data,
	}, nil
}

// readPinnedRepoFile reads ONE repo-relative file the reproducible
// way path= runs require, and is the single implementation of that
// contract — used for the workflow manifest AND for every file an
// `include:` directive pulls in, so a fragment can't diverge from
// what executes any more than the manifest can.
//
// Ladder: prefer the default-branch commit (sha); if the same path
// also exists in the worktree with different bytes, the operator
// has uncommitted edits and we refuse loudly (validating against
// bytes that won't be the bytes that execute is the showcase_v16
// trap); fall back to the worktree only when the file isn't
// committed at all (first-time authoring UX). found=false means the
// file is nowhere — the caller decides whether that is fatal (the
// manifest: yes; an include: yes; both with their own message).
func (w *Workflow) readPinnedRepoFile(sha, workdir, repoRelPath string) ([]byte, bool, error) {
	if sha != "" {
		data, found, rerr := w.git.ReadFileAtCommit(sha, repoRelPath)
		if rerr != nil && !errors.Is(rerr, git.ErrCommitNotFound) {
			return nil, false, fmt.Errorf("reading %s: %w", repoRelPath, rerr)
		}
		if found {
			if workdir != "" {
				wtData, fsErr := os.ReadFile(filepath.Join(workdir, repoRelPath))
				if fsErr == nil && !bytes.Equal(wtData, data) {
					return nil, false, fmt.Errorf("%s has uncommitted changes — the worktree differs from the default-branch commit, but path= runs execute against the committed tree. Commit your edits first (`git add %s && git commit`) or pass yaml=<inline content> to use the worktree version verbatim", repoRelPath, repoRelPath)
				}
			}
			return data, true, nil
		}
	}
	// Worktree fallback for pre-commit authoring UX — when the
	// file ISN'T on the default branch at all (first-time setup).
	// The divergence guard above only fires when both versions
	// exist; first-time create_run with an uncommitted YAML still
	// works.
	if workdir != "" {
		data, fsErr := os.ReadFile(filepath.Join(workdir, repoRelPath))
		if fsErr == nil {
			return data, true, nil
		}
		if !os.IsNotExist(fsErr) {
			return nil, false, fmt.Errorf("reading %s from worktree: %w", repoRelPath, fsErr)
		}
	}
	return nil, false, nil
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
	// Enumerate via git, not a raw filesystem walk. `git ls-files
	// --cached --others --exclude-standard` is exactly the set git
	// itself would stage: tracked files plus untracked files that
	// are NOT gitignored. This is the only correct scoping when
	// bundleDir is "" (root-level workflow YAML) — the old
	// filepath.Walk swept the entire project (gitignored databases,
	// raw reads, the lot) into `git add`, which then rejected the
	// ignored paths; and it crashed on a directory symlink because
	// Walk lstats, so a dir-symlink read as a file. ls-files reads
	// the index (no statting the multi-GB data tree) and honors
	// .gitignore, the global excludes, and .git/info/exclude
	// natively. Symlinks still need filtering — see below.
	rels, lerr := w.git.ListBundleFiles(bundleDir)
	if lerr != nil {
		return "", fmt.Errorf("enjugit: EnsureBundleOnDefault: %w", lerr)
	}
	files, cerr := collectBundleFiles(workDir, rels)
	if cerr != nil {
		return "", fmt.Errorf("enjugit: EnsureBundleOnDefault: %w", cerr)
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

// isRuntimeOrInternalPath reports whether a repo-relative slash
// path is enju runtime state (.enju/) or git internals (.git/).
// Used by collectBundleFiles to keep a *recipe bundle* free of
// either, regardless of git tracked-status: task result history
// is committed under .enju/runs/... by design via plumbing (which
// bypasses .gitignore), so ls-files --cached surfaces it; .git/
// is defensive belt-and-suspenders.
//
// NOT reusable for MaterializeRunRepo, despite the surface
// symmetry. .enju/ is overloaded: besides accreted result
// history it also holds THIS run's own pinned template snapshot
// (RunTemplateSnapshotDir → .enju/runs/<seq>/template-snapshot/),
// and delivering that frozen recipe to scripts via $ENJU_REPO_DIR
// is the entire point of MaterializeRunRepo. A blanket .enju/
// exclusion there throws away the run's own recipe (caught by
// TestMaterializeRunRepo_WholeTreeIncludingBaseAndTemplate). A
// recipe bundle is the source tree and never the snapshot area,
// so the same blanket rule is correct here but wrong there;
// separating "this run's snapshot (keep)" from "result history
// (drop)" in the materialized tree is a real per-seq design
// task, not a shared one-liner.
func isRuntimeOrInternalPath(rel string) bool {
	return rel == ".enju" || strings.HasPrefix(rel, ".enju/") ||
		rel == ".git" || strings.HasPrefix(rel, ".git/")
}

// collectBundleFiles turns the git-enumerated repo-relative paths
// (tracked + untracked-not-ignored — gitignore already applied by
// ListBundleFiles) into the FileWrite list the commit takes. It is
// the load-bearing half of the ISSUE-2 fix and is split out so the
// exclusions are unit-pinned independently of git:
//
//   - Symlinks are never pinned: a recipe bundle is enju.yaml +
//     scripts/prompts (text); a symlink under it points at external
//     data (databases, raw reads), and os.ReadFile on a directory
//     symlink would crash — the `current -> checkv-db-v1.5` case.
//
//   - .enju/ and .git/ are NEVER pinned, unconditionally —
//     regardless of git tracked-status. ListBundleFiles' --cached
//     arm lists *tracked* files even if .gitignored, and enju's own
//     run bookkeeping (.enju/runs/<seq>/…/context.json|result.md|
//     script.log) can become tracked on the default branch when a
//     task's iteration branch is merged through (the deeper ISSUE-2
//     root cause — runtime state leaking into history). Pinning
//     that back into the next bundle snowballs the snapshot. A
//     recipe bundle must never contain runtime state or git
//     internals, full stop — this restores (as an explicit filter)
//     the .git/.enju SkipDir the pre-ls-files walker had.
func collectBundleFiles(workDir string, rels []string) ([]FileWrite, error) {
	var files []FileWrite
	for _, rel := range rels {
		if isRuntimeOrInternalPath(rel) {
			continue // runtime state / git internals — never a recipe
		}
		abs := filepath.Join(workDir, filepath.FromSlash(rel))
		fi, lstatErr := os.Lstat(abs)
		if lstatErr != nil {
			// TOCTOU: ListBundleFiles enumerated, then we Lstat —
			// a file vanishing in between aborts the whole bundle
			// commit. Acceptable: template bundles are small,
			// static, operator-authored, and not concurrently
			// mutated during create_run (the old filepath.Walk
			// likewise propagated per-entry errors fatally).
			return nil, fmt.Errorf("stat %s: %w", rel, lstatErr)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			continue // never pin a symlink (see func doc)
		}
		if fi.IsDir() {
			continue // ls-files lists files only; defensive
		}
		body, rerr := os.ReadFile(abs)
		if rerr != nil {
			return nil, fmt.Errorf("read %s: %w", rel, rerr)
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
	}
	return files, nil
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

// resolveBundlePathShape classifies a caller-supplied path into
// (bundleDir, manifestPath) on shape alone — no tree or
// filesystem check. So both tree and filesystem fallback paths
// share the same classification logic.
//
// Accepted shapes:
//   - Manifest form: a path ending in a .yaml/.yml extension is
//     the workflow YAML itself; its directory is the bundle.
//     A bare ".yaml" at the repo root resolves to bundleDir="."
//     (the project root).
//   - Dir form: any other path is treated as a directory
//     containing a default-named manifest (enju.yaml).
func resolveBundlePathShape(repoRelPath string) (bundleDir, manifestPath string, err error) {
	pth := strings.TrimSuffix(repoRelPath, "/")
	if pth == "" {
		return "", "", fmt.Errorf("template path is empty")
	}
	// Manifest form: any *.yaml / *.yml file is the workflow YAML.
	// Its containing directory is the bundle.
	if strings.HasSuffix(pth, ".yaml") || strings.HasSuffix(pth, ".yml") {
		dir := filepath.ToSlash(filepath.Dir(pth))
		if dir == "." {
			dir = ""
		}
		return dir, pth, nil
	}
	// Dir form: <pth>/<BundleManifestName>.
	return pth, pth + "/" + corelayout.BundleManifestName, nil
}
