package service

// Claim-lease heartbeat for long-running sync compute. The
// coordinator's lease/reaper pair recovers tasks whose worker
// died, but the lease length is a guess (the task's `timeout:`,
// or the 30-min default) and a legitimate compute script can run
// for hours. Without renewal the reaper treated "slow honest
// script" identically to "dead worker": claim expired mid-run,
// task re-readied, and the script's eventual result refused (the
// longtask-parallel-run-desync bug). While the script runs, this
// loop periodically re-anchors the lease so expiry again means
// "this fat-client process went silent", not "the script was slow".

import (
	"context"
	"sync"
	"time"
)

// claimHeartbeatInterval derives the renewal cadence from the
// task's declared timeout: a third of the lease, so a single
// missed ping (network blip, coordinator restart) still leaves
// two chances before the lease runs out. Mirrors the coordinator's
// taskClaimTimeout: declared `timeout:` when parseable, else the
// 30-min default lease → 10-min pings.
func claimHeartbeatInterval(timeout string) time.Duration {
	lease := 30 * time.Minute
	if timeout != "" {
		if d, err := time.ParseDuration(timeout); err == nil && d > 0 {
			lease = d
		}
	}
	return lease / 3
}

// startClaimHeartbeat launches a goroutine that POSTs
// /tasks/{id}/heartbeat every lease/3 until stop() is called or
// ctx is cancelled. Best-effort: a failed or refused ping is
// logged and the loop keeps going — if the claim is truly gone
// (reaped/released), the eventual /result report fails loudly and
// that's where the error surfaces. stop() is idempotent.
func (s *FatClient) startClaimHeartbeat(ctx context.Context, taskID, timeout string) (stop func()) {
	interval := claimHeartbeatInterval(timeout)
	done := make(chan struct{})
	var once sync.Once
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				data, err := s.coord.Post(ctx, "/api/v1/tasks/"+taskID+"/heartbeat", nil)
				if err != nil {
					s.logger.Warn("claim heartbeat failed; lease may expire under the running script",
						"task_id", taskID, "error", err)
					continue
				}
				if msg := extractErrorString(data); msg != "" {
					// Refused = the open claim is gone (reaped before
					// the first ping, released, or resolved). Keep the
					// script running (complete-then-stop policy — the
					// work still lands in git); the /result report
					// will surface the loud failure.
					s.logger.Warn("claim heartbeat refused; this claim is no longer held",
						"task_id", taskID, "error", msg)
				} else {
					s.logger.Debug("claim heartbeat renewed", "task_id", taskID)
				}
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}
