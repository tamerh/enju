// Package scheduler handles background task management.
package scheduler

import (
	"log/slog"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// ClaimReaper owns the per-expired-claim decision. The coordinator
// implements it: for a single-claimant citizen task whose lease
// expired with no delivery it charges the durable layer-① counter
// and, at the cap, parks the task failed_retryable — independent of
// any fat-client report (the spec's D3 backstop). For every other
// case it performs the plain CLAIMED→READY expiry. A nil escalator
// preserves the original behavior (raw ExpireClaim) so tests and
// any unwired path keep working.
type ClaimReaper interface {
	ReapExpiredClaim(taskID string, citizenID int64) error
}

// Reaper checks for expired task claims and resets them to READY.
type Reaper struct {
	store     store.CoordinatorStore
	interval  time.Duration
	logger    *slog.Logger
	stop      chan struct{}
	escalator ClaimReaper
}

// NewReaper creates a new task reaper.
func NewReaper(st store.CoordinatorStore, interval time.Duration, logger *slog.Logger) *Reaper {
	return &Reaper{
		store:    st,
		interval: interval,
		logger:   logger,
		stop:     make(chan struct{}),
	}
}

// SetEscalator wires the coordinator-owned per-claim handler. Call
// before Start. With it set, the reaper delegates EVERY expired
// claim to the coordinator (which decides count / escalate / plain
// expire); without it the reaper does the raw ExpireClaim.
func (r *Reaper) SetEscalator(e ClaimReaper) { r.escalator = e }

// Start begins the reaper loop in a goroutine.
func (r *Reaper) Start() {
	go r.loop()
	r.logger.Info("task reaper started", "interval", r.interval)
}

// Stop signals the reaper to stop.
func (r *Reaper) Stop() {
	close(r.stop)
}

func (r *Reaper) loop() {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			r.sweep()
		case <-r.stop:
			r.logger.Info("task reaper stopped")
			return
		}
	}
}

func (r *Reaper) sweep() {
	expired, err := r.store.GetExpiredClaims()
	if err != nil {
		r.logger.Error("reaper: getting expired claims", "error", err)
		return
	}

	for _, claim := range expired {
		// With the coordinator escalator wired, EVERY expired claim
		// goes through it: it owns the citizen layer-① verify-fail
		// gate (count / escalate to failed_retryable) and falls
		// back to the plain CLAIMED→READY expiry for every other
		// case. This is the coordinator-as-enforcement-boundary
		// backstop — it fires on the lease cadence regardless of
		// whether the fat-client ever reported.
		if r.escalator != nil {
			if err := r.escalator.ReapExpiredClaim(claim.TaskID, claim.CitizenID); err != nil {
				r.logger.Error("reaper: handling expired claim", "task_id", claim.TaskID, "error", err)
				continue
			}
			r.logger.Info("reaper: expired claim handled",
				"task_id", claim.TaskID, "citizen", claim.CitizenID, "deadline", claim.Deadline)
			continue
		}

		// No escalator wired — original behavior. Expired task goes
		// CLAIMED → READY. No cascade needed: the reaped task IS the
		// one becoming ready; nothing downstream unblocks because no
		// upstream resolved.
		if _, err := r.store.ApplyPlan(store.Plan{
			Version: engine.EngineVersion,
			Mutations: []store.Mutation{
				store.ExpireClaim{TaskID: claim.TaskID, CitizenID: claim.CitizenID},
			},
		}); err != nil {
			r.logger.Error("reaper: expiring task", "task_id", claim.TaskID, "error", err)
			continue
		}
		r.logger.Info("reaper: task expired",
			"task_id", claim.TaskID,
			"citizen", claim.CitizenID,
			"deadline", claim.Deadline,
		)
	}
}
