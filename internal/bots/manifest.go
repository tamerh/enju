// Package bots owns the project-local bot roster: parsing
// enju/bots.yaml, resolving conventional defaults (credentials
// paths, system-prompt paths), and validating the manifest before
// any runtime tool acts on it.
//
// Coordinator-side scope: NONE. The coordinator knows bots only
// as citizens with kind=bot — registration, attribution, terminate
// cascade. The manifest is a fatclient-local declaration of "what
// bots does this project use, with what runtime configuration,"
// read by `enju bot setup` and `enju bot run`. Same status as
// .gitignore: lives in repo, used by the local tool, opaque to
// the server.
//
// Distribution: ships with example projects so a clone + setup +
// run sequence brings a working bot roster up in three commands.
package bots

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	corelayout "github.com/enju-ai/enju/internal/common/layout"
	"github.com/enju-ai/enju/internal/common/gitignore"
	yamlv3 "gopkg.in/yaml.v3"
)

// Manifest is the parsed enju/bots.yaml. Aside from the bot
// list itself, the manifest is intentionally minimal — top-level
// project config (model defaults, prompt-dir overrides) belongs
// in conf.yaml, not here.
//
// Version: schema version. Currently 1. Older manifests without
// the field default to 1 for forward compatibility, but new
// authoring should always set it explicitly so older readers
// can detect-and-warn when a future schema bump arrives. Other
// Enju YAML files (run.yaml, enju.yaml template manifests) all
// carry version: 1 — the bots manifest matches the convention.
type Manifest struct {
	Version int   `yaml:"version,omitempty"`
	Bots    []Bot `yaml:"bots"`

	// ProjectID is the coordinator-assigned project id this
	// manifest's bots belong to. Optional — when set, `enju bot
	// setup` auto-adds each registered bot to the project's
	// membership so the daemon can read /projects/{id}/runs and
	// /tasks/ready scoped to this project. When empty, setup
	// skips the membership step and the operator must add bots
	// manually via enju_add_project_member.
	//
	// Why optional: manifests are committed to git and shared
	// across operators / coord instances; project_id is
	// instance-specific (different coord = different ids), so
	// hard-coding it in the committed manifest is brittle. The
	// recommended pattern is to omit it from the committed
	// manifest and pass --project-id=N to `enju bot setup` per
	// machine, OR to commit it for projects with a stable
	// single coord (solo dev, fixed deployment).
	ProjectID int64 `yaml:"project_id,omitempty"`
}

// Bot is one entry in the manifest. Every field has either a
// required value (validated at parse time) or a conventional
// default the resolver fills in (so manifests stay terse for the
// common case).
type Bot struct {
	// Name is the citizen username this bot binds to. Must
	// match the bot's enju citizen row exactly — the manifest
	// is descriptive, not authoritative; identity lives in
	// the coordinator. Required, unique within the manifest.
	Name string `yaml:"name"`

	// Model is the LLM model id the runner passes to the
	// invocation backend (e.g. "claude-sonnet-4-6"). Carried
	// straight through to `claude -p --model=...` and recorded
	// on every submission via the existing model-attribution
	// path. Required.
	Model string `yaml:"model"`

	// SystemPrompt is a repo-relative path to the bot's system
	// prompt file. Read at runtime and concatenated with the
	// per-task prompt; not committed into the daemon's binary.
	// Default convention: enju/prompts/<name>.md (filled in by
	// Resolve when the manifest leaves the field empty).
	SystemPrompt string `yaml:"system_prompt,omitempty"`

	// MCPTools declares which tools the bot's MCP host should
	// expose to the LLM. The runner spawns `enju mcp` with this
	// allowlist via the --allow-tools flag, so the LLM
	// physically cannot call tools outside the list — the
	// allowlist is pinned at process boundary, not enforced by
	// the coord. See docs for the three-layer trust model
	// (manifest declares, runner pins, audit log records).
	//
	// Pointer (vs value) so we can distinguish "section omitted"
	// (nil → all tools, default) from "explicit allow: []"
	// (empty list → validation error). See MCPTools doc for
	// the rationale on rejecting the explicit-empty case.
	MCPTools *MCPTools `yaml:"mcp_tools,omitempty"`

	// Credentials is the absolute path to the per-bot
	// credentials.json (the same shape `enju mcp` writes). The
	// runner loads this to authenticate the daemon as this bot
	// against the coord. Default convention:
	// ~/.enju/credentials/<name>.json. Tilde expansion happens
	// in Resolve.
	Credentials string `yaml:"credentials,omitempty"`

	// Handler picks which Handler implementation the daemon
	// instantiates for this bot. Bots aren't necessarily LLMs:
	// a "linter-bot" might run a deterministic command, a
	// "review-by-rules" bot might match commit metadata.
	// Implementations are registered in handler.go's NewHandler
	// switch.
	//
	// Empty = "claude" (back-compat with pre-Phase-7.2 manifests
	// where every bot was implicitly an LLM via `claude -p`).
	// Validate rejects unknown values so a typo'd handler type
	// surfaces before the daemon starts.
	Handler string `yaml:"handler,omitempty"`
}

// MCPTools holds the per-bot tool allowlist the runner pins on
// the MCP host the LLM talks to.
//
// Three states matter, distinguished via *MCPTools (pointer):
//
//   - mcp_tools section omitted entirely (Bot.MCPTools == nil) →
//     all tools. Backwards-compatible default; matches `enju mcp`
//     without --allow-tools.
//   - mcp_tools.allow: [tool1, tool2] → exactly those tools.
//   - mcp_tools.allow: [] → validation error. An explicit empty
//     allowlist is almost certainly a mistake (the user wrote
//     "no tools" thinking it'd be safer; it would actually leave
//     the bot unable to call anything). We refuse to interpret
//     the surprising case both ways: if you mean "all," omit the
//     section; if you mean "none of these," that's a bot with
//     no purpose. Validate-error stops the foot-gun.
//
// The pointer is the only reasonable way to distinguish "omitted"
// from "present-with-empty-list" in Go's yaml.v3 unmarshal — both
// land as the zero value of a value-typed field. With *MCPTools,
// nil ↔ omitted and &MCPTools{Allow: []} ↔ explicit-empty.
type MCPTools struct {
	Allow []string `yaml:"allow,omitempty"`
}

// Load reads enju/bots.yaml from a project directory. A missing
// file is a normal state and returns (nil, nil) — projects
// without bots are valid. Returns a parse error for malformed
// YAML so the failure surfaces loudly instead of silently
// pretending no bots exist.
//
// projectRoot is the absolute path to the project's working
// tree (the directory containing enju/, .git/, etc.). The
// caller — `enju bot setup` or `enju bot run` — typically
// passes os.Getwd().
func Load(projectRoot string) (*Manifest, error) {
	path := filepath.Join(projectRoot, corelayout.BotManifestPath)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", corelayout.BotManifestPath, err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	var m Manifest
	if err := yamlv3.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", corelayout.BotManifestPath, err)
	}
	if err := m.Resolve(); err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Resolve fills in the conventional defaults for fields the
// manifest left empty. Idempotent: running it twice produces the
// same output (defaults that look like authored values stay
// stable). Errors only on conditions that block default
// resolution itself (e.g. tilde expansion when no $HOME is set).
func (m *Manifest) Resolve() error {
	// Backwards-compat: pre-version-field manifests default to
	// version 1. Once a v2 ships, Validate will refuse newer
	// versions so the daemon doesn't silently misinterpret a
	// schema change.
	if m.Version == 0 {
		m.Version = 1
	}
	home, _ := os.UserHomeDir()
	for i := range m.Bots {
		b := &m.Bots[i]
		// Default system_prompt path: enju/prompts/<name>.md.
		// Authoring rule for users: name your prompt files
		// after the bot and the manifest doesn't need to
		// repeat the path.
		if b.SystemPrompt == "" && b.Name != "" {
			b.SystemPrompt = filepath.ToSlash(filepath.Join(corelayout.BotPromptsDir, b.Name+".md"))
		}
		// Default credentials path: ~/.enju/credentials/<name>.json.
		// One file per bot, in the same parent dir as the
		// human's credentials.json — shared discovery rules.
		if b.Credentials == "" && b.Name != "" {
			if home == "" {
				return fmt.Errorf("bot %q: credentials path defaulted but no home directory available — set credentials: explicitly", b.Name)
			}
			b.Credentials = filepath.Join(home, ".enju", "credentials", b.Name+".json")
		}
		// Tilde expansion on user-supplied paths. Only the
		// leading "~/" form — anything more elaborate is the
		// user's problem to write as an absolute path.
		if strings.HasPrefix(b.Credentials, "~/") {
			if home == "" {
				return fmt.Errorf("bot %q: credentials path uses ~ but no home directory available", b.Name)
			}
			b.Credentials = filepath.Join(home, strings.TrimPrefix(b.Credentials, "~/"))
		}
	}
	return nil
}

// Validate checks the manifest's invariants after Resolve has
// run. Each bot must have a name + model; names must be unique;
// every credentials path must be absolute (Resolve handled the
// tilde, anything else is the user's mistake).
//
// Tool-allowlist validation is intentionally loose — the runner
// will hand the list to `enju mcp --allow-tools=...` which
// silently drops names it doesn't recognize. We could cross-
// reference against enjumcp.All() here for a friendlier error,
// but that would make this package depend on the MCP tool
// catalog. Defer until we hit a real misconfiguration in the
// wild.
func (m *Manifest) Validate() error {
	if m == nil {
		return nil
	}
	// Schema-version gate. We only know v1 today; refuse
	// anything else loudly so a future schema bump won't be
	// silently misread as v1 (which could ignore new required
	// fields).
	if m.Version != 1 {
		return fmt.Errorf("unsupported manifest version %d (this binary supports version: 1)", m.Version)
	}
	seen := make(map[string]struct{}, len(m.Bots))
	for i, b := range m.Bots {
		if b.Name == "" {
			return fmt.Errorf("bots[%d]: name is required", i)
		}
		if !validBotName(b.Name) {
			return fmt.Errorf("bots[%d]: name %q must contain only ASCII letters/digits/dash/underscore (it becomes a citizen username + a default file name)", i, b.Name)
		}
		if _, dup := seen[b.Name]; dup {
			return fmt.Errorf("bots[%d]: duplicate name %q (each bot must have a unique name in the manifest)", i, b.Name)
		}
		seen[b.Name] = struct{}{}
		// Handler discriminator: empty (=claude back-compat),
		// "claude", or "stub". Future handlers extend this list
		// here AND in NewHandler — keep them in sync.
		switch HandlerType(b.Handler) {
		case "", HandlerTypeClaude, HandlerTypeStub:
			// ok
		default:
			return fmt.Errorf("bot %q: unknown handler %q (supported: claude, stub)", b.Name, b.Handler)
		}
		// Model is required only for handlers that drive an LLM.
		// A future ShellHandler / RuleHandler bot has nothing to
		// pass --model to; insisting on it there would be cargo.
		needsModel := b.Handler == "" || HandlerType(b.Handler) == HandlerTypeClaude
		if needsModel && b.Model == "" {
			return fmt.Errorf("bot %q: model is required for handler %q (e.g. \"claude-sonnet-4-6\")", b.Name, b.Handler)
		}
		if b.SystemPrompt != "" {
			// system_prompt is repo-relative; reject absolute
			// or .. paths that would escape the project root.
			// The runtime read happens against projectRoot +
			// system_prompt; a malicious or accidental escape
			// is better caught here than at file-open time.
			if filepath.IsAbs(b.SystemPrompt) || strings.Contains(b.SystemPrompt, "..") {
				return fmt.Errorf("bot %q: system_prompt %q must be a repo-relative path without ..", b.Name, b.SystemPrompt)
			}
		}
		if !filepath.IsAbs(b.Credentials) {
			return fmt.Errorf("bot %q: credentials path %q must be absolute (or use a leading ~/ which Resolve expands)", b.Name, b.Credentials)
		}
		// Reject explicit empty allowlists. See MCPTools doc
		// for the rationale: present-but-empty is almost
		// always a foot-gun (user wrote "no tools" expecting
		// it'd be safer; that bot can't do anything useful).
		// Force an explicit choice: omit the section for
		// "all" or list at least one tool.
		if b.MCPTools != nil && len(b.MCPTools.Allow) == 0 {
			return fmt.Errorf("bot %q: mcp_tools.allow is present but empty — omit the section to allow all tools, or list at least one tool name", b.Name)
		}
	}
	return nil
}

// ByName returns the bot with the given name, or nil if absent.
// O(n) — fine for the manifest sizes we expect (single digits).
func (m *Manifest) ByName(name string) *Bot {
	if m == nil {
		return nil
	}
	for i := range m.Bots {
		if m.Bots[i].Name == name {
			return &m.Bots[i]
		}
	}
	return nil
}

// EnsureGitignored ensures the project's .gitignore lists the
// machine-managed bot directories inside the existing enju-managed
// block. Called by `enju bot setup` so the operator doesn't have
// to remember the gitignore step manually. Three entries land in
// the block:
//
//   - enju/bots/      — per-bot worktrees (transient runtime)
//   - enju/.bare.git/ — bot push target bare (per-machine local)
//   - enju/.clone/    — bot's managed clone (per-machine local)
//
// All three are local-only state, never something to commit. The
// bare and clone are dot-prefixed so they don't clutter `ls enju/`
// alongside the human-curated templates and bots.yaml.
//
// Returns (changed=true, nil) when the file was updated; (false,
// nil) when all paths were already present. Errors only on real
// I/O failure.
//
// Implementation note: directory paths get a trailing slash so
// .gitignore matches the directory specifically rather than (also)
// a sibling file with the same name — a typo'd plain file at
// `enju/bots` should NOT be silently ignored.
func EnsureGitignored(projectRoot string) (bool, error) {
	gitignorePath := filepath.Join(projectRoot, ".gitignore")
	existing, err := os.ReadFile(gitignorePath)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("reading %s: %w", gitignorePath, err)
	}
	// Preserve the existing file's mode if it's already on disk.
	// A user with a hardened 0600 .gitignore (rare but possible
	// for repos with sensitive ignore patterns) shouldn't see
	// it relax to 0644 just because we appended a managed-block
	// entry. New files default to 0644 (the umask-typical
	// gitignore mode).
	mode := os.FileMode(0644)
	if st, statErr := os.Stat(gitignorePath); statErr == nil {
		mode = st.Mode().Perm()
	}
	updated, changed := gitignore.UpdateManagedBlock(existing, []string{
		corelayout.BotsRuntimeDir + "/",
		corelayout.BotPushTargetDir + "/",
		corelayout.BotCloneDir + "/",
	})
	if !changed {
		return false, nil
	}
	if err := os.WriteFile(gitignorePath, updated, mode); err != nil {
		return false, fmt.Errorf("writing %s: %w", gitignorePath, err)
	}
	return true, nil
}

// validBotName allows ASCII letters, digits, dash, underscore.
// Same character set the citizen username field accepts. Keeps
// bot names safe to use as file-name components (credentials
// path, default prompt path, log file path) without escaping.
func validBotName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}
