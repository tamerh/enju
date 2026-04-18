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
	"time"

	"github.com/enju-ai/enju/internal/template"
)

// validActions is the set of supported action values. Declared
// at package scope so it's not rebuilt every call.
var validActions = map[string]bool{
	"answer":     true,
	"contribute": true,
	"compute":    true,
	"review":     true,
	"vote":       true,
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
	ids, hasTaskLevelForEach, err := validateTasks(p)
	if err != nil {
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
	if err := validateTemplateReferences(p, ids); err != nil {
		return nil, err
	}
	if err := validateDynamicForEach(p, ids); err != nil {
		return nil, err
	}
	return append(paramWarnings, reviewWarnings...), nil
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
		switch {
		case src.Ref != "":
			if _, ok := parseForEachParamRef(src.Ref); ok {
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
			return nil, fmt.Errorf("param %q: invalid type %q (must be string, int, bool, or list<string>)", pp.Name, pp.Type)
		}
		if pp.Required && pp.Default != nil {
			return nil, fmt.Errorf("param %q: required and default are mutually exclusive", pp.Name)
		}
		if pp.Default != nil {
			if err := checkParamValueType(pp.Name, pp.Type, pp.Default); err != nil {
				return nil, err
			}
		}
		if pp.Description == "" {
			warnings = append(warnings, fmt.Sprintf("param %q has no description — the LLM needs prose to turn this into a question for the user", pp.Name))
		}
	}
	return warnings, nil
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

		if t.ResultType != "" && t.ResultType != "text" && t.ResultType != "json" && t.ResultType != "file" {
			return nil, false, fmt.Errorf("task %q: invalid result_type %q", t.ID, t.ResultType)
		}

		if len(t.ForEach) > 0 {
			hasTaskLevelForEach = true
			if err := validateForEachMap("task "+t.ID, t.ForEach); err != nil {
				return nil, false, err
			}
		}
	}
	return ids, hasTaskLevelForEach, nil
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
				return fmt.Errorf("task %q: invalid deadline %q (expected a Go duration like 2h, 30m, 1d is NOT supported — use 24h): %w", t.ID, t.Deadline, err)
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
//   1. Run-level and task-level for_each are mutually
//      exclusive. Authors pick one or the other per run.
//   2. If multiple tasks declare task-level for_each, they
//      must agree on the same variable space. A single run
//      supports one iteration dimension at a time.
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
