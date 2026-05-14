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

	corelayout "github.com/enju-ai/enju/internal/common/layout"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
	"github.com/enju-ai/enju/internal/testutil/gittest"
)

// initBareRepoAt seeds a fresh bare git repo at dir for use as a
// pretend remote. Returns dir for convenience.
func initBareRepoAt(t *testing.T, dir string) string {
	t.Helper()
	gittest.InitBare(t, dir)
	return dir
}

// initRepoWithCommit lays down a minimal git repo at dir with one
// commit. Used to construct Case 4 / Case 5 fixtures.
func initRepoWithCommit(t *testing.T, dir string, withEnjuMarker bool) {
	t.Helper()
	gittest.Init(t, dir)
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
	gittest.CommitAll(t, dir, "seed")
}

// TestCreateProject_SmartDetectDispatch pins the 5-case + 2 force-
// gate matrix in `validateAndInspectPath` + `EagerInitProjectClone`
// against directly observable post-state on disk. One row per
// branch of the dispatch table — failures point at exactly which
// case regressed.
//
// Post-Phase-8 contract: no managed bare gets created at project
// init. Operator's working tree's `.git/` IS the single store;
// plumbing-submit writes objects + non-HEAD refs there directly.
// Origin stays unset for solo single-machine projects; user wires
// a real remote via enju_set_project_remote when they're ready
// to share.
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
		wantOriginEquals      string // expected origin URL; "" = no origin expected
		wantHEADResolves      bool   // expect refs/heads/main has a SHA
		wantUserFilePreserved string // relative path that must exist post-init; "" = skip check
	}{
		{
			name: "Case 1: nonexistent path",
			setup: func(t *testing.T, baseDir string) string {
				p := filepath.Join(baseDir, "doesnt-exist", "deep", "child")
				// Don't mkdir — validateAndInspectPath does it.
				return p
			},
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
				initRepoWithCommit(t, p, true)
				// Pre-existing origin URL stays intact across init.
				gittest.AddRemote(t, p, "origin", "git@github.com:enju-ai/test-fixture.git")
				return p
			},
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
			wantHEADResolves:      true,
			wantUserFilePreserved: "README.md",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseDir := t.TempDir()
			path := tc.setup(t, baseDir)

			// Validate + inspect.
			target, err := validateAndInspectPath(path, tc.force, nil)
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
			reg1 := projectreg.Open(filepath.Join(t.TempDir(), "projects.json"))
			ws, err := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(), enjugit.WithLogger(logger), enjugit.WithRegistry(reg1))
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

			// No managed bare gets created at init time.
			barePath := filepath.Join(path, "enju", ".bare.git")
			if _, statErr := os.Stat(filepath.Join(barePath, "HEAD")); statErr == nil {
				t.Errorf("unexpected managed bare at %q — Phase 8 stopped auto-creating bares at project init", barePath)
			}

			// Origin URL check. Empty wantOriginEquals = no origin
			// expected (solo single-machine project, no remote
			// configured yet).
			gotOrigin, originErr := gittest.RunOK(t, path, "remote", "get-url", "origin")
			if tc.wantOriginEquals == "" {
				if originErr == nil {
					t.Errorf("expected no origin, got %q", gotOrigin)
				}
			} else {
				if originErr != nil {
					t.Fatalf("expected origin %q preserved, got error: %v", tc.wantOriginEquals, originErr)
				}
				if gotOrigin != tc.wantOriginEquals {
					t.Errorf("origin: got %q, want %q (preserved)", gotOrigin, tc.wantOriginEquals)
				}
			}

			// HEAD resolves.
			if tc.wantHEADResolves {
				if _, herr := gittest.RunOK(t, path, "rev-parse", "--verify", "HEAD"); herr != nil {
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
