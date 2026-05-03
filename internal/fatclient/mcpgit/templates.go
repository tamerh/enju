package mcpgit

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	corelayout "github.com/enju-ai/enju/internal/common/layout"
	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Template discovery and instantiation on the fat-client side.
// A template is a DIRECTORY containing an `enju.yaml` manifest
// at its root:
//
//   enju/templates/
//     gwas-analysis/
//       enju.yaml           ← the run definition (required)
//       scripts/            ← bundled scripts referenced by compute tasks
//       examples/           ← sample outputs, ignored by the loader
//       README.md           ← author docs, ignored by the loader
//
// Everything else in the bundle — scripts, data files, docs,
// examples — travels with the template when it's instantiated
// into a run (see the snapshot-on-instantiate flow driven from
// handleCreateRun). This makes templates self-contained and
// makes runs reproducible: a live template edit after a run
// was created can't retroactively change that run's behavior
// because the run owns a frozen copy.
//
// Loose `.yaml` files directly under a templates root are not
// recognized — they'd be ambiguous about whether they own the
// surrounding directory. Templates must live in their own
// folder with an `enju.yaml` inside.
//
// Discovery path: corelayout.DefaultTemplatesDir (enju/templates/)
// by default, overridable via enju/conf.yaml's `templates:` list
// for monorepos with existing config/ or workflows/ conventions.
//
// Source-of-truth invariant: all template reads go through the
// default branch's git tree, NOT the worktree filesystem. This
// means `enju_list_templates` returns the same set regardless of
// which run branch the workspace happens to be checked out on —
// a citizen claiming a task on run-42's branch can still discover
// templates that were authored on main. The alternative
// (filesystem reads against p.workDir) coupled discovery to
// workspace state and broke as soon as create_run switched
// branches. Pre-commit authoring UX is preserved because
// EnsureBundleOnDefault commits the bundle to default before any
// run branch is cut.

// TemplateSummary is the lightweight shape returned by
// ListTemplates — enough for an LLM to pick a template from a
// menu without having to parse the full YAML of each one.
// When a template file fails to parse, the summary still shows
// up in the list with ParseError populated; the caller can see
// the path + the reason without having to drill in via
// describe_template to discover why it's missing.
type TemplateSummary struct {
	Path        string         `json:"path"`                  // repo-relative, e.g. "enju/templates/gwas/enju.yaml"
	Name        string         `json:"name,omitempty"`        // from `name:` field
	Description string         `json:"description,omitempty"` // from `description:` field
	Params      []ParamSummary `json:"params,omitempty"`      // short param summary
	ParseError  string         `json:"parse_error,omitempty"` // set when the template YAML failed to decode/validate
}

// ParamSummary is the per-param shape embedded in a
// TemplateSummary. It's a compressed view of the YAML
// ParamDef — just the fields the LLM needs when deciding
// whether a template fits a user's request.
type ParamSummary struct {
	Name        string      `json:"name"`
	Type        string      `json:"type"`
	Required    bool        `json:"required,omitempty"`
	Default     interface{} `json:"default,omitempty"`
	Description string      `json:"description,omitempty"`
}

// defaultBranchTree resolves the default branch tip commit and
// returns its root tree. Returns (nil, nil) when the default
// branch has no commits yet — callers treat that as "no
// templates visible, not an error" so fresh repos don't spew
// errors before the first commit lands. Prefers the local ref
// (canonical after pull) and falls back to refs/remotes/origin
// so a just-cloned workspace can discover templates before its
// first explicit pull.
func (p *Project) defaultBranchTree() (*object.Tree, error) {
	b := p.defaultBranchOr()
	var hash plumbing.Hash
	if ref, err := p.repo.Reference(plumbing.NewBranchReferenceName(b), true); err == nil {
		hash = ref.Hash()
	} else if ref, err := p.repo.Reference(plumbing.NewRemoteReferenceName("origin", b), true); err == nil {
		hash = ref.Hash()
	} else {
		return nil, nil
	}
	commit, err := p.repo.CommitObject(hash)
	if err != nil {
		return nil, fmt.Errorf("loading default-branch commit %s: %w", hash, err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("loading default-branch tree: %w", err)
	}
	return tree, nil
}

// treeSubTree descends `tree` along the slash-separated `path`
// and returns the subtree. Returns (nil, false, nil) when any
// segment is missing or resolves to a blob instead of a tree —
// callers use the bool to distinguish "directory absent" from
// real errors.
func treeSubTree(tree *object.Tree, path string) (*object.Tree, bool, error) {
	path = strings.Trim(path, "/")
	if path == "" {
		return tree, true, nil
	}
	sub, err := tree.Tree(path)
	if err == object.ErrDirectoryNotFound {
		return nil, false, nil
	}
	if err != nil {
		// ErrEntryNotFound (tree.Tree returns this when the
		// entry exists but isn't a subtree) — treat as absent.
		return nil, false, nil
	}
	return sub, true, nil
}

// treeReadBlob reads the blob at `path` within `tree`. Returns
// (nil, false, nil) when the path is absent.
func treeReadBlob(tree *object.Tree, path string) ([]byte, bool, error) {
	path = strings.TrimLeft(path, "/")
	file, err := tree.File(path)
	if err == object.ErrFileNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("looking up %s: %w", path, err)
	}
	contents, err := file.Contents()
	if err != nil {
		return nil, false, fmt.Errorf("reading %s: %w", path, err)
	}
	return []byte(contents), true, nil
}

// ListTemplates scans every configured template root for
// directory-shaped bundles and returns a summary for each one.
// Roots come from enju/conf.yaml's `templates:` list if present,
// otherwise fall back to corelayout.DefaultTemplatesDir. Empty or
// missing roots are a normal state, not an error — just
// contribute zero entries.
//
// Directories without an enju.yaml are skipped silently
// (scratch folders, README-only dirs, etc). Bundles whose
// enju.yaml fails to parse are surfaced with ParseError
// populated — a visible "unparseable" menu entry beats a
// silent drop that makes the author think the scan missed
// their template.
//
// Loose `.yaml` files directly under a templates root are NOT
// discovered. If any are found, they're surfaced as a single
// migration-hint entry in the result so the author knows to
// move them.
func (p *Project) ListTemplates() ([]TemplateSummary, error) {
	roots, err := p.templateRoots()
	if err != nil {
		return nil, err
	}
	tree, err := p.defaultBranchTree()
	if err != nil {
		return nil, err
	}
	if tree == nil {
		return nil, nil
	}
	var out []TemplateSummary
	seen := make(map[string]bool)
	for _, root := range roots {
		items, err := p.scanTemplateRoot(tree, root)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if seen[it.Path] {
				continue // same bundle reachable from overlapping roots
			}
			seen[it.Path] = true
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// scanTemplateRoot walks one template root in the default-branch
// tree and returns the bundles it finds. Split out so
// ListTemplates can fan over multiple configured roots.
func (p *Project) scanTemplateRoot(tree *object.Tree, root string) ([]TemplateSummary, error) {
	rootTree, ok, err := treeSubTree(tree, root)
	if err != nil {
		return nil, fmt.Errorf("reading templates directory %s: %w", root, err)
	}
	if !ok {
		return nil, nil
	}
	var out []TemplateSummary
	for _, e := range rootTree.Entries {
		name := e.Name
		if !e.Mode.IsFile() && e.Mode != 0 {
			// Directory (subtree) — check for the bundle manifest inside.
			if subTree, err := rootTree.Tree(name); err == nil {
				if _, ok, _ := treeReadBlob(subTree, corelayout.BundleManifestName); ok {
					rel := filepath.ToSlash(filepath.Join(root, name, corelayout.BundleManifestName))
					summary, err := p.templateSummaryFromTree(tree, rel)
					if err != nil {
						out = append(out, TemplateSummary{
							Path:       rel,
							ParseError: err.Error(),
						})
						continue
					}
					out = append(out, *summary)
				}
				// Missing manifest → not a template bundle. Skip silently.
				continue
			}
			// Tree lookup failed — fall through to file handling below.
		}
		// File entry at the root. Catch the legacy single-file
		// shape and emit exactly one actionable migration hint per
		// offending file — silently skipping would leave the
		// author with an empty menu and no clue why.
		if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
			legacyRel := filepath.ToSlash(filepath.Join(root, name))
			base := strings.TrimSuffix(strings.TrimSuffix(name, ".yaml"), ".yml")
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

// templateSummaryFromTree reads one template manifest from the
// default-branch tree and returns its compressed view. Used by
// ListTemplates and as a building block for LoadTemplate.
func (p *Project) templateSummaryFromTree(tree *object.Tree, repoRelPath string) (*TemplateSummary, error) {
	data, ok, err := treeReadBlob(tree, repoRelPath)
	if err != nil {
		return nil, fmt.Errorf("reading template %s: %w", repoRelPath, err)
	}
	if !ok {
		return nil, fmt.Errorf("template %s not found on default branch", repoRelPath)
	}
	parsed, err := enjuYaml.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parsing template %s: %w", repoRelPath, err)
	}
	return &TemplateSummary{
		Path:        repoRelPath,
		Name:        parsed.Run.Name,
		Description: parsed.Run.Description,
		Params:      paramSummaries(parsed.Run.Params),
	}, nil
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

// LoadedTemplate is the full parsed view of a template
// bundle, returned by LoadTemplate. Path is the
// repo-relative path of the manifest YAML; BundleDir is the
// enclosing directory, used by the snapshot-on-instantiate
// flow to enumerate every file to copy.
type LoadedTemplate struct {
	Path      string // e.g. "enju/templates/gwas/enju.yaml"
	BundleDir string // e.g. "enju/templates/gwas"
	Raw       []byte
	Summary   TemplateSummary
}

// ReadBundleFiles walks the bundle directory in the default-branch
// tree and returns every regular blob as a FileWrite, with
// repo-relative paths rebased to a target directory (typically
// the per-run snapshot location produced by
// corelayout.RunTemplateSnapshotDir). Used by handleCreateRun to
// commit the bundle into the run's snapshot area at
// instantiation time, locking the recipe + its scripts to the
// moment the run was created.
//
// Reading from the tree — not the worktree filesystem — keeps
// snapshots deterministic: whatever is committed to default is
// what gets frozen into the run, independent of any uncommitted
// edits that might be sitting in the worktree when create_run
// fires. EnsureBundleOnDefault is responsible for landing the
// bundle on default before this runs.
//
// Size guard: if the bundle exceeds 10 MB total, return an
// error — templates aren't the place for large data blobs, and
// a runaway snapshot would bloat every subsequent run commit.
func (p *Project) ReadBundleFiles(bundleDir, targetDir string) ([]FileWrite, error) {
	tree, err := p.defaultBranchTree()
	if err != nil {
		return nil, err
	}
	if tree == nil {
		return nil, fmt.Errorf("default branch has no commits; commit the template bundle before instantiating")
	}
	bundleTree, ok, err := treeSubTree(tree, bundleDir)
	if err != nil {
		return nil, fmt.Errorf("reading bundle %s: %w", bundleDir, err)
	}
	if !ok {
		return nil, fmt.Errorf("bundle %q not found on default branch", bundleDir)
	}
	const maxBundleBytes = 10 * 1024 * 1024
	var totalBytes int64
	var files []FileWrite
	walker := object.NewTreeWalker(bundleTree, true, nil)
	defer walker.Close()
	for {
		name, entry, err := walker.Next()
		if err != nil {
			break
		}
		if !entry.Mode.IsFile() {
			continue
		}
		// Skip hidden dirs — .git (nested submodule checkouts)
		// would pull a huge history into the snapshot. The tree
		// walker is recursive, so filter on the full path.
		skip := false
		for _, seg := range strings.Split(name, "/") {
			if strings.HasPrefix(seg, ".") {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		blob, err := p.repo.BlobObject(entry.Hash)
		if err != nil {
			return nil, fmt.Errorf("loading blob for %s: %w", name, err)
		}
		totalBytes += blob.Size
		if totalBytes > maxBundleBytes {
			return nil, fmt.Errorf("bundle %q exceeds %d-byte size limit; templates shouldn't carry large data blobs", bundleDir, maxBundleBytes)
		}
		reader, err := blob.Reader()
		if err != nil {
			return nil, fmt.Errorf("opening blob for %s: %w", name, err)
		}
		body := make([]byte, blob.Size)
		if _, err := readFullFromBlob(reader, body); err != nil {
			reader.Close()
			return nil, fmt.Errorf("reading blob for %s: %w", name, err)
		}
		reader.Close()
		// Preserve executable bit from the tree entry mode.
		// Scripts committed with +x must stay executable in the
		// snapshot — otherwise the executor resolves `script:`
		// to a non-executable file and the task fails with
		// "permission denied" on run.
		mode := os.FileMode(0o644)
		if entry.Mode == 0o100755 {
			mode = 0o755
		}
		files = append(files, FileWrite{
			RepoRelPath: filepath.ToSlash(filepath.Join(targetDir, name)),
			Content:     body,
			Mode:        mode,
		})
	}
	return files, nil
}

// readFullFromBlob reads exactly len(buf) bytes from r into buf.
// go-git's blob Reader doesn't satisfy io.ReadFull semantics in
// all versions, so we hand-roll the loop.
func readFullFromBlob(r interface{ Read([]byte) (int, error) }, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			if total == len(buf) {
				return total, nil
			}
			return total, err
		}
	}
	return total, nil
}

// LoadTemplate reads a template bundle by either:
//   - its directory path (e.g. "enju/templates/gwas-analysis")
//   - the full path to its manifest (e.g.
//     "enju/templates/gwas-analysis/enju.yaml")
//
// Both forms resolve to the same bundle; the loader picks up
// the enju.yaml at the directory root. The resolved YAML path
// is what shows up in the returned LoadedTemplate.Path;
// BundleDir carries the surrounding directory so callers doing
// snapshot-on-instantiate can enumerate all the files in the
// bundle.
func (p *Project) LoadTemplate(repoRelPath string) (*LoadedTemplate, error) {
	// Block path escapes — user-controlled input even though
	// it's read from the local workspace, and a `../` could
	// let a caller pull files from outside the configured
	// templates roots.
	clean := filepath.ToSlash(filepath.Clean(repoRelPath))
	if strings.Contains(clean, "../") || clean != repoRelPath {
		return nil, fmt.Errorf("template path %q contains disallowed path components", repoRelPath)
	}
	if err := p.assertUnderTemplatesRoot(repoRelPath); err != nil {
		return nil, err
	}
	tree, err := p.defaultBranchTree()
	if err != nil {
		return nil, err
	}

	// Tree-first, worktree-fallback: the primary source of
	// truth is the default-branch tree (so LoadTemplate works
	// from any workspace branch). But create_run's
	// author-on-disk UX — a user writes enju.yaml into the
	// worktree without committing, then calls create_run which
	// EnsureBundleOnDefault auto-commits — needs a read path
	// for uncommitted files too. We check tree first, then fall
	// back to the worktree. Discovery (ListTemplates) stays
	// tree-only so list results don't vary with workspace
	// state, but single-template load tolerates the
	// pre-commit case.
	rb, err := p.resolveAndReadBundle(tree, repoRelPath)
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

// assertUnderTemplatesRoot confirms the caller-supplied path
// lives under at least one of the configured templates roots.
// Cheap containment check — the individual file reads still
// validate existence — but it keeps the error message
// consistent ("must live under <configured roots>") when a
// caller types a typo'd path.
func (p *Project) assertUnderTemplatesRoot(repoRelPath string) error {
	roots, err := p.templateRoots()
	if err != nil {
		return err
	}
	for _, r := range roots {
		if repoRelPath == r || strings.HasPrefix(repoRelPath, r+"/") {
			return nil
		}
	}
	return fmt.Errorf("template path %q must live under one of: %s", repoRelPath, strings.Join(roots, ", "))
}

// resolvedBundle is the output of resolveAndReadBundle: the
// caller-supplied path, classified and paired with the manifest
// bytes we just read.
type resolvedBundle struct {
	bundleDir    string
	manifestPath string
	manifest     []byte
}

// resolveAndReadBundle resolves a caller-supplied template
// reference to a resolvedBundle in one pass: tree first, then
// worktree filesystem as fallback. The filesystem fallback
// supports the author-on-disk UX — a user writes enju.yaml
// into their worktree and calls create_run;
// EnsureBundleOnDefault commits the bundle before the run
// actually branches off. Without the fallback, that flow would
// break because the tree wouldn't have the template yet.
func (p *Project) resolveAndReadBundle(tree *object.Tree, repoRelPath string) (*resolvedBundle, error) {
	bundleDir, manifestPath, err := p.resolveBundlePathShape(repoRelPath)
	if err != nil {
		return nil, err
	}
	if tree != nil {
		if data, ok, rerr := treeReadBlob(tree, manifestPath); rerr != nil {
			return nil, fmt.Errorf("reading template %s: %w", manifestPath, rerr)
		} else if ok {
			return &resolvedBundle{bundleDir: bundleDir, manifestPath: manifestPath, manifest: data}, nil
		}
	}
	// Worktree fallback — tolerate uncommitted authoring.
	data, fsErr := os.ReadFile(filepath.Join(p.workDir, manifestPath))
	if fsErr == nil {
		return &resolvedBundle{bundleDir: bundleDir, manifestPath: manifestPath, manifest: data}, nil
	}
	if !os.IsNotExist(fsErr) {
		return nil, fmt.Errorf("reading template %s: %w", manifestPath, fsErr)
	}
	return nil, fmt.Errorf("template %q not found on default branch or in worktree — check `enju_list_templates` for available recipes", repoRelPath)
}

// resolveBundlePathShape classifies the caller-supplied path
// into (bundleDir, manifestPath) based on shape only — no tree
// or filesystem check. Lets both tree-read and
// filesystem-fallback loaders share the same classification
// logic.
func (p *Project) resolveBundlePathShape(repoRelPath string) (bundleDir, manifestPath string, err error) {
	pth := strings.TrimSuffix(repoRelPath, "/")
	// Manifest form: ends in /<BundleManifestName>.
	if strings.HasSuffix(pth, "/"+corelayout.BundleManifestName) {
		bundleDir = strings.TrimSuffix(pth, "/"+corelayout.BundleManifestName)
		// Disallow manifest sitting directly in the templates
		// root (bundleDir would equal the root itself).
		roots, rerr := p.templateRoots()
		if rerr != nil {
			return "", "", rerr
		}
		for _, r := range roots {
			if bundleDir == r {
				return "", "", fmt.Errorf("template manifest must live inside a bundle subdirectory, e.g. %s/NAME/%s", r, corelayout.BundleManifestName)
			}
		}
		return bundleDir, pth, nil
	}
	// Dir form: must be a directory with the manifest inside.
	if strings.HasSuffix(pth, ".yaml") || strings.HasSuffix(pth, ".yml") {
		// Legacy single-file reference — emit a migration hint.
		base := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(pth), ".yaml"), ".yml")
		parentDir := filepath.ToSlash(filepath.Dir(pth))
		return "", "", fmt.Errorf(
			"legacy single-file template path %q — templates are now directory bundles. "+
				"Move %s to %s/%s/%s and reference it as %s/%s (or the full manifest path)",
			repoRelPath, repoRelPath, parentDir, base, corelayout.BundleManifestName, parentDir, base)
	}
	return pth, pth + "/" + corelayout.BundleManifestName, nil
}

// InstantiateTemplate loads a template, substitutes the
// supplied param values, and returns the fully-resolved run
// ready for the normal submit path. Errors from
// ParseWithParams (missing required params, type mismatches,
// unknown param names) bubble up with their natural-language
// phrasing so the LLM can forward them to the user.
func (p *Project) InstantiateTemplate(repoRelPath string, params map[string]interface{}) (*enjuYaml.ParsedRun, []byte, error) {
	loaded, err := p.LoadTemplate(repoRelPath)
	if err != nil {
		return nil, nil, err
	}
	parsed, err := enjuYaml.ParseWithParams(loaded.Raw, params)
	if err != nil {
		return nil, nil, err
	}
	return parsed, loaded.Raw, nil
}

// ValidateTemplateParams runs the ParseWithParams path without
// producing a run — useful as a dry-run from the LLM side
// before the user commits to submission. Returns nil if the
// param set is valid; returns the natural-language error
// otherwise.
func (p *Project) ValidateTemplateParams(repoRelPath string, params map[string]interface{}) error {
	_, _, err := p.InstantiateTemplate(repoRelPath, params)
	return err
}
