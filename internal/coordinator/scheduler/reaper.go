// Package scheduler handles background task management.
package scheduler

import (
	"log/slog"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// Reaper checks for expired task claims and resets them to READY.
type Reaper struct {
	store    store.CoordinatorStore
	interval time.Duration
	logger   *slog.Logger
	stop     chan struct{}
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
		// Expired task goes CLAIMED → READY. No cascade needed:
		// the reaped task IS the one becoming ready; nothing
		// downstream unblocks because no upstream resolved.
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
