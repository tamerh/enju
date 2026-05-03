package store

// noopEventStore is the fallback EventStore returned by
// Store.Events() when no real store has been attached. Every
// write is silently dropped; every read returns
// ErrEventStoreDisabled. Used by:
//
//  - Unit tests that don't care about events.
//  - Offline fixtures and CLI tools that don't open events.db.
//  - The transition path during the migration of
//   existing emit sites — call sites pointing at Store.Events()
//   keep working before AttachEventStore wiring lands.
//
// The "disabled" framing is deliberate: code that branches on
// store.Events().Enabled() naturally treats the no-op store
// the same as an operator-killed store, which is the right
// behavior — both states mean "no audit happening here."

import (
	"context"
	"time"
)

type noopEventStore struct{}

func (noopEventStore) Record(Event) {}

func (noopEventStore) QueryByRun(context.Context, int64, int64, time.Time, int) ([]Event, error) {
	return nil, ErrEventStoreDisabled
}

// noopWaitCh is closed at init — the noop store has nothing to
// notify about, so long-poll waiters return immediately and fall
// through to their next query (which will also be empty).
var noopWaitCh = func() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

func (noopEventStore) WaitForEvent() <-chan struct{} { return noopWaitCh }

func (noopEventStore) QueryByCitizen(context.Context, int64, int) ([]Event, error) {
	return nil, ErrEventStoreDisabled
}

func (noopEventStore) Query(context.Context, EventQuery) ([]Event, error) {
	return nil, ErrEventStoreDisabled
}

func (noopEventStore) CountByCitizenAndType(context.Context, int64) (map[string]map[string]int, error) {
	return nil, ErrEventStoreDisabled
}

func (noopEventStore) SumTokensForCitizen(context.Context, int64) (int64, error) {
	return 0, ErrEventStoreDisabled
}

func (noopEventStore) CountDistinctProjectsForCitizen(context.Context, int64) (int, error) {
	return 0, ErrEventStoreDisabled
}

func (noopEventStore) CountContributionEvents(context.Context, int64) (int, error) {
	return 0, ErrEventStoreDisabled
}

func (noopEventStore) CountProjectsThisMonth(context.Context, int64, time.Time) (int, error) {
	return 0, ErrEventStoreDisabled
}

func (noopEventStore) LatestMetadataForTask(context.Context, string, string) (string, error) {
	return "", ErrEventStoreDisabled
}

func (noopEventStore) DistinctTaskIDsForCitizenAndType(context.Context, int64, string) ([]string, error) {
	return nil, ErrEventStoreDisabled
}

func (noopEventStore) Stats() Stats { return Stats{Enabled: false} }

func (noopEventStore) Enabled() bool { return false }

func (noopEventStore) SetEnabled(bool) {}

func (noopEventStore) WaitForDrain(time.Duration) {}

func (noopEventStore) GapsInProject(context.Context, int64) ([]int64, error) {
	return nil, ErrEventStoreDisabled
}

func (noopEventStore) Close() error { return nil }
