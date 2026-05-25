package yaml

// Validation. Given a decoded Run (already passed through
// resolveDefaults), validate() enforces the schema invariants
// and returns any non-fatal warnings. It composes narrow
// sub-validators (one per concern) so adding a new check is a
// localized edit, not a scroll through a 400-line function.
//
// A note on mutation: pure defaulting (e.g. Action="answer")
// lives in resolveDefaults in parse.go; validators here don't
// fill missing fields. HOWEVER, two validators still mutate:
//
//   - validateDependsOnReferences auto-appends reviews-target
//     to depends_on so review tasks always run after their
//     target.
//   - injectReviewGating + injectVoteActivation auto-insert
//     the review-waits-on-target and activated-waits-on-vote
//     edges that authors would otherwise have to hand-write on
//     every downstream task.
//
// These are DAG-correctness derivations — the validated Run
// wouldn't be semantically complete without them — not
// defaulting. The function names (Inject, Derive) signal the
// mutation at every call site.

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/common/artifactpath"
	"github.com/enju-ai/enju/internal/common/template"
)

// validActions is the set of supported action values. Declared
// at package scope so it's not rebuilt every call.
//
// merge_resolve is a system-spawned action — coord auto-spawns
// these tasks when a non-FF auto-merge hits a content conflict
// (parallel-merge phase 3). YAML authors don't write
// `action: merge_resolve` directly, but the parser must accept
// it on already-loaded run state so a re-parse of a project
// with spawned merge_resolve tasks doesn't fail.
var validActions = map[string]bool{
	"answer":        true,
	"contribute":    true,
	"compute":       true,
	"review":        true,
	"vote":          true,
	"merge_resolve": true,
}

// validate orchestrates every parse-time check on the decoded
// Run. Each sub-step is a named function so readers can see
// the pipeline shape without tracing individual conditions.
// Returns the collected warnings (non-fatal authoring hints)
// plus the first fatal error encountered.
func validate(p *Run) ([]string, error) {
	if err := validateHeader(p); err != nil {
		return nil, err
	}
	if err := validateRunForEach(p); err != nil {
		return nil, err
	}
	paramWarnings, err := validateParams(p)
	if err != nil {
		return nil, err
	}
	if err := validateNoParamForEachCollision(p); err != nil {
		return nil, err
	}
	ids, hasTaskLevelForEach, err := validateTasks(p)
	if err != nil {
		return nil, err
	}
	if err := validateNoDuplicateReviewTargets(p); err != nil {
		return nil, err
	}
	if err := validateForEachScopes(p, hasTaskLevelForEach); err != nil {
		return nil, err
	}
	if err := validateDependsOnReferences(p, ids); err != nil {
		return nil, err
	}
	reviewWarnings := injectReviewGating(p)
	injectVoteActivation(p)
	if err := validateNoParallelSiblingWrites(p); err != nil {
		return nil, err
	}
	if err := validateArtifactDeclarations(p); err != nil {
		return nil, err
	}
	if err := validateTemplateReferences(p, ids); err != nil {
		return nil, err
	}
	if err := validateDynamicForEach(p, ids); err != nil {
		return nil, err
	}
	if err := validatePublishMode(p); err != nil {
		return nil, err
	}
	computeWarnings := validateComputeDependsDeclared(p)
	contentRefWarnings := validateComputeContentRefs(p)
	collectsWarnings := validateCollectsConsumed(p)
	outputRefWarnings := warnUndeclaredOutputFieldRefs(p, ids)
	warnings := append(paramWarnings, reviewWarnings...)
	warnings = append(warnings, computeWarnings...)
	warnings = append(warnings, contentRefWarnings...)
	warnings = append(warnings, collectsWarnings...)
	warnings = append(warnings, outputRefWarnings...)
	// Unknown schema version (L1): version is currently always 1.
	// A higher number means the YAML was authored against a schema
	// this build doesn't know — warn so a future schema roll has a
	// clean upgrade signal instead of silently mis-parsing. Omitted
	// (0) is tolerated as "unspecified, assume 1" and doesn't warn.
	if p.Version != 0 && p.Version != 1 {
		warnings = append(warnings, fmt.Sprintf("unknown schema version %d — this build understands version 1; proceeding as version 1", p.Version))
	}
	return warnings, nil
}

// validateComputeDependsDeclared flags compute tasks that
// declare no visible dependencies — no `{{task.*}}` refs in
// prompt / user_prompt / script, no `reads_artifacts`, no
// `depends_on`. Since compute scripts run opaquely (Enju can't
// inspect what the script actually reads), a task with zero
// declared deps whose script secretly reads another task's
// private `enju/runs/...` output produces a dep-less DAG.
// The scheduler then marks producer and consumer ready in
// parallel, and whichever claimant hits the consumer first
// fails mid-script with a file-not-found that looks unrelated
// to authoring.
//
// Warnings are non-fatal (false positives exist: a truly
// independent compute task reading only config is valid).
// The message is actionable — tell the author what the three
// declaration forms are so they can pick one or dismiss the
// warning knowingly.
//
// Not applied to non-compute actions because their dependency
// surface is parseable (prompts for answer/review/vote,
// template refs for any action). Only compute is opaque
// enough to warrant the structural fallback check.
func validateComputeDependsDeclared(p *Run) []string {
	var warnings []string
	for _, t := range p.Tasks {
		if t.Action != "compute" {
			continue
		}
		// Task-field refs (`{{upstream.content}}`) imply a
		// DAG edge — suppress as before.
		if hasTaskFieldReference(t.Prompt) ||
			hasTaskFieldReference(t.UserPrompt) ||
			hasTaskFieldReference(t.Script) ||
			len(t.ReadsArtifacts) > 0 ||
			len(t.DependsOn) > 0 {
			continue
		}
		// Bare `{{param}}` refs (no dot) indicate the task
		// is parameterized by run-level context — `{{source_repo}}`,
		// `{{file}}` for_each variable, etc. Scripts that
		// reach run context explicitly aren't the class of
		// compute task the lint was designed to catch
		// (stealth-readers of peer task outputs). Suppress to
		// clear the false-positive tester hit on templates
		// that ingest external data via ENJU_PARAM_* /
		// context.json.
		if hasParamReference(t.Prompt) ||
			hasParamReference(t.UserPrompt) ||
			hasParamReference(t.Script) {
			continue
		}
		// Leaf-and-self-contained suppression: if no other
		// task in the run consumes this one's output (no
		// depends_on, no {{X.*}} ref, no review target, no
		// reads_artifacts path match, no dynamic for_each
		// source), the "stealth-reader" risk has no cascade.
		// Even if the script does happen to read external
		// state, only this task fails — there's nothing
		// downstream to break. Prototype / test / one-shot
		// compute tasks are exactly this shape and shouldn't
		// nag the author.
		if !hasDeclaredConsumer(p, t.ID, t.WritesArtifacts.Paths()) {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"compute task %q has no declared dependencies "+
				"(no {{task.*}} refs, no {{param}} refs, no reads, no depends_on). "+
				"If its script reads output from another task, declare that "+
				"dependency explicitly — otherwise the scheduler may run this "+
				"task in parallel with its unknown upstream and a citizen "+
				"will hit a file-not-found error mid-script. "+
				"See docs/task-actions.md § compute — Dependency declaration.",
			t.ID))
	}
	return warnings
}

// hasDeclaredConsumer reports whether any task in the run
// consumes taskID's output via any declared mechanism:
//
//   - `depends_on: [taskID]` — explicit edge
//   - `{{taskID.anything}}` in prompt or user_prompt — resolver
//     injects the upstream result
//   - `reviews: taskID` — review task gates on this one
//   - `reads_artifacts` path that matches one of taskID's
//     writes_artifacts paths — implicit artifact edge
//   - `for_each: var: "{{taskID.field}}"` — dynamic fan-out
//     source
//
// Pure inspection of the parsed Run; no side effects. Used by
// the compute-dep lint to decide whether a "no declared
// upstream" warning would be speculative (no cascade possible)
// or load-bearing (something downstream depends on this).
func hasDeclaredConsumer(p *Run, taskID string, writesArtifactPaths []string) bool {
	writeSet := make(map[string]struct{}, len(writesArtifactPaths))
	for _, w := range writesArtifactPaths {
		if w != "" {
			writeSet[w] = struct{}{}
		}
	}
	refPrefix := "{{" + taskID + "."
	for _, t := range p.Tasks {
		if t.ID == taskID {
			continue
		}
		for _, d := range t.DependsOn {
			if d == taskID {
				return true
			}
		}
		if t.Reviews == taskID {
			return true
		}
		if strings.Contains(t.Prompt, refPrefix) ||
			strings.Contains(t.UserPrompt, refPrefix) {
			return true
		}
		for _, r := range t.ReadsArtifacts {
			if _, ok := writeSet[r]; ok {
				return true
			}
		}
		for _, src := range t.ForEach {
			if src.Ref != "" && strings.Contains(src.Ref, refPrefix) {
				return true
			}
		}
	}
	return false
}

// contentRefPattern matches `{{taskID.content}}` — mirrors the
// resolver's ref pattern (see internal/template/resolve.go)
// except pinned specifically to the `.content` field. No
// whitespace tolerance: the resolver itself doesn't accept
// `{{ x.content }}`, so the lint's coverage matches reality.
var contentRefPattern = regexp.MustCompile(`\{\{(\w+)\.content\}\}`)

// validateComputeContentRefs flags `{{X.content}}` references
// where X is a compute task that declared `writes_artifacts`.
// The resolver will inline X's stdout (what the script printed,
// captured in result.md) — NOT the artifact file's bytes. A
// compute script that emits a status line like
// "aggregated 12345 rows" and writes the real output to a TSV
// will silently surface the status line downstream instead of
// the data, and the author only discovers it when the
// downstream task hallucinates or produces garbage summaries.
//
// Warning names the producer, the declared artifacts, and the
// replacement syntax so the fix is a one-line swap:
//
//	{{aggregate.content}}          → ✗ stdout echo
//	{{artifact:out/totals.tsv}}    → ✓ file bytes
//
// Suppressed when the prompt also contains `{{artifact:<one of
// X's declared paths>}}` — the author clearly knows the
// distinction and is pulling both on purpose (status + data).
// Also suppressed for non-compute upstreams (stdout IS the
// canonical output there) and for compute producers that
// didn't declare any writes_artifacts (stdout really is all
// they have).
func validateComputeContentRefs(p *Run) []string {
	// Build lookup: task ID → declared writes_artifacts paths
	// (only compute tasks with at least one entry).
	computeWrites := map[string][]string{}
	for _, t := range p.Tasks {
		if t.Action == "compute" && len(t.WritesArtifacts) > 0 {
			computeWrites[t.ID] = t.WritesArtifacts.Paths()
		}
	}
	if len(computeWrites) == 0 {
		return nil
	}

	var warnings []string
	for _, t := range p.Tasks {
		// Scan every prompt-shaped string the resolver touches.
		// Script strings aren't resolved, so they stay out of
		// the scan.
		for _, text := range []string{t.Prompt, t.UserPrompt} {
			if text == "" {
				continue
			}
			matches := contentRefPattern.FindAllStringSubmatch(text, -1)
			for _, m := range matches {
				producerID := m[1]
				paths, ok := computeWrites[producerID]
				if !ok {
					continue
				}
				// Suppress if the author already references ANY
				// of the producer's artifacts via
				// {{artifact:path}} in the same prompt — they
				// know the distinction.
				if hasArtifactRefFor(text, paths) {
					continue
				}
				warnings = append(warnings, fmt.Sprintf(
					"task %q: {{%s.content}} references compute task %q's stdout (result.md), "+
						"not its declared writes (%s). "+
						"Most compute scripts write data to artifacts and echo a status line to stdout — "+
						"the downstream will see the status line, not the data. "+
						"To pull the artifact bytes, use {{artifact:%s}} instead "+
						"(resolves through the artifact index; no second depends_on needed). "+
						"See docs/template-reference.md § {{artifact:path}}.",
					t.ID, producerID, producerID,
					strings.Join(paths, ", "),
					paths[0]))
			}
		}
	}
	return warnings
}

// validateCollectsConsumed flags an aggregator (`collects: T`)
// whose collected fan-in is never consumed anywhere — no
// `{{T.<field>}}` / `{{T[*]}}` template ref and no `reads:` of a
// path T writes. Such a task does pointless fan-in waiting and
// surfaces zero content: a footgun the other (hard-error)
// collects rules don't catch because the wiring is structurally
// valid, just useless. Non-fatal hint (escalated by -strict),
// mirroring the compute-content lint's shape.
//
// Conservative by construction — a single reference of ANY kind
// to T (any field, any index, or an artifact read of T's writes)
// suppresses it, so a correctly-wired aggregator never warns. The
// collects-target-exists / target-is-fanned hard errors run
// before warnings, so T is a real, fanned task here.
func validateCollectsConsumed(p *Run) []string {
	var warnings []string
	for _, agg := range p.Tasks {
		target := agg.Aggregates
		if target == "" {
			continue
		}
		// {{target.<field>}} / {{target[...]}} / {{target}} in any
		// prompt-shaped string the resolver touches. No whitespace
		// tolerance — matches the resolver, same as contentRefPattern.
		refRE := regexp.MustCompile(`\{\{` + regexp.QuoteMeta(target) + `(\.|\[|\}\})`)
		// Artifact-read consumption: any task that reads a path
		// the target declares it writes is consuming the fan-in.
		var targetWrites []string
		for _, t := range p.Tasks {
			if t.ID == target {
				targetWrites = t.WritesArtifacts.Paths()
				break
			}
		}
		consumed := false
		for _, t := range p.Tasks {
			for _, text := range []string{t.Prompt, t.UserPrompt} {
				if text != "" && refRE.MatchString(text) {
					consumed = true
					break
				}
			}
			if consumed {
				break
			}
			if len(targetWrites) > 0 {
				for _, r := range t.ReadsArtifacts {
					for _, w := range targetWrites {
						// Compare with template expressions collapsed to a
						// wildcard (M7): the canonical fan-in reads the
						// glob path `data/{{items[*]}}/x` while the target
						// writes the per-iteration `data/{{item}}/x`; both
						// normalize to `data/*/x` and match. Literal paths
						// normalize to themselves, so the direct case is
						// unaffected.
						if r == w || normalizeTemplatePath(r) == normalizeTemplatePath(w) {
							consumed = true
							break
						}
					}
					if consumed {
						break
					}
				}
			}
			if consumed {
				break
			}
		}
		if !consumed {
			warnings = append(warnings, fmt.Sprintf(
				"task %q: collects %q but nothing references the collected fan-in "+
					"(no {{%s.content}} / {{%s.<field>}} / {{%s[*]}} template ref, "+
					"no reads: of a path %q writes) — the aggregator waits on the "+
					"full fan-out and yields zero content. Reference it (e.g. "+
					"{{%s.content}} in this task's prompt) or drop the collects:.",
				agg.ID, target, target, target, target, target, target))
		}
	}
	return warnings
}

// normalizeTemplatePath collapses every `{{...}}` expression in a
// repo-relative path to a single "*" wildcard. Lets the collects
// reachability check (M7) match a glob fan-in read
// (`data/{{items[*]}}/analysis.txt`) against the per-iteration
// write that feeds it (`data/{{item}}/analysis.txt`) — both
// normalize to `data/*/analysis.txt`. Only used to SUPPRESS a
// non-fatal warning, so over-matching is the safe direction.
func normalizeTemplatePath(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); {
		if i+1 < len(p) && p[i] == '{' && p[i+1] == '{' {
			if j := strings.Index(p[i:], "}}"); j >= 0 {
				b.WriteByte('*')
				i += j + 2
				continue
			}
		}
		b.WriteByte(p[i])
		i++
	}
	return b.String()
}

// hasArtifactRefFor reports whether `text` contains a
// `{{artifact:<path>}}` reference for any of the given paths.
// Used to suppress the compute-content lint when the author is
// visibly pulling both stdout (status line) and the artifact
// (data) on purpose.
func hasArtifactRefFor(text string, paths []string) bool {
	for _, p := range paths {
		if strings.Contains(text, "{{artifact:"+p+"}}") {
			return true
		}
	}
	return false
}

// hasParamReference returns true when the string contains a
// bare `{{name}}` reference — no dot inside the braces. Those
// resolve to run-level params or for_each iteration variables
// at creation / materialization time, never to a peer task's
// output. Used by the compute-dep-lint to suppress warnings
// on tasks that visibly reach run context rather than
// stealth-read peer outputs.
func hasParamReference(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] != '{' || s[i+1] != '{' {
			continue
		}
		sawDot := false
		sawContent := false
		for j := i + 2; j+1 < len(s); j++ {
			if s[j] == '}' && s[j+1] == '}' {
				if sawContent && !sawDot {
					return true
				}
				break
			}
			if s[j] == '.' {
				sawDot = true
			}
			sawContent = true
		}
	}
	return false
}

// hasTaskFieldReference returns true when the string contains
// a `{{task.field}}` / `{{task.content}}` / `{{task.responses}}`
// / `{{task.winning_option}}` reference — i.e. anything Enju's
// prompt resolver would translate into a DAG edge. Param-only
// refs (`{{paramname}}`) don't count because those are
// substituted at run creation and don't establish task
// dependencies.
//
// Heuristic: the prompt resolver treats `{{word.anything}}` as
// a task reference (the dot is the discriminator). Plain
// `{{word}}` is a param. So we look for `.` inside `{{ ... }}`.
func hasTaskFieldReference(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] != '{' || s[i+1] != '{' {
			continue
		}
		// Walk to the matching `}}` or end-of-string. Track
		// whether we saw a `.` inside — that's the task-ref
		// marker.
		sawDot := false
		for j := i + 2; j+1 < len(s); j++ {
			if s[j] == '}' && s[j+1] == '}' {
				if sawDot {
					return true
				}
				break
			}
			if s[j] == '.' {
				sawDot = true
			}
		}
	}
	return false
}

// validateHeader checks the minimal "is this a run at all"
// invariants: a name is present, at least one task is declared.
func validateHeader(p *Run) error {
	if p.Name == "" {
		return fmt.Errorf("run name is required")
	}
	if len(p.Tasks) == 0 {
		return fmt.Errorf("at least one task is required")
	}
	return nil
}

// validateRunForEach enforces the run-level for_each shape.
// Run-level supports literal lists AND {{paramname}} refs
// (substituted at ParseWithParams time from a declared
// top-level list<string> param). Task-refs at run-level are
// nonsense (no task context when the run's tasks are being
// built) and rejected with a clear hint about the correct
// shapes.
func validateRunForEach(p *Run) error {
	for name, src := range p.ForEach {
		if err := rejectDoubleUnderscoreForEachVar("run", name); err != nil {
			return err
		}
		switch {
		case src.Ref != "":
			if paramName, ok := parseForEachParamRef(src.Ref); ok {
				// for_each fans out over the elements of a list. A
				// scalar param (string/int/bool) can't drive a
				// fan-out — catch it statically here (M6) instead of
				// letting it pass validate and fail at run
				// materialization with "must be a list (got string)".
				for i := range p.Params {
					if p.Params[i].Name != paramName {
						continue
					}
					if !strings.HasPrefix(p.Params[i].Type, "list<") {
						return fmt.Errorf("run for_each: variable %q references param %q of type %q — for_each requires a list param (list<string> or list<record>)", name, paramName, p.Params[i].Type)
					}
					break
				}
				continue // substitution resolves it later
			}
			if _, _, ok := parseForEachRef(src.Ref); ok {
				return fmt.Errorf("run for_each: variable %q: task references like %q are not supported at run-level — use a static list or a {{paramname}} reference from a top-level param", name, src.Ref)
			}
			return fmt.Errorf("run for_each: variable %q: %q is not a valid template reference (expected \"{{paramname}}\")", name, src.Ref)
		case len(src.Values) == 0:
			return fmt.Errorf("run for_each: variable %q has an empty list — declare at least one value, a {{paramname}} reference, or remove the variable", name)
		default:
			if err := validateForEachLiteralMap("run", map[string][]string{name: src.Values}); err != nil {
				return err
			}
		}
	}
	// Multi-variable for_each with a list<record> source is not yet
	// supported. cartesianProduct only handles []string values and would
	// silently drop the record metadata, leaving {{var.field}} refs
	// unresolved in prompts. Reject now; extend when there's a concrete
	// use case.
	if len(p.ForEach) > 1 {
		paramByName := make(map[string]*ParamDef, len(p.Params))
		for i := range p.Params {
			paramByName[p.Params[i].Name] = &p.Params[i]
		}
		for varName, src := range p.ForEach {
			if paramName, ok := parseForEachParamRef(src.Ref); ok {
				if pd, found := paramByName[paramName]; found && pd.Type == "list<record>" {
					return fmt.Errorf("run for_each: variable %q references list<record> param %q — multi-variable for_each with a list<record> source is not yet supported; use a single for_each variable", varName, paramName)
				}
			}
		}
	}
	return nil
}

// validateNoParamForEachCollision rejects a name declared as
// BOTH a top-level param and a run-level for_each variable.
//
// Why this is its own hard error: param substitution wins the
// {{name}} precedence battle, so the for_each variable gets
// resolved to the param value and is never seen as "referenced".
// The downstream unused-for_each-variable check then fired with
// a flatly false message — `"{{disease}} is declared but never
// referenced in any task prompt — remove it or reference it via
// {{disease}}"` — even though the prompt literally contained
// `{{disease}}`, and it recommended two fixes that both miss the
// real problem. The collision itself is the error (it makes the
// run's intent ambiguous: iterate, or substitute once?), so name
// it directly here, before the misleading heuristic can run.
func validateNoParamForEachCollision(p *Run) error {
	if len(p.ForEach) == 0 || len(p.Params) == 0 {
		return nil
	}
	for i := range p.Params {
		name := p.Params[i].Name
		if _, clash := p.ForEach[name]; clash {
			return fmt.Errorf(
				"%q is declared as BOTH a top-level param and a run-level for_each variable — these collide (param substitution shadows the for_each variable, making the run's intent ambiguous). Rename one of them: a param if you want a single caller-supplied value, or the for_each variable if you want to fan out over a list",
				name,
			)
		}
	}
	return nil
}

// validateParams enforces the top-level params: names are
// unique, types recognized, required/default mutually
// exclusive. Returns non-fatal warnings for params missing a
// description (the LLM needs prose to turn the param into a
// follow-up question for the user).
func validateParams(p *Run) ([]string, error) {
	var warnings []string
	paramNames := make(map[string]bool, len(p.Params))
	for i := range p.Params {
		pp := &p.Params[i]
		if pp.Name == "" {
			return nil, fmt.Errorf("params[%d]: name is required", i)
		}
		if paramNames[pp.Name] {
			return nil, fmt.Errorf("params[%d]: duplicate name %q", i, pp.Name)
		}
		paramNames[pp.Name] = true
		if !isValidParamType(pp.Type) {
			return nil, fmt.Errorf("param %q: invalid type %q (must be string, int, bool, list<string>, or list<record>)", pp.Name, pp.Type)
		}
		if err := validateParamDef(pp); err != nil {
			return nil, err
		}
		if pp.Required && pp.Default != nil {
			return nil, fmt.Errorf("param %q: required and default are mutually exclusive", pp.Name)
		}
		if pp.Default != nil {
			if err := checkParamValueType(pp, pp.Default); err != nil {
				return nil, err
			}
		}
		// The "needs prose" warning only makes sense for a param an
		// LLM/human task actually reads in a prompt — that's when the
		// description becomes a follow-up question. A param used only
		// as a for_each fan-out axis or in compute paths (the canonical
		// pure-compute pipeline) is never turned into a question, so a
		// missing description there is not a defect (L8).
		if pp.Description == "" && paramReferencedInPromptableTask(p, pp.Name) {
			warnings = append(warnings, fmt.Sprintf("param %q has no description — the LLM needs prose to turn this into a question for the user", pp.Name))
		}
	}
	return warnings, nil
}

// paramReferencedInPromptableTask reports whether `name` is
// referenced (as `{{name}}` or `{{name[...]}}`) in the prompt or
// user_prompt of any task whose action surfaces text to a human or
// LLM. compute tasks are excluded — a param consumed only by a
// compute script (via env / paths) or as a for_each axis never
// becomes an LLM question, so its missing description (L8) is not a
// defect. Whitespace-free match, mirroring the prompt resolver.
func paramReferencedInPromptableTask(p *Run, name string) bool {
	bare := "{{" + name + "}}"
	indexed := "{{" + name + "["
	for _, t := range p.Tasks {
		if t.Action == "compute" {
			continue
		}
		for _, text := range []string{t.Prompt, t.UserPrompt} {
			if strings.Contains(text, bare) || strings.Contains(text, indexed) {
				return true
			}
		}
	}
	return false
}

// validateParamDef enforces the list<record>-specific shape rules
// and defaults Key to the first declared field when absent.
// Called for every param after the type is confirmed valid.
func validateParamDef(pp *ParamDef) error {
	if pp.Type == "list<record>" {
		if pp.Fields.Len() == 0 {
			return fmt.Errorf("param %q (list<record>): fields: is required — declare the record shape with field-name: type pairs", pp.Name)
		}
		for _, fname := range pp.Fields.Names() {
			ftype, _ := pp.Fields.TypeOf(fname)
			switch ftype {
			case "string", "int", "bool":
				// ok
			default:
				return fmt.Errorf("param %q fields.%s: unsupported type %q (must be string, int, or bool)", pp.Name, fname, ftype)
			}
			// Double-underscore is the env-var field separator
			// (ENJU_PARAM_<var>__<field>). A field name containing
			// __ would produce a collision with another field's
			// env var slot.
			if strings.Contains(fname, "__") {
				return fmt.Errorf("param %q fields.%s: field names must not contain \"__\" (reserved for env var expansion)", pp.Name, fname)
			}
		}
		if pp.Key != "" {
			if _, ok := pp.Fields.TypeOf(pp.Key); !ok {
				known := pp.Fields.Names()
				return fmt.Errorf("param %q: key: %q is not a declared field (known fields: %s)", pp.Name, pp.Key, strings.Join(known, ", "))
			}
		} else {
			// Default key to the first declared field.
			pp.Key = pp.Fields.FirstName()
		}
	} else {
		if pp.Fields.Len() > 0 {
			return fmt.Errorf("param %q: fields: is only valid on type: list<record> (got %q)", pp.Name, pp.Type)
		}
		if pp.Key != "" {
			return fmt.Errorf("param %q: key: is only valid on type: list<record> (got %q)", pp.Name, pp.Type)
		}
	}
	return nil
}

// validateTasks walks every task def and enforces per-task
// invariants: action is valid, required fields for the action
// are present, vote options / review targets / task-level
// for_each shape are well-formed, result_type is recognized.
//
// Returns the ID set (used downstream by depends_on and
// template-reference validators) and a flag indicating whether
// any task declared a task-level for_each (used by the scope
// validator to enforce run-level/task-level mutual exclusion).
//
// By the time this runs, resolveDefaults has already set
// t.Action = "answer" for tasks that left it blank — so the
// action check below expects every task to have an action.
func validateTasks(p *Run) (ids map[string]bool, hasTaskLevelForEach bool, err error) {
	paramByName := make(map[string]*ParamDef, len(p.Params))
	for i := range p.Params {
		paramByName[p.Params[i].Name] = &p.Params[i]
	}

	ids = make(map[string]bool)
	for i := range p.Tasks {
		t := &p.Tasks[i]

		if t.ID == "" {
			return nil, false, fmt.Errorf("task ID is required")
		}
		if ids[t.ID] {
			return nil, false, fmt.Errorf("duplicate task ID %q", t.ID)
		}
		ids[t.ID] = true

		// merge_resolve is system-only. coord auto-spawns these
		// tasks when a non-FF auto-merge hits a content
		// conflict (parallel-merge phase 3); the validator
		// accepts the action so re-parses of state with spawned
		// tasks don't fail, but a user-authored YAML with
		// `action: merge_resolve` is almost certainly a typo or
		// copy-paste and should be rejected loudly. spawn.go
		// bypasses this validator, so the system path stays
		// open.
		if t.Action == "merge_resolve" {
			return nil, false, fmt.Errorf(
				"task %q: action: merge_resolve is system-only — coord auto-spawns these on a non-FF merge conflict, "+
					"YAML authors don't write them by hand. Did you mean answer or contribute?",
				t.ID)
		}
		if !validActions[t.Action] {
			return nil, false, fmt.Errorf("task %q: invalid action %q (must be answer, contribute, compute, review, or vote)", t.ID, t.Action)
		}

		switch t.Action {
		case "answer", "contribute", "review":
			if t.Prompt == "" {
				return nil, false, fmt.Errorf("task %q: prompt is required for %s action", t.ID, t.Action)
			}
		case "compute":
			if t.Script == "" {
				return nil, false, fmt.Errorf("task %q: script is required for compute action", t.ID)
			}
		}

		if err := validateReviewTarget(t); err != nil {
			return nil, false, err
		}
		if err := validateActionFields(t); err != nil {
			return nil, false, err
		}
		if err := validateTaskEnv(t); err != nil {
			return nil, false, err
		}
		if err := validateTaskMode(t); err != nil {
			return nil, false, err
		}
		if err := validateTaskRetries(t); err != nil {
			return nil, false, err
		}
		if err := validateTaskContainer(t); err != nil {
			return nil, false, err
		}
		if err := validateTaskContainerRuntime(t); err != nil {
			return nil, false, err
		}
		if err := validateTaskVolumes(t); err != nil {
			return nil, false, err
		}
		if err := validateTaskExecutor(t); err != nil {
			return nil, false, err
		}
		if err := validateTaskResources(t); err != nil {
			return nil, false, err
		}

		if t.ResultType != "" && t.ResultType != "text" && t.ResultType != "json" && t.ResultType != "file" {
			return nil, false, fmt.Errorf("task %q: invalid result_type %q", t.ID, t.ResultType)
		}

		if len(t.ForEach) > 0 {
			hasTaskLevelForEach = true
			if err := validateForEachMap("task "+t.ID, t.ForEach); err != nil {
				return nil, false, err
			}
			// list<record> for_each sources are only supported at the
			// run level. Task-level iteration over a record param would
			// require threading record metadata through cartesianProduct
			// and per-task build logic — deferred until there's a real
			// use case. Declare the for_each at the run level instead.
			for varName, src := range t.ForEach {
				if paramName, ok := parseForEachParamRef(src.Ref); ok {
					if pd, found := paramByName[paramName]; found && pd.Type == "list<record>" {
						return nil, false, fmt.Errorf(
							"task %q: for_each variable %q references list<record> param %q — list<record> for_each sources are supported at run level only; declare the for_each at the run level",
							t.ID, varName, paramName,
						)
					}
				}
			}
		}
	}
	return ids, hasTaskLevelForEach, nil
}

// validatePublishMode rejects unknown values in the run-level
// publish: mode: field. Empty (omitted block) is always accepted — it
// defaults to "local" at run-completion time. Validated here so a bad
// mode in the YAML file is caught at create_run, not silently at
// completion.
func validatePublishMode(p *Run) error {
	if p.Publish == nil || p.Publish.Mode == "" {
		return nil
	}
	switch p.Publish.Mode {
	case "none", "local", "push":
		return nil
	default:
		return fmt.Errorf("publish: mode %q is invalid (must be \"none\", \"local\", or \"push\")", p.Publish.Mode)
	}
}

// validateTaskMode enforces the shape of the `mode:` field on a
// task definition: only "sync" or "async", only on action:
// compute. Empty means "default" and is always accepted —
// compute tasks default to sync, non-compute tasks ignore it
// entirely (the field being set without a declared purpose
// would be a template-author confusion, so reject that up front).
func validateTaskMode(t *TaskDef) error {
	if t.Mode == "" {
		return nil
	}
	if t.Action != "compute" {
		return fmt.Errorf("task %q: mode: is only valid on action: compute tasks (got action: %s)", t.ID, t.Action)
	}
	switch t.Mode {
	case "sync", "async":
		return nil
	default:
		return fmt.Errorf("task %q: mode %q is invalid (must be \"sync\" or \"async\")", t.ID, t.Mode)
	}
}

// validateTaskRetries enforces the `retries:` field: non-negative,
// and compute-only (citizen tasks recover via the contract-gate
// verify_retry_cap path, not this one). 0 (the default) is always fine.
func validateTaskRetries(t *TaskDef) error {
	if t.Retries == 0 {
		return nil
	}
	if t.Retries < 0 {
		return fmt.Errorf("task %q: retries must be >= 0 (got %d)", t.ID, t.Retries)
	}
	if t.Action != "compute" {
		return fmt.Errorf("task %q: retries: is only valid on action: compute tasks (got action: %s)", t.ID, t.Action)
	}
	return nil
}

// validateTaskContainer enforces the shape of the `container:`
// field: only valid on action: compute, and the image reference
// must be non-empty when declared (`container: ""` is a typo
// risk, not an intentional "no container" — leave the field out
// entirely to run script-on-host).
//
// The image reference itself is NOT parsed — docker's own CLI
// arbitrates registry/tag/digest validity at pull time, and
// re-implementing that grammar here would drift. We only check
// for empty and for trivially-bad characters (whitespace,
// newlines) that almost always indicate a template-author
// mistake.
func validateTaskContainer(t *TaskDef) error {
	if t.Container == "" {
		return nil
	}
	if t.Action != "compute" {
		return fmt.Errorf("task %q: container: is only valid on action: compute tasks (got action: %s)", t.ID, t.Action)
	}
	if strings.ContainsAny(t.Container, " \t\n\r") {
		return fmt.Errorf("task %q: container %q contains whitespace — image references should be a single token (e.g. biocontainers/samtools:1.18)", t.ID, t.Container)
	}
	return nil
}

// validateTaskContainerRuntime enforces the shape of the
// `container_runtime:` field. Accepts "docker", "apptainer",
// or "singularity" (alias for apptainer, rewritten in place on
// the TaskDef so internal storage + logs always say
// "apptainer"). Empty defaults to docker at execute time.
//
// A runtime selector without a container image is a config
// error — there's nothing for the runtime to execute, so the
// declaration is almost certainly an author mistake (left over
// from removing the image, or typed in advance of adding one).
// Surface it at parse time rather than letting the wrapper hit
// a more confusing failure later.
//
// Only valid on action: compute (runtimes don't apply to
// answer/review/vote tasks).
func validateTaskContainerRuntime(t *TaskDef) error {
	if t.ContainerRuntime == "" {
		return nil
	}
	if t.Action != "compute" {
		return fmt.Errorf("task %q: container_runtime: is only valid on action: compute tasks (got action: %s)", t.ID, t.Action)
	}
	if t.Container == "" {
		return fmt.Errorf("task %q: container_runtime %q is set without container: — declare an image for the runtime to execute (e.g. container: docker://alpine:latest)", t.ID, t.ContainerRuntime)
	}
	if t.ContainerRuntime == "singularity" {
		t.ContainerRuntime = "apptainer"
	}
	switch t.ContainerRuntime {
	case "docker", "apptainer":
		return nil
	default:
		return fmt.Errorf("task %q: container_runtime %q is not supported (valid values: \"docker\", \"apptainer\", \"singularity\")", t.ID, t.ContainerRuntime)
	}
}

// validateTaskVolumes enforces the shape of the `volumes:`
// field — extra host paths bind-mounted into the container.
//
// Rules:
//   - compute-only (mirrors container:/env:/executor:).
//   - requires container: set. A bare-host script already sees
//     the host filesystem, so a volume declaration without a
//     container image is meaningless — almost certainly an
//     author mistake (same reasoning as container_runtime:
//     without container:).
//   - each entry must be a non-empty "host[:container[:options]]"
//     spec: 1–3 colon-separated segments, no whitespace, with
//     a non-empty host segment.
//
// The options segment is NOT interpreted here. Same stance as
// container: — we don't re-implement the runtime's grammar; the
// runtime CLI arbitrates option validity (ro, rw, z, Z, ro,z,
// nocopy, …) at run time, and a bogus option surfaces as a
// clear runtime error. Hardcoding an allowlist would (a) drift
// from docker/apptainer's actual vocabulary and (b) block the
// author from opting into an SELinux relabel (:z/:Z), which is
// the very escape hatch buildDockerArgs leaves them now that
// declared volumes are passed verbatim (see review of ISSUE-4).
//
// Validation runs BEFORE param substitution (see
// parseInternal's pipeline), so entries can still contain raw
// {{param}} tokens here — the structural checks tolerate them,
// and no execute-time re-validation is needed because the
// wrapper passes the resolved spec to the runtime verbatim.
func validateTaskVolumes(t *TaskDef) error {
	if len(t.Volumes) == 0 {
		return nil
	}
	if t.Action != "compute" {
		return fmt.Errorf("task %q: volumes: is only valid on action: compute tasks (got action: %s)", t.ID, t.Action)
	}
	if t.Container == "" {
		return fmt.Errorf("task %q: volumes: is set without container: — extra mounts only apply to containerized tasks; a bare-host script already sees the host filesystem", t.ID)
	}
	for _, v := range t.Volumes {
		if v == "" {
			return fmt.Errorf("task %q: volumes: has an empty entry", t.ID)
		}
		if strings.ContainsAny(v, " \t\n\r") {
			return fmt.Errorf("task %q: volume %q contains whitespace — each entry must be a single host[:container[:options]] token", t.ID, v)
		}
		parts := strings.Split(v, ":")
		if len(parts) > 3 {
			return fmt.Errorf("task %q: volume %q has too many ':'-separated segments (want host[:container[:options]])", t.ID, v)
		}
		if parts[0] == "" {
			return fmt.Errorf("task %q: volume %q has an empty host path", t.ID, v)
		}
	}
	return nil
}

// validateTaskExecutor enforces the executor enum. v1 accepts
// "" / "local" (host fork — today's behavior) and "slurm"
// (sbatch job). Kubernetes / AWS Batch / GCP Batch stay
// roadmap and are still rejected with a pointer to it.
//
// Only valid on action: compute. Resource-shape rules live in
// validateTaskResources (called separately from the loop) so
// the enum check and the slurm-ask check stay independent.
func validateTaskExecutor(t *TaskDef) error {
	if t.Executor == "" {
		return nil
	}
	if t.Action != "compute" {
		return fmt.Errorf("task %q: executor: is only valid on action: compute tasks (got action: %s)", t.ID, t.Action)
	}
	switch t.Executor {
	case "local", "slurm":
		return nil
	default:
		return fmt.Errorf("task %q: executor %q is not yet supported (only \"local\" and \"slurm\" are available). "+
			"Kubernetes / AWS Batch / GCP Batch executors are planned post-launch — see WORKFLOW_GAPS.md § Executor abstraction.",
			t.ID, t.Executor)
	}
}

// validateTaskResources anchors resources: to its purpose:
// it only means anything for executor: slurm. A resources
// block on a local / inline / non-compute task is dead config
// — almost always an author mistake (left over from flipping
// executor, or typed in advance) — so surface it at parse
// time, mirroring the container_runtime-without-container and
// volumes-without-container guards.
//
// The field values themselves are NOT grammar-checked: SLURM
// arbitrates partition / time / mem syntax at submit time (same
// stance as container: — we don't re-implement the runtime's
// vocabulary). Only nonsensical numerics (negative cpus/gpus)
// are rejected here, since those can't be an intentional ask.
func validateTaskResources(t *TaskDef) error {
	if t.Resources.IsZero() {
		return nil
	}
	if t.Action != "compute" {
		return fmt.Errorf("task %q: resources: is only valid on action: compute tasks (got action: %s)", t.ID, t.Action)
	}
	if t.Executor != "slurm" {
		ex := t.Executor
		if ex == "" {
			ex = "local"
		}
		return fmt.Errorf("task %q: resources: is set but executor is %q — resource asks only apply to executor: slurm (a host fork takes whatever the host has)", t.ID, ex)
	}
	if t.Resources.CPUs < 0 {
		return fmt.Errorf("task %q: resources.cpus %d is negative", t.ID, t.Resources.CPUs)
	}
	if t.Resources.GPUs < 0 {
		return fmt.Errorf("task %q: resources.gpus %d is negative", t.ID, t.Resources.GPUs)
	}
	return nil
}

// ResolvedMode returns the mode a compute task should run in,
// applying the default. Non-compute tasks return "" since the
// concept doesn't apply; callers that branch on mode (the
// scheduler, the MCP execute handler) should scope the check
// to compute tasks first.
func ResolvedMode(t *TaskDef) string {
	return ResolvedModeFields(t.Action, t.Mode, t.Executor)
}

// ResolvedModeFields is the field-level variant of ResolvedMode.
// Useful for sites like the MCP execute handler that have an
// action + mode + executor triple from a task record (not a
// full TaskDef struct) — avoids constructing a synthetic
// TaskDef purely to call the defaulting logic.
//
// Effective-async rule (the single source of truth): a compute
// task is async iff mode == "async" OR it has a non-local
// executor. A queued/remote job (slurm, …) can never be
// synchronous — the execute_task call can't block on the SLURM
// queue — so the executor forces async regardless of mode:.
func ResolvedModeFields(action, mode, executor string) string {
	if action != "compute" {
		return ""
	}
	if mode == "async" || (executor != "" && executor != "local") {
		return "async"
	}
	if mode == "" {
		return "sync"
	}
	return mode
}

// validateTaskEnv enforces the compute-only + reserved-prefix
// rules for a task's env: block. The shape is dead simple on
// purpose: keys become env var names, values become env var
// values, and the three compute-context namespaces (system
// ENJU_*, run params ENJU_PARAM_*, task env) are kept disjoint
// by rejecting anything starting with ENJU_ here.
func validateTaskEnv(t *TaskDef) error {
	if len(t.Env) == 0 {
		return nil
	}
	if t.Action != "compute" {
		return fmt.Errorf("task %q: env: is only valid on action: compute tasks", t.ID)
	}
	for k := range t.Env {
		if k == "" {
			return fmt.Errorf("task %q: env: has an empty key", t.ID)
		}
		if strings.HasPrefix(k, "ENJU_") {
			return fmt.Errorf("task %q: env key %q: the ENJU_ prefix is reserved for system and run-param env vars — pick a different name", t.ID, k)
		}
		if !isValidEnvName(k) {
			return fmt.Errorf("task %q: env key %q: must match [A-Za-z_][A-Za-z0-9_]* (standard env var name rules)", t.ID, k)
		}
	}
	return nil
}

// isValidEnvName matches the POSIX-ish rule for environment
// variable names: starts with a letter or underscore, followed
// by letters, digits, or underscores. Anything else is rejected
// so authors don't ship env: blocks that the shell can't
// express.
func isValidEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		isAlpha := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
		isDigit := r >= '0' && r <= '9'
		if i == 0 {
			if !isAlpha && r != '_' {
				return false
			}
			continue
		}
		if !isAlpha && !isDigit && r != '_' {
			return false
		}
	}
	return true
}

// validateNoDuplicateReviewTargets refuses YAML where two
// distinct review tasks both target the same task. Living-
// workflow phase 6b.2 / multi-reviewer-per-task gate.
//
// The topic-branch + auto-merge model can't coherently merge
// two review topics that fork from the same upstream: the
// first approve advances main to review_a's tip, then the
// second approve attempts to FF main → review_b's tip — which
// isn't a descendant (review_b shares only the upstream's
// commit with review_a, not review_a's verdict commit). The
// FF refuses, surfacing the failure mode the reviewer can't
// recover from.
//
// v1 stance is "refuse at parse time with a clear path
// forward." Two recovery shapes for the YAML author:
//
//   - Use citizens: N on a single review task for quorum-
//     style multi-citizen review (handled by the run-level
//     multi-citizen gate).
//   - Split the reviewers into sequential review stages
//     (review_a depends_on draft, review_b depends_on
//     review_a). Each stage merges in turn.
//
// v2 will lift the restriction once rebase-on-non-FF and
// parallel-merge handling land; until then, parse-time
// rejection beats silent breakage on the second approve.
func validateNoDuplicateReviewTargets(p *Run) error {
	// targetID → first reviewer task that claimed it. Compared
	// pre-instance-expansion (against the bare `reviews:` field
	// from the YAML) since for_each-scoped reviews of the same
	// per-instance target are fine — each instance is a
	// different upstream task.
	firstReviewer := make(map[string]string)
	for _, t := range p.Tasks {
		if t.Action != "review" || t.Reviews == "" {
			continue
		}
		if prior, dup := firstReviewer[t.Reviews]; dup {
			return fmt.Errorf(
				"task %q reviews %q but task %q already does; "+
					"multi-reviewer-per-task is not supported in v1 "+
					"(would produce non-FF merge on the second approve). "+
					"Use citizens: N on a single review task for quorum-style "+
					"multi-citizen review, or split the reviewers into "+
					"sequential review stages (e.g. add depends_on so they "+
					"approve in turn).",
				t.ID, t.Reviews, prior)
		}
		firstReviewer[t.Reviews] = t.ID
	}
	return nil
}

// validateNoParallelSiblingWrites flags task pairs with no
// transitive dep edge between them whose declared
// writes_artifacts paths overlap on a literal-path match.
// Parallel-merge phase 4.
//
// Why this matters: under parallel execution two siblings'
// commits both target the same run branch. If two siblings
// declare the SAME literal output path, their commits will
// conflict at auto-merge time, which spawns a merge_resolve
// task and pulls a human into the loop. Catching the overlap
// at parse time gives the YAML author a clear rewrite path
// (add a dep edge or change one path) before any work runs.
//
// Glob ("*.md"), directory ("out/"), and template ("{{x}}")
// patterns are skipped here — literal-path overlap catches
// the common case; pattern-aware lint is a follow-up.
//
// Runs through after dep injection (review gating, vote
// activation, reviews-target auto-append) so the reachability
// walk sees the final dep graph. The walk also adds IMPLICIT
// dep edges for artifact reads/writes — `{{artifact:p}}` in a
// prompt or an explicit `reads_artifacts: [p]` makes the
// reading task transitively depend on writers of `p`. Without
// that, a producer→consumer pair (bootstrap writes p, refactor
// reads p, both write p again) would false-positive even though
// wireArtifactDeps will serialize them at materialization time.
//
// Known gap: `for_each` is processed at materialization time,
// not parse time, so a single TaskDef inside a `for_each` loop
// counts as ONE entry here. If that task declares a literal
// `writes_artifacts` path — same string for every iteration —
// the lint can't detect the N-way overlap that surfaces at
// runtime. Templated paths (`out/{{instance}}.md`) avoid the
// hazard naturally; literal paths inside for_each that
// actually overlap across iterations would slip through. Worth
// re-running on the materialized instance list in
// `engine.ValidateRunCreation` if this becomes a real
// authoring footgun.
func validateNoParallelSiblingWrites(p *Run) error {
	direct := make(map[string][]string, len(p.Tasks))
	for _, t := range p.Tasks {
		direct[t.ID] = append([]string(nil), t.DependsOn...)
	}
	// Augment with implicit artifact-read → writer edges so
	// the reachability walk matches what wireArtifactDeps will
	// inject post-materialization. Build a path → writers map
	// from declared writes_artifacts (literal paths only).
	writersByPath := make(map[string][]string)
	for _, t := range p.Tasks {
		for _, w := range t.WritesArtifacts {
			if w.Path == "" || strings.Contains(w.Path, "{{") {
				continue
			}
			writersByPath[w.Path] = append(writersByPath[w.Path], t.ID)
		}
	}
	for _, t := range p.Tasks {
		// Collect literal paths the task reads, from explicit
		// reads_artifacts and from `{{artifact:<path>}}` refs in
		// prompt / user_prompt / script.
		readPaths := map[string]struct{}{}
		for _, r := range t.ReadsArtifacts {
			if r == "" || strings.Contains(r, "{{") {
				continue
			}
			readPaths[r] = struct{}{}
		}
		for _, p := range artifactRefPaths(t.Prompt) {
			readPaths[p] = struct{}{}
		}
		for _, p := range artifactRefPaths(t.UserPrompt) {
			readPaths[p] = struct{}{}
		}
		for _, p := range artifactRefPaths(t.Script) {
			readPaths[p] = struct{}{}
		}
		for path := range readPaths {
			for _, writer := range writersByPath[path] {
				if writer == t.ID {
					continue
				}
				// Implicit edge: this task depends on `writer`.
				if !stringSliceContains(direct[t.ID], writer) {
					direct[t.ID] = append(direct[t.ID], writer)
				}
			}
		}
	}
	// Cache per-task transitive deps so each pair-check is a
	// constant-time map lookup. O(N * E) precompute, where E is
	// the total dep-edge count, then O(N²) pair scan with O(1)
	// lookups inside.
	transitive := make(map[string]map[string]struct{}, len(p.Tasks))
	for _, t := range p.Tasks {
		seen := make(map[string]struct{})
		stack := append([]string{}, direct[t.ID]...)
		for len(stack) > 0 {
			n := len(stack) - 1
			cur := stack[n]
			stack = stack[:n]
			if _, ok := seen[cur]; ok {
				continue
			}
			seen[cur] = struct{}{}
			stack = append(stack, direct[cur]...)
		}
		transitive[t.ID] = seen
	}
	for i := 0; i < len(p.Tasks); i++ {
		a := &p.Tasks[i]
		if len(a.WritesArtifacts) == 0 {
			continue
		}
		aLits := literalWritePaths(a.WritesArtifacts)
		if len(aLits) == 0 {
			continue
		}
		for j := i + 1; j < len(p.Tasks); j++ {
			b := &p.Tasks[j]
			if len(b.WritesArtifacts) == 0 {
				continue
			}
			// Connected via the dep DAG in either direction?
			// Then they aren't parallel — git's serialization
			// + linear FF handle the merge.
			if _, ok := transitive[a.ID][b.ID]; ok {
				continue
			}
			if _, ok := transitive[b.ID][a.ID]; ok {
				continue
			}
			bLits := literalWritePaths(b.WritesArtifacts)
			for path := range aLits {
				if _, overlap := bLits[path]; overlap {
					return fmt.Errorf(
						"tasks %q and %q both write %q but have no dep edge between them — "+
							"under parallel execution their commits will conflict at auto-merge time. "+
							"Add a dep edge (e.g. one task `depends_on: [%s]`) or change one path so the writes are disjoint",
						a.ID, b.ID, path, a.ID)
				}
			}
		}
	}
	return nil
}

// validateArtifactDeclarations enforces the artifact-path safety
// floor + list-expansion shape at PARSE time, so `enju validate`
// and `enju go --dry-run` reject the exact YAML the create path
// (engine.ValidateRunCreation) rejects. Before this existed the
// pre-flight was materially weaker than create despite the docs
// promising they "flatten identically": validate blessed `[*]` on
// a scalar param, double-`[*]`, `../outside`, `/etc/passwd`, and
// reserved-dir writes that create then refused — a false green.
//
// Two classes of check:
//
//  1. List-expansion shape (`{{p[*]}}`). Decidable from declared
//     param TYPES alone (no values needed): >1 ref per element is
//     rejected; a ref to an undeclared param is rejected; a ref to
//     a non-list<…> param is rejected. The wording matches the
//     substitution-time messages so the verdict is identical
//     whichever path the author hit.
//
//  2. Path safety floor (relative, no `..`, no reserved prefix).
//     Applied to the literal skeleton — every `{{…}}` segment is
//     replaced with an inert token first, so a still-templated
//     path like `state/{{x[*]}}.json` is checked on `state/_.json`
//     while `/etc/passwd` / `../x` / `enju/x` (no templates) are
//     caught verbatim. Param values can only ever make a path
//     MORE specific within a segment; they cannot retroactively
//     remove a literal `..` or a literal reserved prefix the
//     author wrote, so the parse-time verdict is a safe lower
//     bound on the create-time one.
func validateArtifactDeclarations(p *Run) error {
	declared := make(map[string]*ParamDef, len(p.Params))
	for i := range p.Params {
		declared[p.Params[i].Name] = &p.Params[i]
	}
	for i := range p.Tasks {
		t := &p.Tasks[i]
		for _, w := range t.WritesArtifacts {
			if err := checkStarRefShape(w.Path, declared); err != nil {
				return fmt.Errorf("task %q.writes: %v", t.ID, err)
			}
			if err := artifactpath.ValidateDeclaration(literalSkeleton(w.Path)); err != nil {
				return fmt.Errorf("task %q: invalid writes_artifacts path %q: %v", t.ID, w.Path, err)
			}
		}
		for _, r := range t.ReadsArtifacts {
			if err := checkStarRefShape(r, declared); err != nil {
				return fmt.Errorf("task %q.reads: %v", t.ID, err)
			}
			if err := artifactpath.ValidateLiteral(literalSkeleton(r)); err != nil {
				return fmt.Errorf("task %q: invalid reads_artifacts path %q: %v", t.ID, r, err)
			}
		}
	}
	return nil
}

// checkStarRefShape statically validates the `{{p[*]}}` list-
// expansion refs in a single declaration element using only the
// declared param TYPES — the same rules expandOneStarElement /
// starExpansionValues enforce at substitution time, but decided
// before values exist. Returns nil for elements with no `[*]`.
func checkStarRefShape(item string, declared map[string]*ParamDef) error {
	matches := starRefPattern.FindAllStringSubmatch(item, -1)
	if len(matches) == 0 {
		return nil
	}
	if len(matches) > 1 {
		return fmt.Errorf("element %q contains multiple [*] refs; only one is supported per element", item)
	}
	full := matches[0][0]
	name := matches[0][1]
	field := matches[0][2] // "" for the bare form
	pd, ok := declared[name]
	if !ok {
		return fmt.Errorf("%s references unknown parameter %q — [*] list-expansion requires a declared list<string> or list<record> parameter", full, name)
	}
	switch pd.Type {
	case "list<string>":
		if field != "" {
			return fmt.Errorf("{{%s[*].%s}} uses .field, which requires a list<record> parameter; %q is list<string>", name, field, name)
		}
	case "list<record>":
		// Field defaulting + unknown-field rejection happen at
		// substitution time against the actual records; the only
		// statically-decidable error is an explicitly-named field
		// that isn't a declared fields: entry.
		if field != "" {
			if _, df := pd.Fields.TypeOf(field); !df {
				return fmt.Errorf("{{%s[*].%s}} references unknown field %q on list<record> %q; declared fields: %s",
					name, field, field, name, strings.Join(pd.Fields.Names(), ", "))
			}
		}
	default:
		return fmt.Errorf("{{%s[*]}} requires a list<string> parameter; got %s", name, pd.Type)
	}
	return nil
}

// starSkeletonPattern matches any `{{…}}` template segment so the
// path-safety floor runs against the literal skeleton. Anchored on
// the same brace syntax the substituter uses; the inner body is
// intentionally permissive (covers {{p}}, {{p[*]}}, {{p[*].f}},
// {{t.field}}, {{artifact:x}}).
var starSkeletonPattern = regexp.MustCompile(`\{\{[^}]*\}\}`)

// literalSkeleton replaces every `{{…}}` segment with an inert
// token so the path-safety floor (`..`, leading `/`, reserved
// prefix) is checked on the author's literal text without a
// templated segment masking or fabricating a violation. The
// token is a single safe path char — it can't be empty (which
// could collapse `a/{{x}}/b` into `a//b`) and can't introduce a
// metacharacter or a `..`.
func literalSkeleton(pathExpr string) string {
	return starSkeletonPattern.ReplaceAllString(pathExpr, "_")
}

// artifactRefPaths extracts every literal path referenced by
// `{{artifact:<path>}}` in `text`. Used by the parallel-sibling
// lint to recognize implicit reads that wireArtifactDeps will
// turn into dep edges at materialization. Templated payloads
// (`{{artifact:{{x}}/foo}}`) are skipped — only fully literal
// paths are matched, mirroring the literal-path scope of the
// overlap check.
func artifactRefPaths(text string) []string {
	if text == "" {
		return nil
	}
	const tag = "{{artifact:"
	var out []string
	for {
		i := strings.Index(text, tag)
		if i < 0 {
			break
		}
		rest := text[i+len(tag):]
		end := strings.Index(rest, "}}")
		if end < 0 {
			break
		}
		path := rest[:end]
		if path != "" && !strings.Contains(path, "{{") {
			out = append(out, path)
		}
		text = rest[end+2:]
	}
	return out
}

// stringSliceContains is a tiny membership helper used by the
// parallel-sibling lint when augmenting the dep graph with
// implicit artifact-read edges. Local-named to avoid collision
// with the package-test `contains(string, string)` helper.
func stringSliceContains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// literalWritePaths returns the subset of declared writes whose
// `path` field is a fully literal string (no template var, no
// glob). Used by the parallel-sibling overlap lint to skip
// patterns it can't safely compare.
func literalWritePaths(w WriteArtifacts) map[string]struct{} {
	out := make(map[string]struct{}, len(w))
	for _, e := range w {
		if e.Path == "" {
			continue
		}
		if strings.Contains(e.Path, "{{") {
			continue
		}
		if strings.ContainsAny(e.Path, "*?[") {
			continue
		}
		// Directory-form ("out/") is a containment scope, not
		// a single file — skip for now. The "literal path"
		// lint targets file-level collisions only.
		if strings.HasSuffix(e.Path, "/") {
			continue
		}
		out[e.Path] = struct{}{}
	}
	return out
}

// validateReviewTarget enforces the "reviews:" field contract:
// required on action:review tasks, must differ from the
// reviewing task itself, forbidden on non-review tasks.
func validateReviewTarget(t *TaskDef) error {
	if t.Action == "review" {
		if t.Reviews == "" {
			return fmt.Errorf("task %q: reviews: <target_task_id> is required on review-action tasks", t.ID)
		}
		if t.Reviews == t.ID {
			return fmt.Errorf("task %q: reviews cannot reference the review task itself", t.ID)
		}
	} else if t.Reviews != "" {
		return fmt.Errorf("task %q: reviews is only valid on action: review tasks", t.ID)
	}
	return nil
}

// validateActionFields enforces the action-specific field
// matrix: vote options / threshold / deadline / quorum,
// review threshold / deadline / quorum / visibility /
// anonymize, and rejection of those fields on non-vote /
// non-review tasks.
func validateActionFields(t *TaskDef) error {
	if t.Action == "vote" {
		if len(t.Options) < 2 {
			return fmt.Errorf("task %q: action: vote requires at least 2 options", t.ID)
		}
		seenOptIDs := make(map[string]bool, len(t.Options))
		for i, opt := range t.Options {
			if opt.ID == "" {
				return fmt.Errorf("task %q: option #%d is missing an id", t.ID, i+1)
			}
			if seenOptIDs[opt.ID] {
				return fmt.Errorf("task %q: duplicate option id %q", t.ID, opt.ID)
			}
			seenOptIDs[opt.ID] = true
		}
		if t.Threshold != "" {
			if err := validateThreshold(t.Threshold); err != nil {
				return fmt.Errorf("task %q: %w", t.ID, err)
			}
		}
		if t.Deadline != "" {
			if _, err := time.ParseDuration(t.Deadline); err != nil {
				return fmt.Errorf("task %q: invalid deadline %q (expected a Go duration string such as %q or %q; bare numbers and day units are not accepted — use %q, not %q): %w", t.ID, t.Deadline, "2h", "30m", "24h", "1d", err)
			}
		}
		citizens := t.Citizens
		if citizens == 0 {
			citizens = 1
		}
		if t.MinQuorum > 0 && t.MinQuorum > citizens {
			return fmt.Errorf("task %q: min_quorum %d exceeds citizens %d", t.ID, t.MinQuorum, citizens)
		}
		if t.Visibility != "" && t.Visibility != "open" && t.Visibility != "blind" {
			return fmt.Errorf("task %q: invalid visibility %q (must be 'open' or 'blind')", t.ID, t.Visibility)
		}
		return nil
	}

	// Non-vote action.
	if len(t.Options) > 0 {
		return fmt.Errorf("task %q: options is only valid on action: vote tasks", t.ID)
	}
	if t.Threshold != "" && t.Action != "review" {
		return fmt.Errorf("task %q: threshold is only valid on action: vote or action: review tasks", t.ID)
	}
	if t.Deadline != "" && t.Action != "review" {
		return fmt.Errorf("task %q: deadline is only valid on action: vote or action: review tasks", t.ID)
	}
	if t.MinQuorum > 0 && t.Action != "review" {
		return fmt.Errorf("task %q: min_quorum is only valid on action: vote or action: review tasks", t.ID)
	}

	if t.Action == "review" {
		rc := t.Citizens
		if rc == 0 {
			rc = 1
		}
		if t.MinQuorum > 0 && t.MinQuorum > rc {
			return fmt.Errorf("task %q: min_quorum %d exceeds citizens %d", t.ID, t.MinQuorum, rc)
		}
		if t.Deadline != "" {
			if _, err := time.ParseDuration(t.Deadline); err != nil {
				return fmt.Errorf("task %q: invalid deadline %q: %w", t.ID, t.Deadline, err)
			}
		}
		if t.Threshold != "" {
			if err := validateReviewThreshold(t.Threshold); err != nil {
				return fmt.Errorf("task %q: %w", t.ID, err)
			}
		}
		if t.Visibility != "" && t.Visibility != "open" && t.Visibility != "blind" {
			return fmt.Errorf("task %q: invalid visibility %q (must be 'open' or 'blind')", t.ID, t.Visibility)
		}
		return nil
	}

	// Neither vote nor review — anonymize/visibility are
	// multi-citizen-only concepts and don't belong here.
	if t.Anonymize {
		return fmt.Errorf("task %q: anonymize is only valid on action: vote or action: review tasks", t.ID)
	}
	if t.Visibility != "" {
		return fmt.Errorf("task %q: visibility is only valid on action: vote or action: review tasks", t.ID)
	}
	return nil
}

// validateForEachScopes enforces the two run-wide invariants
// on for_each use:
//
//  1. Run-level and task-level for_each are mutually
//     exclusive. Authors pick one or the other per run.
//  2. If multiple tasks declare task-level for_each, they
//     must agree on the same variable space. A single run
//     supports one iteration dimension at a time.
func validateForEachScopes(p *Run, hasTaskLevelForEach bool) error {
	if len(p.ForEach) > 0 && hasTaskLevelForEach {
		return fmt.Errorf("run declares a run-level for_each AND task-level for_each on at least one task — these are mutually exclusive; move the for_each block to either the run level or the individual tasks but not both")
	}
	if !hasTaskLevelForEach {
		return nil
	}
	var firstID string
	var firstFE ForEachMap
	for i := range p.Tasks {
		t := &p.Tasks[i]
		if len(t.ForEach) == 0 {
			continue
		}
		if firstFE == nil {
			firstID = t.ID
			firstFE = t.ForEach
			continue
		}
		if !forEachEqual(firstFE, t.ForEach) {
			return fmt.Errorf("task %q declares a for_each that differs from task %q — all tasks in a run that use task-level for_each must declare the same variables and sources", t.ID, firstID)
		}
	}
	return nil
}

// validateDependsOnReferences checks every task's explicit
// depends_on list + its reviews target against the known
// task-ID set. Also auto-appends the reviews-target to
// depends_on so a review task always runs after whatever it
// reviews (mutation — authors don't have to declare the same
// relationship twice).
func validateDependsOnReferences(p *Run, ids map[string]bool) error {
	for i := range p.Tasks {
		t := &p.Tasks[i]
		for _, dep := range t.DependsOn {
			if !ids[dep] {
				return fmt.Errorf("task %q depends on %q which does not exist", t.ID, dep)
			}
		}
		if t.Reviews != "" {
			if !ids[t.Reviews] {
				return fmt.Errorf("task %q: reviews target %q does not exist", t.ID, t.Reviews)
			}
			hasDep := false
			for _, dep := range t.DependsOn {
				if dep == t.Reviews {
					hasDep = true
					break
				}
			}
			if !hasDep {
				t.DependsOn = append(t.DependsOn, t.Reviews)
			}
		}
		if t.Aggregates != "" {
			if t.Aggregates == t.ID {
				return fmt.Errorf("task %q: collects cannot reference itself", t.ID)
			}
			if !ids[t.Aggregates] {
				return fmt.Errorf("task %q: collects target %q does not exist", t.ID, t.Aggregates)
			}
			// Target must be a fanned task: either it declares
			// its own task-level for_each, or the run carries a
			// run-level for_each (which multiplies every task,
			// making the named target a fan-out source).
			var target *TaskDef
			for j := range p.Tasks {
				if p.Tasks[j].ID == t.Aggregates {
					target = &p.Tasks[j]
					break
				}
			}
			targetIsFanned := target != nil && (len(target.ForEach) > 0 || len(p.ForEach) > 0)
			if !targetIsFanned {
				return fmt.Errorf("task %q: collects target %q is not fanned (the target needs its own for_each, or the run must declare a run-level for_each that multiplies it)", t.ID, t.Aggregates)
			}
			// An aggregator with its own for_each is a category
			// error — the whole point of aggregates is to STAY
			// singular while reducing over the fanned source.
			if len(t.ForEach) > 0 {
				return fmt.Errorf("task %q: collects and for_each are mutually exclusive — an aggregator stays singular by definition", t.ID)
			}
			// Auto-add as a dependency so authors don't have
			// to write the same relationship twice (mirrors
			// the reviews auto-dep above). The expander uses
			// the fan-in topology rule on this edge: parent
			// expanded, child singleton → N edges to one child.
			hasDep := false
			for _, dep := range t.DependsOn {
				if dep == t.Aggregates {
					hasDep = true
					break
				}
			}
			if !hasDep {
				t.DependsOn = append(t.DependsOn, t.Aggregates)
			}
		}
		if t.Action == "vote" && len(t.Options) > 0 {
			for optIdx, opt := range t.Options {
				for _, target := range opt.Activates {
					if !ids[target] {
						return fmt.Errorf("task %q: option %q (#%d) activates unknown task %q", t.ID, opt.ID, optIdx+1, target)
					}
					if target == t.ID {
						return fmt.Errorf("task %q: option %q cannot activate the vote task itself", t.ID, opt.ID)
					}
				}
			}
		}
	}
	return nil
}

// injectReviewGating is the "if you added a review, everything
// downstream waits for the verdict" pass. For every review task
// R with reviews: T, inject an implicit dep from every task
// that consumes T (via explicit depends_on OR {{T.content}} /
// {{T.field}} references) to R. Without this pass, draft →
// {check, publish} runs in parallel and publish can ship an
// unreviewed draft before the review finishes.
//
// Returns non-fatal warnings for review tasks whose targets
// have zero downstream consumers (review runs but gates
// nothing — probably an authoring mistake).
//
// Mutation: appends to consumer.DependsOn.
func injectReviewGating(p *Run) []string {
	var warnings []string
	for i := range p.Tasks {
		reviewTask := &p.Tasks[i]
		if reviewTask.Action != "review" || reviewTask.Reviews == "" {
			continue
		}
		target := reviewTask.Reviews
		// Resolve the target's writes_artifacts once per review
		// pass. A consumer that declares a matching
		// reads_artifacts entry IS a downstream consumer for
		// review-gating: build.go wireArtifactDeps wires the
		// edge at parse time, and sqlite.go updateReadyTasksOn
		// holds the reader PENDING until the writer is
		// ACCEPTED. Counting these here keeps the lint aligned
		// with runtime DAG semantics.
		//
		// Scope: this mirrors wireArtifactDeps, which today
		// pairs only on explicit reads_artifacts / writes_artifacts
		// declarations — not on {{artifact:path}} prompt refs
		// (those resolve content but don't currently wire DAG
		// edges). If wireArtifactDeps ever widens to include
		// prompt-ref reads, mirror the broadened check here so
		// the two stay in lockstep.
		var targetWrites []string
		for k := range p.Tasks {
			if p.Tasks[k].ID == target {
				targetWrites = p.Tasks[k].WritesArtifacts.Paths()
				break
			}
		}
		targetWriteSet := make(map[string]struct{}, len(targetWrites))
		for _, w := range targetWrites {
			if w != "" {
				targetWriteSet[w] = struct{}{}
			}
		}
		consumersCount := 0
		for j := range p.Tasks {
			if i == j {
				continue
			}
			consumer := &p.Tasks[j]
			// Skip other review tasks reviewing the same
			// target — each review is independent and
			// shouldn't wait on another reviewer's verdict.
			if consumer.Action == "review" && consumer.Reviews == target {
				continue
			}
			consumes := false
			for _, dep := range consumer.DependsOn {
				if dep == target {
					consumes = true
					break
				}
			}
			if !consumes {
				for _, inferred := range template.InferDependencies(consumer.Prompt) {
					if inferred == target {
						consumes = true
						break
					}
				}
			}
			if !consumes && len(targetWriteSet) > 0 {
				for _, r := range consumer.ReadsArtifacts {
					if _, ok := targetWriteSet[r]; ok {
						consumes = true
						break
					}
				}
			}
			if !consumes {
				continue
			}
			consumersCount++
			hasDep := false
			for _, dep := range consumer.DependsOn {
				if dep == reviewTask.ID {
					hasDep = true
					break
				}
			}
			if !hasDep {
				consumer.DependsOn = append(consumer.DependsOn, reviewTask.ID)
			}
		}
		if consumersCount == 0 {
			warnings = append(warnings, fmt.Sprintf(
				"task %q reviews %q but %q has no downstream consumers — the review runs but gates nothing (possibly an authoring mistake)",
				reviewTask.ID, target, target,
			))
		}
	}
	return warnings
}

// injectVoteActivation injects the reverse "activated →
// vote" depends_on edges so vote-routed branches can't start
// running until the vote resolves. A separate pass because
// activated tasks might appear after the vote task in source
// order — doing this inline in the initial task walk would
// miss forward references.
//
// Mutation: appends to activated.DependsOn.
func injectVoteActivation(p *Run) {
	for i := range p.Tasks {
		voteTask := &p.Tasks[i]
		if voteTask.Action != "vote" {
			continue
		}
		for _, opt := range voteTask.Options {
			for _, target := range opt.Activates {
				for j := range p.Tasks {
					activated := &p.Tasks[j]
					if activated.ID != target {
						continue
					}
					hasDep := false
					for _, dep := range activated.DependsOn {
						if dep == voteTask.ID {
							hasDep = true
							break
						}
					}
					if !hasDep {
						activated.DependsOn = append(activated.DependsOn, voteTask.ID)
					}
					break
				}
			}
		}
	}
}
