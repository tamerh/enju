package git

// WithLock holds the project lock across the closure. Inside fn,
// the passed Ops is the same Clone but with the reentrant flag
// set, so nested mutating method calls don't try to re-acquire
// the (non-recursive) lock.
//
// Used by enjugit when a workflow verb needs multiple git ops to
// land atomically (e.g. SubmitTaskResult: switch branch + write
// files + commit + push as one indivisible unit). Without this,
// another goroutine could see the half-applied state between ops.
//
// Returns whatever fn returns. The lock is released even when fn
// panics.
//
// Nested WithLock calls are safe: the inner one sees reentrant
// already true, skips re-acquisition, and is a no-op around fn.
func (c *Clone) WithLock(fn func(Ops) error) error {
	if c.reentrant {
		// Already inside a WithLock; just invoke.
		return fn(c)
	}
	c.mu.Lock()
	if c.fileLock != nil {
		_ = c.fileLock.Lock()
	}
	c.reentrant = true
	defer func() {
		c.reentrant = false
		if c.fileLock != nil {
			_ = c.fileLock.Unlock()
		}
		c.mu.Unlock()
	}()
	return fn(c)
}
