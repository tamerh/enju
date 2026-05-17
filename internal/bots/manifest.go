// Package bots owns the project-local bot roster: parsing the
// inline `bots:` section from a workflow YAML, resolving
// conventional defaults (credentials paths, system-prompt
// paths), and validating the manifest before any runtime tool
// acts on it.
//
// Coordinator-side scope: NONE. The coordinator knows bots only
// as citizens with kind=bot — registration, attribution, terminate
// cascade. The manifest is a fatclient-local declaration of "what
// bots does this workflow use, with what runtime configuration,"
// read by `enju bot setup` and `enju bot run` against the workflow
// YAML the operator passes via --workflow. Same status as
// .gitignore: lives in the repo, used by the local tool, opaque
// to the server.
package bots

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	corelayout "github.com/enju-ai/enju/internal/common/layout"
	"github.com/enju-ai/enju/internal/common/gitignore"
	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	yamlv3 "gopkg.in/yaml.v3"
)

// Manifest is the parsed bot roster. Built from a workflow
// YAML's inline `bots:` section via LoadFromWorkflow /
// FromInlineNode — there's no separate per-project bots.yaml.
// Workflow YAML is the single source of truth.
//
// Version: schema version, defaults to 1 when omitted. Reserved
// for future schema bumps so older readers can detect-and-warn.
type Manifest struct {
	Version int   `yaml:"version,omitempty"`
	Bots    []Bot `yaml:"agents"`
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

	// SystemPrompt is an optional repo-relative path to the
	// bot's system prompt file. Read at runtime and
	// concatenated with the per-task prompt. Empty = no system
	// prompt (the handler runs without one); workflow authors
	// write the explicit path when they want one.
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

	// Handler doubles as the discriminator AND the binary name
	// the SubprocessHandler exec's. Two reserved values get
	// special treatment in NewHandler:
	//   - ""      → "claude" (back-compat default)
	//   - "stub"  → in-process StubHandler (testing)
	//
	// Any other value is the binary name (resolved via $PATH
	// for bare names like "claude" / "gemini", or treated as
	// an absolute / repo-relative path when containing `/`).
	// Adding a new handler = providing a binary that satisfies
	// the protocol; no Go change.
	//
	// Bots aren't necessarily LLMs: a "linter-bot" might run a
	// deterministic command, a "review-by-rules" bot might
	// match commit metadata. Same field, same wiring.
	Handler string `yaml:"handler,omitempty"`

	// Args is the literal argv template the SubprocessHandler
	// passes to the binary. Each entry can carry one or more
	// {{var}} placeholders that get substituted at invoke
	// time. Recognized vars:
	//
	//   {{model}}             Bot.Model field
	//   {{system_prompt}}     Bot.SystemPrompt body (read
	//                         from the file at invoke time)
	//   {{allowed_tools}}     Bot.MCPTools.Allow joined with ","
	//   {{task_id}}           coord task identifier
	//   {{branch}}            run branch name
	//   {{repo_dir}}          path to run snapshot
	//   {{git_dir}}           path to .git/ for history reads
	//   {{scratch}}           writable workspace path
	//   {{review_feedback}}   reviewer prose on iter > 1
	//   {{handler_args.<k>}}  the value at key <k> in the
	//                          merged bot+task handler_args
	//
	// Empty-substitution rule: when a {{var}} resolves to an
	// empty string, the WHOLE arg containing it is dropped from
	// argv. That matches operator intent: a `--model={{model}}`
	// entry should disappear entirely if no model is set,
	// rather than passing `--model=` to the binary.
	//
	// Args that contain no {{var}} are passed through verbatim.
	// Values land as single argv slots via exec.Command — never
	// shell-evaluated, so a `{{var}}` whose value contains
	// `$(rm -rf /)` reaches the subprocess as literal bytes.
	//
	// Example (claude bot):
	//   handler: claude
	//   model: claude-sonnet-4-6
	//   system_prompt: enju/prompts/dev.md
	//   args:
	//     - "-p"
	//     - "--model={{model}}"
	//     - "--append-system-prompt={{system_prompt}}"
	//
	// Example (lint bot, no LLM):
	//   handler: ./bin/lint-bot.sh
	//   args:
	//     - "--format={{handler_args.format}}"
	//   handler_args:
	//     format: json
	Args []string `yaml:"args,omitempty"`

	// HandlerArgs are operator-defined values referenceable from
	// Bot.Args via {{handler_args.<key>}} substitution.
	//
	// Operators who want flags reference the keys explicitly in
	// `args:`:
	//
	//   args:
	//     - "--effort={{handler_args.effort}}"
	//   handler_args:
	//     effort: high
	//
	// There is no enju-side auto-translation from map keys to
	// argv flags — the operator's `args:` template is the only
	// place that decides if/where each entry lands in the
	// spawned command line.
	//
	// Workflow YAML:
	//   handler_args:
	//     effort: high
	//     max-tokens: "8192"
	//     thinking: "true"
	//
	// Used to thread per-bot LLM tuning that varies across
	// providers / versions without requiring a Go change every
	// time. The CLI rejects unknown flags as a normal exec
	// failure with stderr in the audit log; no enju-side
	// validation gates the keys.
	//
	// Per-task overrides come from TaskDef.HandlerArgs and win
	// on collision (TaskDef.HandlerArgs ⨁ Bot.HandlerArgs).
	HandlerArgs map[string]string `yaml:"handler_args,omitempty"`

	// Replicas requests N independently-running copies of this
	// bot — useful when you want parallel work from multiple
	// instances of the same role (three developer bots picking
	// from the same READY queue, fastest claim wins). Optional;
	// absent or 1 means a single bot with the entry's name.
	//
	// On parse, replicas >= 2 expand into N synthetic Bot
	// entries with names suffixed -1, -2, ..., -N. All other
	// fields copy verbatim per replica EXCEPT credentials, which
	// resolve per replica name (one cred file per identity), and
	// the default system prompt, which resolves from the BASE
	// name so all replicas share the same prompt (they're
	// supposed to be identical bots).
	//
	// Capped at 32 to prevent typo'd configs from spawning
	// runaway citizen registrations.
	//
	// Downstream code (Resolve, Validate, Load callers) only
	// sees the expanded Bot entries; the Replicas field is
	// cleared after expansion so this knob can't leak into
	// runtime decisions.
	Replicas int `yaml:"replicas,omitempty"`
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

// LoadFromWorkflow reads a workflow YAML file and returns its
// inline `bots:` section as a Manifest, fully resolved and
// validated. workflowPath is the absolute or repo-relative path
// to the workflow YAML; `enju bot setup` and `enju bot run`
// receive it via --workflow.
//
// Returns (nil, nil) when the workflow YAML has no `bots:` block
// or declares an empty list — projects without bots are valid.
// Returns a parse error for malformed YAML so failures surface
// loudly instead of silently pretending no bots exist.
//
// Workflow YAML is the single source of truth for bot
// definitions. There is no separate per-project bots.yaml file;
// every bot a workflow uses is declared inline alongside the
// tasks that reference it.
func LoadFromWorkflow(workflowPath string) (*Manifest, error) {
	data, err := os.ReadFile(workflowPath)
	if err != nil {
		return nil, fmt.Errorf("reading workflow %s: %w", workflowPath, err)
	}
	parsed, err := enjuYaml.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parsing workflow %s: %w", workflowPath, err)
	}
	if parsed == nil || parsed.Run == nil {
		return nil, nil
	}
	return FromInlineNode(parsed.Run.Bots)
}

// FromInlineNode parses a `bots:` block embedded in a workflow
// YAML's Run struct into a Manifest. The node is the raw
// yamlv3.Node captured by internal/common/yaml.Run.Bots; an
// absent or empty block returns (nil, nil) — workflows without
// bots are valid.
//
// The block is a YAML sequence of Bot entries. FromInlineNode
// wraps it in a Manifest{Bots: ...} and runs the
// expand-resolve-validate pipeline.
//
// Errors: malformed inline content surfaces with a clear
// "inline agents:" prefix.
func FromInlineNode(node yamlv3.Node) (*Manifest, error) {
	// Zero-value Node (no `bots:` key in the workflow YAML) →
	// no inline manifest. Kind==0 is the unset state.
	if node.Kind == 0 {
		return nil, nil
	}
	// Empty sequence (`bots: []`) → explicit "no bots".
	if node.Kind == yamlv3.SequenceNode && len(node.Content) == 0 {
		return nil, nil
	}
	// Refuse `project_id:` at the bot level — it was a top-level
	// Manifest field in the pre-inline standalone bots.yaml and
	// has no semantic effect now. yaml.v3 would silently ignore
	// an unknown key, so an operator copying a stale entry from
	// the old file format would get a no-op. Catch it loudly so
	// they redirect to `--project-id` on the CLI instead.
	if node.Kind == yamlv3.SequenceNode {
		for i, entry := range node.Content {
			if entry.Kind != yamlv3.MappingNode {
				continue
			}
			for j := 0; j < len(entry.Content); j += 2 {
				if entry.Content[j].Value == "project_id" {
					return nil, fmt.Errorf(
						"inline agents[%d]: project_id is no longer accepted in bot manifests — "+
							"pass via --project-id on the CLI (enju bot setup --project-id=N)", i)
				}
			}
		}
	}
	var bots []Bot
	if err := node.Decode(&bots); err != nil {
		return nil, fmt.Errorf("parsing inline agents: %w", err)
	}
	m := &Manifest{
		// Version defaults to 1 (the only known schema) — the
		// inline form intentionally has no `version:` knob since
		// the workflow YAML carries its own version on the Run
		// struct. Validate still gates against unsupported
		// versions when the legacy file path is used.
		Version: 1,
		Bots:    bots,
	}
	if err := m.expandReplicas(); err != nil {
		return nil, fmt.Errorf("inline agents: %w", err)
	}
	if err := m.Resolve(); err != nil {
		return nil, fmt.Errorf("inline agents: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("inline agents: %w", err)
	}
	return m, nil
}

// botReplicasCap bounds the number of synthetic entries a single
// `replicas: N` field can spawn. The cap exists to make typo'd
// configs (replicas: 1000 from a fat finger or a unit confusion)
// fail loudly before they land 1000 citizens on the coord.
const botReplicasCap = 32

// expandReplicas processes the `replicas` field on each manifest
// entry: entries with replicas >= 2 expand into N synthetic Bot
// entries with names suffixed `-1` through `-N`, copying all
// other fields verbatim. Entries with replicas absent or 1 stay
// unchanged. The Replicas field is cleared on every entry post-
// expansion so downstream code never sees it.
//
// Run before Resolve so per-replica defaults (credentials path
// based on the suffixed name) get filled in correctly. The
// system_prompt default is set inline here (against the BASE
// name) so all replicas share one prompt file — the typical
// case for "three developer bots all reading prompts/dev-bot.md."
func (m *Manifest) expandReplicas() error {
	expanded := make([]Bot, 0, len(m.Bots))
	for i, b := range m.Bots {
		switch {
		case b.Replicas < 0:
			return fmt.Errorf("bots[%d] %q: replicas must be >= 1 (got %d); omit the field for a single bot", i, b.Name, b.Replicas)
		case b.Replicas > botReplicasCap:
			return fmt.Errorf("bots[%d] %q: replicas %d exceeds cap of %d (cap exists to catch typo'd configs before they register runaway citizens)", i, b.Name, b.Replicas, botReplicasCap)
		}
		// 0 (absent) or 1 → single entry; clear Replicas so
		// downstream sees a normal Bot.
		if b.Replicas < 2 {
			b.Replicas = 0
			expanded = append(expanded, b)
			continue
		}
		base := b.Name
		// Replicas share the user-authored system_prompt path
		// verbatim (or empty when the family doesn't use one).
		// No auto-default — prompts are explicit-or-omitted.
		sharedPrompt := b.SystemPrompt
		for n := 1; n <= b.Replicas; n++ {
			rep := b
			rep.Replicas = 0
			rep.Name = fmt.Sprintf("%s-%d", base, n)
			rep.SystemPrompt = sharedPrompt
			// Credentials default left empty so Resolve fills
			// per replica name; an explicit user-supplied path
			// is unusual for a replicas: N entry but if present,
			// every replica shares the same credentials file —
			// almost certainly wrong. We leave that as the
			// user's choice; Validate doesn't have a rule against
			// it.
			expanded = append(expanded, rep)
		}
	}
	m.Bots = expanded
	return nil
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
		// SystemPrompt has no auto-default — authors specify
		// the path when they want one, or omit it entirely.
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
		// Handler discriminator. Two reserved values:
		//   - ""      → defaults to "claude" at SubprocessHandler
		//               construction time (back-compat).
		//   - "stub"  → in-process StubHandler for tests; no
		//               binary spawn, no args/model required.
		// Anything else is treated as a binary name or path the
		// SubprocessHandler exec's at claim time. Bare names
		// resolve via $PATH (claude, gemini, …); slash-containing
		// values are absolute or repo-relative paths used
		// verbatim (./bin/lint-bot.sh, /usr/local/bin/foo).
		//
		// The validator deliberately doesn't try to verify the
		// binary exists at load time — the daemon's Preflight
		// does that at startup with the right $PATH context.
		// Manifest-load validation here would force operators to
		// have every bot's binary installed BEFORE editing the
		// YAML; not the right gate.
		//
		// model: required only for the claude handler. Other
		// handlers (gemini, lint scripts, rule-based bots, …)
		// decide for themselves whether they need a `model:`
		// field — typically by whether the args: template
		// references {{model}}. Adding a strict "every handler
		// needs a model" rule would force lint-bots to declare
		// fake models; the rule stays narrow on purpose.
		isStub := HandlerType(b.Handler) == HandlerTypeStub
		isClaude := b.Handler == "" || HandlerType(b.Handler) == HandlerTypeClaude
		if isClaude && b.Model == "" {
			return fmt.Errorf("bot %q: model is required for handler %q (e.g. \"claude-sonnet-4-6\")", b.Name, b.Handler)
		}
		// args: is required for every subprocess handler — without
		// it the binary spawns with zero arguments. The default
		// `claude` invocation in particular reads the prompt from
		// stdin, exits silently in ~20s with no output, and the
		// operator sees a "writes_artifacts missing" failure with
		// no trace of what happened. Stub handler is the lone
		// exception (it returns canned responses without spawning).
		if !isStub && len(b.Args) == 0 {
			return fmt.Errorf("bot %q: args: is required for handler %q — supply the literal argv template the binary expects (claude shape: [\"-p\", \"--model={{model}}\", \"--append-system-prompt={{system_prompt}}\", \"--allowedTools={{allowed_tools}}\"]). See example_bots/bots.yaml or docs/handler-protocol.md.", b.Name, b.Handler)
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
		// Validate each Args entry's {{var}} placeholders. Catches
		// typo'd static-var references AND malformed brace
		// syntax at manifest load — otherwise these surface
		// only at first claim, after the daemon's started and
		// a task is in flight. Review-fix #3 + #10.
		for j, a := range b.Args {
			if err := ValidateArgsTemplate(a); err != nil {
				return fmt.Errorf("bot %q: args[%d] %q: %w", b.Name, j, a, err)
			}
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
// machine-managed enju cache directories inside the existing
// enju-managed block. Called by `enju bot setup` so the operator
// doesn't have to remember the gitignore step manually.
//
// Post-Phase-8.h: the .enju/ umbrella holds BOTH tracked audit
// files (`.enju/runs/<seq>-<slug>/<task>/{result.md,metadata.json,
// context.json,script.log}` and `.enju/runs/<seq>-<slug>/template-
// snapshot/`) AND gitignored caches/scratch (snapshot/, bots/,
// bigfiles/, events/, logs/). Git's gitignore semantics don't let
// us "exclude .enju/ but re-include specific files" — once a parent
// directory is excluded, git skips traversal entirely. So instead
// of one umbrella entry, we list each cache subdirectory explicitly.
// Audit files live alongside in the same tree and stay tracked
// because none of the gitignore rules match them.
//
// Cache subdirs ignored:
//   - .enju/runs/*/snapshot/   per-run on-disk materialization
//   - .enju/bots/              per-bot per-claim scratch
//   - .enju/bigfiles/          track:false compute outputs
//   - .enju/events/            project-level event log
//   - .enju/logs/              per-clone trace logs
//   - .enju/scratch/           legacy / future generic scratch
//
// Returns (changed=true, nil) when the file was updated; (false,
// nil) when all paths were already present. Errors only on real
// I/O failure.
//
// Implementation note: directory paths get a trailing slash so
// .gitignore matches the directory specifically rather than (also)
// a sibling file with the same name.
func EnsureGitignored(projectRoot string) (bool, error) {
	gitignorePath := filepath.Join(projectRoot, ".gitignore")
	existing, err := os.ReadFile(gitignorePath)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("reading %s: %w", gitignorePath, err)
	}
	mode := os.FileMode(0644)
	if st, statErr := os.Stat(gitignorePath); statErr == nil {
		mode = st.Mode().Perm()
	}
	updated, changed := gitignore.UpdateManagedBlock(existing, []string{
		corelayout.StateDirRoot + "/runs/*/snapshot/",
		corelayout.StateDirRoot + "/agents/",
		corelayout.StateDirRoot + "/bigfiles/",
		corelayout.StateDirRoot + "/events/",
		corelayout.StateDirRoot + "/logs/",
		corelayout.StateDirRoot + "/scratch/",
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
