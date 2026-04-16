package engine

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/store"
)

// ReviewTallyOutcome describes the result of evaluating a
// multi-reviewer review task's submissions against the
// task's dissent-policy threshold.
type ReviewTallyOutcome struct {
	Resolved     bool
	Verdict      string // "approve" or "reject" when Resolved
	Approves     int
	Rejects      int
	TotalReviews int
	Reason       string // why not resolved yet
}

// VoteTallyOutcome describes the result of evaluating the
// current set of submitted votes against a multi-citizen
// vote task's threshold + quorum + deadline rules.
type VoteTallyOutcome struct {
	Resolved      bool
	WinningOption string
	Counts        map[string]int
	TotalVotes    int
	Reason        string
}

// EvaluateReviewTally walks the per-citizen review
// submissions and applies the task's dissent-policy
// threshold. Pure computation — reads submissions from the
// store, returns the outcome. Never writes state.
//
// Supported policies (set via threshold: in YAML):
//
//   - "any-reject-kills" (default): first reject resolves
//     as reject immediately. Matches real-world code review:
//     one "you can't ship this" kills the submission. All
//     approves with quorum met resolves as approve.
//   - "unanimous-approve": all reviewers must approve. Any
//     reject kills it. Equivalent to any-reject-kills for
//     the reject path but requires the full set to weigh in.
//   - "majority-approve": strictly more than half of
//     submitted reviews must be approve. Short-circuits as
//     soon as the outcome is mathematically decided — e.g.
//     with citizens: 3, 2 approves resolve immediately
//     without waiting for the third ballot.
//   - "percent:N": at least N% of reviewers (of min_quorum
//     or citizens count) must approve. Short-circuits when
//     mathematically impossible to reach.
//
// Quorum (min_quorum or default citizens) gates when the
// approve-path can resolve. The reject-path short-circuits
// regardless of quorum under any-reject-kills.
func (e *Engine) EvaluateReviewTally(task *store.TaskRecord) (*ReviewTallyOutcome, error) {
	submissions, err := e.store.ListVoteSubmissions(task.ID)
	if err != nil {
		return nil, fmt.Errorf("listing review submissions: %w", err)
	}
	out := &ReviewTallyOutcome{TotalReviews: len(submissions)}
	for _, sub := range submissions {
		switch sub.Option {
		case "approve":
			out.Approves++
		case "reject":
			out.Rejects++
		}
	}

	policy := strings.ToLower(task.VoteThreshold)
	if policy == "" {
		policy = "any-reject-kills"
	}

	needed := task.MinQuorum
	if needed <= 0 {
		needed = task.Citizens
		if needed <= 0 {
			needed = 1
		}
	}

	switch {
	case policy == "any-reject-kills":
		if out.Rejects > 0 {
			out.Resolved = true
			out.Verdict = "reject"
			return out, nil
		}
		if out.Approves < needed {
			out.Reason = fmt.Sprintf("approvals not yet at quorum (%d of %d)", out.Approves, needed)
			return out, nil
		}
		out.Resolved = true
		out.Verdict = "approve"
	case policy == "unanimous-approve":
		if out.Rejects > 0 {
			out.Resolved = true
			out.Verdict = "reject"
			return out, nil
		}
		if out.Approves < needed {
			out.Reason = fmt.Sprintf("unanimous approval not yet at quorum (%d of %d)", out.Approves, needed)
			return out, nil
		}
		out.Resolved = true
		out.Verdict = "approve"
	case policy == "majority-approve":
		if out.Approves*2 > needed {
			out.Resolved = true
			out.Verdict = "approve"
			return out, nil
		}
		if out.Rejects*2 >= needed {
			out.Resolved = true
			out.Verdict = "reject"
			return out, nil
		}
		pending := needed - out.TotalReviews
		if pending < 0 {
			pending = 0
		}
		out.Reason = fmt.Sprintf("majority not yet decidable (%d approve, %d reject, waiting for %d more)",
			out.Approves, out.Rejects, pending)
	case strings.HasPrefix(policy, "percent:"):
		pctStr := strings.TrimPrefix(policy, "percent:")
		pct, err := strconv.Atoi(pctStr)
		if err != nil || pct < 1 || pct > 100 {
			out.Reason = fmt.Sprintf("invalid percent threshold %q", task.VoteThreshold)
			return out, nil
		}
		requiredApproves := (pct*needed + 99) / 100
		if out.Approves >= requiredApproves {
			out.Resolved = true
			out.Verdict = "approve"
			return out, nil
		}
		pending := needed - out.TotalReviews
		if pending < 0 {
			pending = 0
		}
		if out.Approves+pending < requiredApproves {
			out.Resolved = true
			out.Verdict = "reject"
			return out, nil
		}
		out.Reason = fmt.Sprintf("percent:%d not yet met (%d of %d needed)",
			pct, out.Approves, requiredApproves)
	default:
		out.Reason = fmt.Sprintf("unknown review threshold %q", task.VoteThreshold)
	}
	return out, nil
}

// EvaluateVoteTally applies the task's threshold + quorum
// rules to the current set of submitted votes. Pure
// computation — reads submissions and deadline state from
// the store, returns the outcome. Never writes state.
//
// Threshold rules: plurality (default), majority,
// unanimous, percent:N. Ties under plurality/majority are
// broken by declaration order in the YAML (first-declared
// option wins a tie).
//
// Quorum defaults to citizens count for multi-voter tasks
// (wait for everyone). Deadline override: once past, quorum
// drops to 1 — tally whatever landed. Under-threshold
// tallies stay stuck for human intervention.
func (e *Engine) EvaluateVoteTally(task *store.TaskRecord) (*VoteTallyOutcome, error) {
	submissions, err := e.store.ListVoteSubmissions(task.ID)
	if err != nil {
		return nil, fmt.Errorf("listing submissions: %w", err)
	}
	counts := make(map[string]int, len(submissions))
	for _, sub := range submissions {
		if sub.Option == "" {
			continue
		}
		counts[sub.Option]++
	}
	total := len(submissions)

	var declared []struct {
		ID        string   `json:"id"`
		Label     string   `json:"label,omitempty"`
		Activates []string `json:"activates,omitempty"`
	}
	if task.VoteOptions != "" {
		_ = json.Unmarshal([]byte(task.VoteOptions), &declared)
	}

	deadlinePassed, err := e.DeadlinePassed(task)
	if err != nil {
		return nil, err
	}

	minQuorum := task.MinQuorum
	if minQuorum <= 0 {
		if task.Citizens > 1 {
			minQuorum = task.Citizens
		} else {
			minQuorum = 1
		}
	}
	if deadlinePassed {
		minQuorum = 1
	}
	if total < minQuorum {
		reason := fmt.Sprintf("quorum not met (%d of %d)", total, minQuorum)
		if deadlinePassed {
			reason = "deadline passed with zero submissions"
		}
		return &VoteTallyOutcome{
			Counts:     counts,
			TotalVotes: total,
			Reason:     reason,
		}, nil
	}

	threshold := task.VoteThreshold
	if threshold == "" {
		threshold = "plurality"
	}
	winner, reason := PickWinner(declared, counts, total, threshold)
	if winner == "" {
		if deadlinePassed {
			reason = "deadline passed, " + reason
		}
		return &VoteTallyOutcome{
			Counts:     counts,
			TotalVotes: total,
			Reason:     reason,
		}, nil
	}
	return &VoteTallyOutcome{
		Resolved:      true,
		WinningOption: winner,
		Counts:        counts,
		TotalVotes:    total,
	}, nil
}

// DeadlinePassed reports whether the task's voting deadline
// has elapsed. The clock starts when the first citizen
// claims (not when the task was created or the run was
// submitted) — a task can sit in READY waiting for deps,
// and the deadline should only tick when voting opens.
// Tasks with no deadline always return false.
func (e *Engine) DeadlinePassed(task *store.TaskRecord) (bool, error) {
	if task.VoteDeadline == "" {
		return false, nil
	}
	d, err := time.ParseDuration(task.VoteDeadline)
	if err != nil {
		e.logger.Warn("invalid vote deadline", "task_id", task.ID, "deadline", task.VoteDeadline, "error", err)
		return false, nil
	}
	firstClaim, err := e.store.EarliestClaimTime(task.ID)
	if err != nil {
		return false, fmt.Errorf("earliest claim lookup: %w", err)
	}
	if firstClaim.IsZero() {
		return false, nil
	}
	return time.Now().After(firstClaim.Add(d)), nil
}

// PickWinner returns the winning option id given the current
// counts and the threshold rule. Returns empty string + a
// reason when no winner can be declared. Ties under
// plurality/majority are broken by declaration order.
// Exported so both the engine and callers can use it.
func PickWinner(declared []struct {
	ID        string   `json:"id"`
	Label     string   `json:"label,omitempty"`
	Activates []string `json:"activates,omitempty"`
}, counts map[string]int, total int, threshold string) (string, string) {
	if total == 0 {
		return "", "no votes cast"
	}

	maxCount := 0
	for _, c := range counts {
		if c > maxCount {
			maxCount = c
		}
	}
	if maxCount == 0 {
		return "", "no votes cast"
	}

	var leaders []string
	for _, opt := range declared {
		if counts[opt.ID] == maxCount {
			leaders = append(leaders, opt.ID)
		}
	}
	if len(leaders) == 0 {
		return "", "no declared option matched the top count"
	}
	winner := leaders[0]

	lower := strings.ToLower(threshold)
	switch {
	case lower == "plurality":
		return winner, ""
	case lower == "majority":
		if maxCount*2 > total {
			return winner, ""
		}
		return "", fmt.Sprintf("majority not met (%d of %d)", maxCount, total)
	case lower == "unanimous":
		if maxCount == total && len(leaders) == 1 {
			return winner, ""
		}
		return "", fmt.Sprintf("unanimous not met (%d of %d agree)", maxCount, total)
	case strings.HasPrefix(lower, "percent:"):
		pctStr := strings.TrimPrefix(lower, "percent:")
		pct, err := strconv.Atoi(pctStr)
		if err != nil || pct < 1 || pct > 100 {
			return "", fmt.Sprintf("invalid percent threshold %q", threshold)
		}
		if maxCount*100 >= pct*total {
			return winner, ""
		}
		return "", fmt.Sprintf("percent:%d not met (%d of %d = %d%%)", pct, maxCount, total, (maxCount*100)/total)
	}
	return "", fmt.Sprintf("unknown threshold %q", threshold)
}
