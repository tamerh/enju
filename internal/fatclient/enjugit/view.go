package enjugit

import (
	"log/slog"

	"github.com/enju-ai/enju/internal/fatclient/enjugit/internal/git"
)

// View is the read-only handle for one project. Constructed by
// Workspace.OpenView or OpenOrLazyClone. Used by surfaces that
// only display content (webui, inbox).
//
// The type itself is the capability boundary — code that takes
// *View can't call mutating methods, full stop. That's the
// point: webui code reading "this function takes *View" knows
// at a glance it can't push, can't commit, can't switch branches.
//
// Concrete read methods live in view_methods.go (added in
// Phase 9).
type View struct {
	git    git.View // the read-only interface, not Ops
	convs  Conventions
	projID int64
	logger *slog.Logger
}

// ProjectID returns the project ID this View operates on.
func (v *View) ProjectID() int64 { return v.projID }
