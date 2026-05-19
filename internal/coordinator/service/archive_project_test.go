package service

// Acceptance tests for reversible project archive/restore. Real
// *store.Store + attached SQLite event store so the
// "event rides the same write" and idempotency (no duplicate
// event) contracts are observable end-to-end, not mocked.

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

func newArchiveStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	es, err := store.NewSQLiteEventStore(filepath.Join(dir, "e.db"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewSQLiteEventStore: %v", err)
	}
	st.AttachEventStore(es)
	t.Cleanup(func() { st.Close(); es.Close() })
	return st
}

func ap_apply(t *testing.T, st *store.Store, m store.Mutation) store.ApplyResult {
	t.Helper()
	res, err := st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{m}})
	if err != nil {
		t.Fatalf("ApplyPlan(%T): %v", m, err)
	}
	return res
}

func ap_project(t *testing.T, st *store.Store, name string) int64 {
	now := time.Now()
	return ap_apply(t, st, store.CreateProject{Project: store.ProjectRecord{
		Name: name, CreatedAt: now, UpdatedAt: now,
	}}).ProjectID
}

func ap_citizen(t *testing.T, st *store.Store, uname string) *store.CitizenRecord {
	now := time.Now()
	id := ap_apply(t, st, store.CreateCitizen{
		Citizen: store.CitizenRecord{
			Username: uname, Name: uname, Email: uname + "@t.local",
			Kind: store.CitizenKindHuman, RegisteredAt: now, LastSeen: now,
		},
		Token: "tok-" + uname,
	}).CitizenID
	c, err := st.GetCitizen(id)
	if err != nil || c == nil {
		t.Fatalf("GetCitizen: %v", err)
	}
	return c
}

// ap_projectOwned creates a project AND records `owner` as an
// owner-member. Membership matters twice: ListProjects is
// membership-gated (ListProjectsForCitizen), and requireOwner
// only treats a project as open when it has ZERO members — once
// members exist the owner role is actually enforced.
func ap_projectOwned(t *testing.T, st *store.Store, name string, owner *store.CitizenRecord) int64 {
	pid := ap_project(t, st, name)
	ap_apply(t, st, store.AddProjectMember{
		ProjectID: pid, CitizenID: owner.ID,
		Role: store.ProjectRoleOwner, AddedBy: owner.ID,
	})
	return pid
}

func ap_run(t *testing.T, st *store.Store, projectID int64, state store.RunState) {
	now := time.Now()
	ap_apply(t, st, store.CreateRun{Run: store.RunRecord{
		ProjectID: projectID, Name: "r", YAMLData: "name: r",
		State: state, CreatedAt: now, UpdatedAt: now,
	}})
}

// eventCount returns the exact number of project-scoped events of
// the given type AFTER the async event store has fully drained
// (mirrors store-test waitForEventsDrained: Persisted+Dropped >=
// Enqueued). Draining first is what makes an exact-count
// assertion — incl. "exactly 0" and "no duplicate" — reliable
// rather than racy.
func eventCount(t *testing.T, st *store.Store, projectID int64, evtType string) int {
	t.Helper()
	if es, ok := st.Events().(*store.SQLiteEventStore); ok {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			s := es.Stats()
			if s.Persisted+s.Dropped >= s.Enqueued {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	evs, err := st.ListEvents(store.EventQuery{
		ProjectID: projectID, EventTypes: []string{evtType}, Limit: 100,
	})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	return len(evs)
}

func inList(t *testing.T, st *store.Store, caller *store.CitizenRecord, includeArchived bool, projectID int64) bool {
	t.Helper()
	ps, err := ListProjects(st, caller, includeArchived)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	for _, p := range ps {
		if p.ID == projectID {
			return true
		}
	}
	return false
}

// Archive a terminal-only project → hidden by
// default, visible+marked with include_archived, event once;
// restore → reappears, event once, archive_at/by kept as
// provenance; runs stay queryable throughout (history not locked).
func TestArchiveProject_TerminalRuns_HiddenRestorableEventsOnce(t *testing.T) {
	st := newArchiveStore(t)
	owner := ap_citizen(t, st, "owner")
	pid := ap_projectOwned(t, st, "smoke", owner)
	ap_run(t, st, pid, store.RunCompleted)
	ap_run(t, st, pid, store.RunFailed)

	resp, err := ArchiveProject(st, owner, pid)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if resp.Status != "archived" || !resp.Archived {
		t.Fatalf("resp = %+v, want archived", resp)
	}
	p, _ := st.GetProject(pid)
	if !p.Archived || p.ArchivedBy == "" || p.ArchivedAt.IsZero() {
		t.Fatalf("project row not stamped: %+v", p)
	}
	if inList(t, st, owner, false, pid) {
		t.Error("archived project must be hidden from the default list")
	}
	if !inList(t, st, owner, true, pid) {
		t.Error("archived project must show with include_archived=true")
	}
	if n := eventCount(t, st, pid, "project_archived"); n != 1 {
		t.Errorf("project_archived emitted %d times, want exactly 1", n)
	}
	// Runs still queryable — archive hides the project
	// from the index, it does not lock its history.
	if runs, _ := st.ListRunsByProject(pid); len(runs) != 2 {
		t.Errorf("archived project's runs must stay queryable; got %d, want 2", len(runs))
	}

	rresp, err := RestoreProject(st, owner, pid)
	if err != nil || rresp.Status != "restored" {
		t.Fatalf("restore: resp=%+v err=%v", rresp, err)
	}
	p2, _ := st.GetProject(pid)
	if p2.Archived {
		t.Error("restored project must not be archived")
	}
	// ArchivedAt/By kept as last-archive provenance on restore.
	if p2.ArchivedBy == "" || p2.ArchivedAt.IsZero() {
		t.Error("restore must KEEP archived_at/by as last-archive provenance")
	}
	if !inList(t, st, owner, false, pid) {
		t.Error("restored project must reappear in the default list")
	}
	if n := eventCount(t, st, pid, "project_restored"); n != 1 {
		t.Errorf("project_restored emitted %d times, want exactly 1", n)
	}
}

// Archive refused while a non-terminal run exists; no
// state change, no event, still listed (fail-closed precondition).
func TestArchiveProject_NonTerminalRun_Refused(t *testing.T) {
	st := newArchiveStore(t)
	owner := ap_citizen(t, st, "owner")
	pid := ap_projectOwned(t, st, "live", owner)
	ap_run(t, st, pid, store.RunActive) // non-terminal

	_, err := ArchiveProject(st, owner, pid)
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
	if !strings.Contains(err.Error(), "non-terminal run") {
		t.Errorf("message must name the precondition; got %q", err.Error())
	}
	if p, _ := st.GetProject(pid); p.Archived {
		t.Error("project must not be archived when refused")
	}
	if !inList(t, st, owner, false, pid) {
		t.Error("refused project must still be listed")
	}
	if n := eventCount(t, st, pid, "project_archived"); n != 0 {
		t.Errorf("no event on a refused archive; got %d", n)
	}
}

// Non-owner → ErrForbidden; unknown id → ErrNotFound
// (both archive and restore).
func TestArchiveProject_OwnerGateAndNotFound(t *testing.T) {
	st := newArchiveStore(t)
	owner := ap_citizen(t, st, "owner")
	stranger := ap_citizen(t, st, "stranger")
	pid := ap_project(t, st, "guarded")
	// Make it non-zero-member so the open-legacy bypass doesn't
	// apply: owner is an owner-member, stranger is a plain member.
	ap_apply(t, st, store.AddProjectMember{ProjectID: pid, CitizenID: owner.ID, Role: store.ProjectRoleOwner, AddedBy: owner.ID})
	ap_apply(t, st, store.AddProjectMember{ProjectID: pid, CitizenID: stranger.ID, Role: store.ProjectRoleMember, AddedBy: owner.ID})

	if _, err := ArchiveProject(st, stranger, pid); !errors.Is(err, ErrForbidden) {
		t.Errorf("non-owner archive: want ErrForbidden, got %v", err)
	}
	if _, err := RestoreProject(st, stranger, pid); !errors.Is(err, ErrForbidden) {
		t.Errorf("non-owner restore: want ErrForbidden, got %v", err)
	}
	if _, err := ArchiveProject(st, owner, 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id archive: want ErrNotFound, got %v", err)
	}
	if _, err := RestoreProject(st, owner, 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id restore: want ErrNotFound, got %v", err)
	}
	// The real owner can still archive (owner gate allows owners).
	if _, err := ArchiveProject(st, owner, pid); err != nil {
		t.Errorf("owner archive must succeed: %v", err)
	}
}

// Idempotent — repeat archive/restore is a no-op
// success and emits NO duplicate event. archive→restore→archive is
// clean.
func TestArchiveProject_Idempotent_NoDupEvents(t *testing.T) {
	st := newArchiveStore(t)
	owner := ap_citizen(t, st, "owner")
	pid := ap_projectOwned(t, st, "idem", owner)

	if r, err := ArchiveProject(st, owner, pid); err != nil || r.Status != "archived" {
		t.Fatalf("archive#1: r=%+v err=%v", r, err)
	}
	if r, err := ArchiveProject(st, owner, pid); err != nil || r.Status != "already_archived" {
		t.Fatalf("archive#2 must be already_archived no-op: r=%+v err=%v", r, err)
	}
	if r, err := RestoreProject(st, owner, pid); err != nil || r.Status != "restored" {
		t.Fatalf("restore#1: r=%+v err=%v", r, err)
	}
	if r, err := RestoreProject(st, owner, pid); err != nil || r.Status != "already_restored" {
		t.Fatalf("restore#2 must be already_restored no-op: r=%+v err=%v", r, err)
	}
	if r, err := ArchiveProject(st, owner, pid); err != nil || r.Status != "archived" {
		t.Fatalf("archive#3 (re-archive after restore): r=%+v err=%v", r, err)
	}
	// Two real archive transitions (no-ops issued no Plan) ⇒
	// exactly two project_archived events, never three.
	if n := eventCount(t, st, pid, "project_archived"); n != 2 {
		t.Errorf("project_archived count = %d, want 2 (no dup from the no-op)", n)
	}
	if n := eventCount(t, st, pid, "project_restored"); n != 1 {
		t.Errorf("project_restored count = %d, want 1 (no dup from the no-op)", n)
	}
}

// Archive is content-neutral — zero on-disk effect.
// Structural proof at this layer: ArchiveProject's only inputs are
// (store, caller, projectID); it never receives or derives a
// filesystem path, and the store here is a temp sqlite with no
// project working tree on disk at all. Archiving a project that has
// NO on-disk path still succeeds — proving the operation is purely
// a coordinator-row write. A path-touching implementation
// would have nothing to touch and could not pass.
func TestArchiveProject_NoOnDiskEffect(t *testing.T) {
	st := newArchiveStore(t)
	owner := ap_citizen(t, st, "owner")
	pid := ap_project(t, st, "nodisk") // never created on a filesystem
	if _, err := ArchiveProject(st, owner, pid); err != nil {
		t.Fatalf("archive must not depend on any on-disk project state: %v", err)
	}
}
