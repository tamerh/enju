package yaml

// Modular workflows: a flat top-level `include:` directive that
// splices sibling fragment files into one document BEFORE the
// existing parse/validate pipeline runs. The rest of the system
// (coord blob, snapshot, DAG, reproducibility pinning) only ever
// sees the flattened result, so this is purely a pre-parse pass and
// nothing downstream changes.
//
// Behavior:
//   - Flat merge only. `include: [a.yaml, b.yaml]` concatenates the
//     fragments' tasks/params/bots into the entry document. There is
//     no inheritance/override form.
//   - Hard error on any cross-file collision (duplicate task id /
//     param name / bot name), naming both source files. No silent
//     override, no auto-namespacing.
//   - Singletons (name/version/description/for_each/sync/
//     requirements/auto_triage) belong to the ENTRY file only. An
//     included fragment that sets one is an error: a fragment is a
//     bag of tasks/params/bots, not a workflow.
//   - Bundle scope: every resolved include must stay within the
//     entry file's directory subtree (the directory captured in the
//     run snapshot). Escaping it (`../shared/x.yaml`) is rejected so
//     the run stays reproducible from the snapshot alone.
//   - No-include passthrough is BYTE-IDENTICAL: a workflow without an
//     `include:` key returns its original bytes untouched, so every
//     existing single-file workflow (and its comments/formatting) is
//     completely unaffected.

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"
)

// FlattenFile reads path and resolves any `include:` directive
// against the filesystem (each include relative to the including
// file's directory). This is the bytes ParseFile feeds to Parse —
// exposed so the `enju validate` and `enju go --dry-run` paths,
// which read the file themselves, share one include implementation
// instead of each re-deriving it.
//
// The FS-backed reader additionally resolves symlinks before
// admitting a file: FlattenIncludes' scope guard is lexical
// (path.Clean does not follow links), so a symlink inside the
// bundle pointing outside it would otherwise let validate /
// --dry-run succeed on a workflow whose run-create path resolves
// differently (there the reader is a git tree, where a symlink is
// just its target string, not the pointed-at content — already
// safe). Keeping validate honest about what actually runs is the
// same stance as the uncommitted-divergence guard, so we enforce
// the real-path containment here rather than only documenting it.
func FlattenFile(p string) ([]byte, error) {
	// Deliberate fail-open: if the entry dir can't be real-path
	// resolved (it was just read from, so this is near-impossible)
	// realEntryDir stays "" and the containment check is skipped.
	// The guard is best-effort reproducibility hygiene, not a
	// security boundary — includes are operator-authored committed
	// files, so the trust model is the real boundary (same stance
	// as the handler-anchor path).
	realEntryDir := ""
	if d, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		realEntryDir = d
	}
	confined := func(q string) ([]byte, error) {
		// Resolve symlinks BEFORE reading so an escaping include's
		// pointed-at bytes are never touched. EvalSymlinks fails on
		// a missing/broken path — fall through to os.ReadFile so a
		// genuinely-absent include still yields the resolver's
		// clean not-found message rather than a symlink error.
		if realEntryDir != "" {
			if rq, eerr := filepath.EvalSymlinks(q); eerr == nil &&
				rq != realEntryDir && !strings.HasPrefix(rq, realEntryDir+string(filepath.Separator)) {
				return nil, fmt.Errorf("%s resolves via symlink outside the workflow directory %s — includes must stay inside it so the run snapshot is reproducible", q, realEntryDir)
			}
		}
		return os.ReadFile(q)
	}
	return FlattenIncludes(p, confined)
}

// includeKey is the directive name. listMergeKeys are the top-level
// sequences a fragment may contribute; everything else a fragment
// must not set (it's an entry-only singleton or unknown).
const includeKey = "include"

// listMergeKeys maps a mergeable top-level key to the child field
// whose value must be unique across all files (the collision key).
var listMergeKeys = map[string]string{
	"tasks":  "id",
	"params": "name",
	"bots":   "name",
}

// FlattenIncludes resolves a flat `include:` directive in the entry
// workflow into a single YAML document.
//
// entryPath is the slash path of the assembly file (repo-relative
// for the run-create path, an fs path for `enju validate`). read(p)
// returns the raw bytes of a file at slash-path p — the caller binds
// it to the filesystem, a git tree, etc. All include paths resolve
// relative to the *including* file's directory and must stay within
// entryPath's directory.
//
// The scope guard here is LEXICAL (path.Clean; symlinks are not
// followed). That is sufficient for the git-tree reader (a symlink
// is stored as its target string, not the pointed-at content). The
// FS-backed reader (FlattenFile) layers real-path symlink
// resolution on top so disk and git-tree readers agree on what is
// in-bundle.
//
// Returns the entry's original bytes unchanged when there is no
// `include:` key (byte-identical passthrough). Otherwise returns the
// flattened document with a provenance header comment.
func FlattenIncludes(entryPath string, read func(path string) ([]byte, error)) ([]byte, error) {
	entryRaw, err := read(entryPath)
	if err != nil {
		return nil, fmt.Errorf("reading workflow %s: %w", entryPath, err)
	}
	hasInc, herr := hasIncludeKey(entryRaw)
	if herr != nil {
		return nil, fmt.Errorf("parsing %s: %w", entryPath, herr)
	}
	if !hasInc {
		return entryRaw, nil // untouched — existing single-file path
	}

	entryDir := path.Dir(path.Clean(filepathToSlash(entryPath)))
	acc := &includeAcc{
		seq:   map[string]*yamlv3.Node{},
		owner: map[string]map[string]string{},
		order: nil,
	}
	visiting := map[string]bool{}
	sources := []string{}
	if err := acc.mergeFile(entryPath, entryRaw, true, entryDir, visiting, &sources, read); err != nil {
		return nil, err
	}
	return acc.marshal(sources)
}

// includeAcc accumulates the merged document across files.
type includeAcc struct {
	// singletons: entry-only top-level keys, keyed by name, in
	// first-seen order via `order`. In practice every singleton
	// comes from the entry file (a fragment setting one is an
	// error), so `order` is always just the entry's key order —
	// the ordering machinery is defensive, not load-bearing.
	singletons map[string]*yamlv3.Node
	order      []string
	// seq: merged sequence nodes per list key (tasks/params/bots).
	seq map[string]*yamlv3.Node
	// owner[listKey][collisionValue] = source file that defined it
	// (for the duplicate-naming error message).
	owner map[string]map[string]string
}

// preRead, when non-nil, is the already-read bytes of p (the entry
// file — FlattenIncludes has read it once for the include-key
// probe). Recursive include calls pass nil and read on demand.
func (a *includeAcc) mergeFile(p string, preRead []byte, isEntry bool, entryDir string, visiting map[string]bool, sources *[]string, read func(string) ([]byte, error)) error {
	cp := path.Clean(filepathToSlash(p))
	if visiting[cp] {
		return fmt.Errorf("include cycle detected at %s", p)
	}
	visiting[cp] = true
	defer delete(visiting, cp)
	*sources = append(*sources, cp)

	data := preRead
	if data == nil {
		var err error
		if data, err = read(p); err != nil {
			return fmt.Errorf("reading included %s: %w", p, err)
		}
	}

	root, err := topMapping(data)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", p, err)
	}
	if root == nil {
		return nil // empty document — nothing to contribute
	}

	if a.singletons == nil {
		a.singletons = map[string]*yamlv3.Node{}
	}

	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i].Value
		val := root.Content[i+1]

		switch {
		case key == includeKey:
			if val.Kind != yamlv3.SequenceNode {
				return fmt.Errorf("%s: `include:` must be a list of file paths", p)
			}
			for _, inc := range val.Content {
				if inc.Kind != yamlv3.ScalarNode || inc.Value == "" {
					return fmt.Errorf("%s: `include:` entries must be non-empty path strings", p)
				}
				childAbs := path.Clean(path.Join(path.Dir(cp), inc.Value))
				if !withinDir(entryDir, childAbs) {
					return fmt.Errorf("%s: include %q escapes the workflow directory %q (includes must stay inside it so the run snapshot is reproducible)", p, inc.Value, entryDir)
				}
				if err := a.mergeFile(childAbs, nil, false, entryDir, visiting, sources, read); err != nil {
					return err
				}
			}

		case listMergeKeys[key] != "":
			if val.Kind != yamlv3.SequenceNode {
				return fmt.Errorf("%s: `%s:` must be a list", p, key)
			}
			if a.seq[key] == nil {
				a.seq[key] = &yamlv3.Node{Kind: yamlv3.SequenceNode, Tag: "!!seq"}
				a.owner[key] = map[string]string{}
			}
			idField := listMergeKeys[key]
			for _, item := range val.Content {
				cv := scalarChild(item, idField)
				// Division of labor: the include layer only owns
				// CROSS-FILE collisions. An item missing its id/
				// name entirely isn't collision-checked here — it
				// flows to the downstream validator, which already
				// reports the "ID/name is required" presence error.
				if cv != "" {
					if prev, dup := a.owner[key][cv]; dup {
						return fmt.Errorf("duplicate %s %s=%q: defined in %s and %s", strings.TrimSuffix(key, "s"), idField, cv, prev, cp)
					}
					a.owner[key][cv] = cp
				}
				a.seq[key].Content = append(a.seq[key].Content, item)
			}

		default:
			// Singleton / unknown top-level key. Allowed only in
			// the entry file — a fragment is tasks/params/bots.
			if !isEntry {
				return fmt.Errorf("%s: an included fragment may only set include/tasks/params/bots, not %q (that's a workflow-level setting — keep it in the entry file)", p, key)
			}
			if _, seen := a.singletons[key]; !seen {
				a.order = append(a.order, key)
			}
			a.singletons[key] = val
		}
	}
	return nil
}

// marshal rebuilds one document: entry singletons in first-seen
// order, then the merged params/bots/tasks sequences. A provenance
// header records the flatten sources.
func (a *includeAcc) marshal(sources []string) ([]byte, error) {
	root := &yamlv3.Node{Kind: yamlv3.MappingNode, Tag: "!!map"}
	put := func(k string, v *yamlv3.Node) {
		root.Content = append(root.Content,
			&yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!str", Value: k}, v)
	}
	for _, k := range a.order {
		put(k, a.singletons[k])
	}
	// Stable order for the merged sequences so the flattened
	// document is deterministic regardless of map iteration.
	for _, k := range []string{"params", "bots", "tasks"} {
		if a.seq[k] != nil && len(a.seq[k].Content) > 0 {
			put(k, a.seq[k])
		}
	}
	doc := &yamlv3.Node{Kind: yamlv3.DocumentNode, Content: []*yamlv3.Node{root}}
	body, err := yamlv3.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("re-emitting flattened workflow: %w", err)
	}
	sorted := append([]string(nil), sources...)
	sort.Strings(sorted)
	header := "# Flattened by enju from: " + strings.Join(sorted, " + ") + "\n" +
		"# (generated — edit the source files, not this.)\n"
	return append([]byte(header), body...), nil
}

// --- small helpers ---

// hasIncludeKey reports whether the top-level mapping has an
// `include:` key, without committing to a full parse.
func hasIncludeKey(data []byte) (bool, error) {
	root, err := topMapping(data)
	if err != nil || root == nil {
		return false, err
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == includeKey {
			return true, nil
		}
	}
	return false, nil
}

// topMapping decodes data and returns its top-level mapping node
// (nil for an empty document).
func topMapping(data []byte) (*yamlv3.Node, error) {
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return nil, nil
	}
	root := doc.Content[0]
	if root.Kind != yamlv3.MappingNode {
		return nil, fmt.Errorf("expected a YAML mapping at the top level")
	}
	return root, nil
}

// scalarChild returns the scalar value of mapping child `field`,
// or "" if absent / non-scalar.
func scalarChild(m *yamlv3.Node, field string) string {
	if m == nil || m.Kind != yamlv3.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == field && m.Content[i+1].Kind == yamlv3.ScalarNode {
			return m.Content[i+1].Value
		}
	}
	return ""
}

// withinDir reports whether child (a cleaned slash path) is dir
// itself or lies inside it — the bundle-scope guard.
func withinDir(dir, child string) bool {
	if dir == "." || dir == "" {
		return !strings.HasPrefix(child, "../") && child != ".."
	}
	return child == dir || strings.HasPrefix(child, dir+"/")
}

// filepathToSlash normalizes OS separators so the resolver's path
// math is slash-only regardless of the caller's path style.
func filepathToSlash(p string) string {
	return strings.ReplaceAll(p, "\\", "/")
}
