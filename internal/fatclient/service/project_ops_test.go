package service

// Tests for the smart-detect dispatch in enju_create_project.
// validateAndInspectPath classifies the on-disk state of the
// caller's path; EagerInitProjectClone takes the resulting
// AdoptionTarget and dispatches to one of five branches:
//
//   Case 1: nonexistent path        → mkdir + git init + seed + managed bare
//   Case 2: existing empty folder   → git init + seed + managed bare
//   Case 3: populated, no .git      → git init + commit existing files + managed bare
//   Case 4: .git, no origin         → managed bare wired in (history untouched)
//   Case 5: .git + origin           → register only, leave the remote alone
//
// Plus the safety gate (Case 4-unrelated): an existing .git with
// commits and no Enju marker is refused unless force=true.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corelayout "github.com/enju-ai/enju/internal/common/layout"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// initBareRepoAt seeds a fresh bare git repo at dir for use as a
// pretend remote. Returns dir for convenience.
func initBareRepoAt(t *testing.T, dir string) string {
	t.Helper()
	if _, err := gogit.PlainInit(dir, true); err != nil {
		t.Fatalf("init bare at %s: %v", dir, err)
	}
	return dir
}

// initRepoWithCommit lays down a minimal git repo at dir with one
// commit. Used to construct Case 4 / Case 5 fixtures.
func initRepoWithCommit(t *testing.T, dir string, withEnjuMarker bool) *gogit.Repository {
	t.Helper()
	repo, err := gogit.PlainInitWithOptions(dir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
		},
	})
	if err != nil {
		t.Fatalf("plain init: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# pre-existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if withEnjuMarker {
		// enju/templates/.gitkeep — the marker DetectPopulatedUnrelatedRepo
		// looks for. Differentiates "previously adopted by Enju, safe to
		// re-adopt" from "totally unrelated user repo, refuse without force".
		tmplDir := filepath.Join(dir, corelayout.DefaultTemplatesDir)
		if err := os.MkdirAll(tmplDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmplDir, ".gitkeep"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("add glob: %v", err)
	}
	sig := &object.Signature{Name: "u", Email: "u@u", When: time.Now()}
	if _, err := wt.Commit("seed", &gogit.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return repo
}

// TestCreateProject_SmartDetectDispatch pins the 5-case + 2 force-
// gate matrix in `validateAndInspectPath` + `EagerInitProjectClone`
// against directly observable post-state on disk. One row per
// branch of the dispatch table — failures point at exactly which
// case regressed.
func TestCreateProject_SmartDetectDispatch(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cases := []struct {
		name string

		// setup populates the temp dir into the desired pre-state
		// and returns the path EagerInitProjectClone should be
		// called with. Returning a different path is how Case 1
		// expresses "the path doesn't exist": setup removes it
		// after t.TempDir() created it.
		setup func(t *testing.T, baseDir string) string

		force bool

		// Validation expectations.
		wantValidationErr string // substring; "" = no error

		// Post-state expectations (only checked when validation
		// + EagerInitProjectClone succeed).
		wantBareWired       bool   // expect <path>/enju/.bare.git/HEAD
		wantOriginEquals    string // expect origin == this URL; "" = bare path; "preserve" = pre-existing URL
		wantHEADResolves    bool   // expect refs/heads/main has a SHA
		wantUserFilePreserved string // relative path that must exist post-init; "" = skip check
	}{
		{
			name: "Case 1: nonexistent path",
			setup: func(t *testing.T, baseDir string) string {
				p := filepath.Join(baseDir, "doesnt-exist", "deep", "child")
				// Don't mkdir — validateAndInspectPath does it.
				return p
			},
			wantBareWired:    true,
			wantHEADResolves: true,
		},
		{
			name: "Case 2: existing empty folder",
			setup: func(t *testing.T, baseDir string) string {
				p := filepath.Join(baseDir, "empty")
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
				return p
			},
			wantBareWired:    true,
			wantHEADResolves: true,
		},
		{
			name: "Case 3: populated, no .git",
			setup: func(t *testing.T, baseDir string) string {
				p := filepath.Join(baseDir, "populated")
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(p, "user-data.txt"), []byte("important\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return p
			},
			wantBareWired:         true,
			wantHEADResolves:      true,
			wantUserFilePreserved: "user-data.txt",
		},
		{
			name: "Case 4: .git, no origin",
			setup: func(t *testing.T, baseDir string) string {
				p := filepath.Join(baseDir, "git-no-origin")
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
				initRepoWithCommit(t, p, true /* enju marker so safety gate doesn't fire */)
				return p
			},
			wantBareWired:         true,
			wantHEADResolves:      true,
			wantUserFilePreserved: "README.md",
		},
		{
			name: "Case 5: .git + origin (preserved)",
			setup: func(t *testing.T, baseDir string) string {
				p := filepath.Join(baseDir, "git-with-origin")
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
				repo := initRepoWithCommit(t, p, true)
				// Real-looking origin URL — ensureManagedBare's
				// existing-origin guard skips bare creation when
				// any URL is set, so we don't need a reachable
				// remote.
				if _, err := repo.CreateRemote(&config.RemoteConfig{
					Name: "origin",
					URLs: []string{"git@github.com:enju-ai/test-fixture.git"},
				}); err != nil {
					t.Fatalf("create remote: %v", err)
				}
				return p
			},
			wantBareWired:         false,
			wantOriginEquals:      "git@github.com:enju-ai/test-fixture.git",
			wantHEADResolves:      true,
			wantUserFilePreserved: "README.md",
		},
		{
			name: "Safety gate: populated unrelated repo refused without force",
			setup: func(t *testing.T, baseDir string) string {
				p := filepath.Join(baseDir, "unrelated")
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
				initRepoWithCommit(t, p, false /* no enju marker */)
				return p
			},
			force:             false,
			wantValidationErr: "force=true",
		},
		{
			name: "Safety gate: force=true bypasses, adopts repo",
			setup: func(t *testing.T, baseDir string) string {
				p := filepath.Join(baseDir, "unrelated-forced")
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
				initRepoWithCommit(t, p, false)
				return p
			},
			force:                 true,
			wantBareWired:         true,
			wantHEADResolves:      true,
			wantUserFilePreserved: "README.md",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseDir := t.TempDir()
			path := tc.setup(t, baseDir)

			// Validate + inspect.
			target, err := validateAndInspectPath(path, tc.force)
			if tc.wantValidationErr != "" {
				if err == nil {
					t.Fatalf("expected validation error containing %q, got nil", tc.wantValidationErr)
				}
				if !strings.Contains(err.Error(), tc.wantValidationErr) {
					t.Fatalf("validation error %q does not contain %q", err.Error(), tc.wantValidationErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}

			// Materialize.
			wsRoot := filepath.Join(baseDir, ".enju-workspaces")
			ws, err := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(), enjugit.WithLogger(logger))
			if err != nil {
				t.Fatalf("workspace: %v", err)
			}
			fc := New(Config{
				WorkspaceRoot:   ws.RootDir(),
				Logger:          logger,
				ProjectRegistry: projectreg.Open(filepath.Join(baseDir, "registry.json")),
			})
			projectID := int64(42)
			if err := fc.EagerInitProjectClone(context.Background(), projectID, path, target); err != nil {
				t.Fatalf("EagerInitProjectClone: %v", err)
			}

			// Bare presence check.
			barePath := filepath.Join(path, corelayout.BotPushTargetDir)
			_, statErr := os.Stat(filepath.Join(barePath, "HEAD"))
			bareExists := statErr == nil
			if tc.wantBareWired && !bareExists {
				t.Errorf("expected managed bare at %q, got stat error: %v", barePath, statErr)
			}
			if !tc.wantBareWired && bareExists {
				t.Errorf("expected NO managed bare at %q (case 5 should preserve existing origin), but bare exists", barePath)
			}

			// Origin URL check.
			repo, openErr := gogit.PlainOpen(path)
			if openErr != nil {
				t.Fatalf("PlainOpen %q: %v", path, openErr)
			}
			rem, remErr := repo.Remote("origin")
			if remErr != nil {
				t.Fatalf("repo has no origin after dispatch: %v", remErr)
			}
			gotOrigin := rem.Config().URLs[0]
			switch {
			case tc.wantOriginEquals != "":
				if gotOrigin != tc.wantOriginEquals {
					t.Errorf("origin: got %q, want %q (preserved)", gotOrigin, tc.wantOriginEquals)
				}
			case tc.wantBareWired:
				// Origin should point at the managed bare.
				if gotOrigin != barePath {
					t.Errorf("origin: got %q, want %q (managed bare)", gotOrigin, barePath)
				}
			}

			// HEAD resolves.
			if tc.wantHEADResolves {
				if _, herr := repo.Head(); herr != nil {
					t.Errorf("HEAD does not resolve: %v", herr)
				}
			}

			// User-file survival.
			if tc.wantUserFilePreserved != "" {
				if _, ferr := os.Stat(filepath.Join(path, tc.wantUserFilePreserved)); ferr != nil {
					t.Errorf("user file %q lost: %v", tc.wantUserFilePreserved, ferr)
				}
			}
		})
	}
}
