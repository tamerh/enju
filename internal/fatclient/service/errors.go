package service

import "errors"

// ErrNotFound is returned by read methods (GetRun, GetProject)
// when the coordinator answered HTTP 404. Callers errors.Is()
// it to map to a real 404 instead of a blanket 502.
//
// Why it's needed: coord.Get deliberately swallows the status
// code and hands back 4xx bodies with err==nil ("a successful
// read of an error-shaped payload" — see coord/client.go
// GetStatus doc). Without recovering the status, a missing run
// decodes into a zero-value struct and the failure only
// surfaces later as an opaque decode error. These methods use
// coord.GetStatus to recover the 404 and wrap it with this
// sentinel.
var ErrNotFound = errors.New("not found")
