package gitv6

// Stub implementations of every Ops method that hasn't been
// ported from the v5 sibling yet. Each returns ErrNotImplemented
// (or zero values + that error). They exist so *Clone satisfies
// the Ops interface from the start of the migration — withlock,
// cross-package wiring, and parity tests all need a real
// satisfying type.
//
// As each phase ports a real method into its own dedicated file
// (read.go, branch.go, etc.), DELETE the stub here. When this
// file is empty, every Ops method has been ported and Phase G
// can flip the default backend.

import (
	"time"
)

// --- Metadata getters (real impls — pure field access) ---

func (c *Clone) RemoteURL() string {
	return c.remoteURL
}

func (c *Clone) LastPushAt() time.Time {
	return c.lastPushAt
}

func (c *Clone) LastPushError() string {
	return c.lastPushError
}
