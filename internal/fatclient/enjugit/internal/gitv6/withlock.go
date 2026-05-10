package gitv6

// WithLock holds the project lock across the closure. Inside fn,
// the passed Ops is the same Clone; nested mutating method calls
// from the SAME goroutine see themselves as the lock holder and
// skip re-acquisition (the non-recursive sync.Mutex would
// deadlock otherwise).
//
// Used by enjugit when a workflow verb needs multiple git ops to
// land atomically (e.g. SubmitTaskResult: switch branch + write
// files + commit + push as one indivisible unit). Without this,
// another goroutine could see the half-applied state between ops.
//
// Returns whatever fn returns. The lock is released even when fn
// panics.
//
// Goroutine semantics: another goroutine's WithLock or per-op
// lock() blocks on mu until fn returns. An EARLIER design used a
// shared `reentrant bool` that allowed any goroutine to observe
// "we're inside WithLock" and skip locking — that collapsed
// serialization across goroutines and is the bug this comment
// outlives. Reentrancy is now keyed on the holder goroutine ID
// stored on the Clone, not on a process-global flag.
//
// Nested WithLock calls from the same goroutine are safe: the
// inner one sees holder==me, skips re-acquisition, and is a
// no-op around fn.
func (c *Clone) WithLock(fn func(Ops) error) error {
	me := goroutineID()
	if c.holder.Load() == me {
		// Already inside this goroutine's WithLock; just invoke.
		return fn(c)
	}
	c.mu.Lock()
	if c.fileLock != nil {
		_ = c.fileLock.Lock()
	}
	c.holder.Store(me)
	defer func() {
		c.holder.Store(0)
		if c.fileLock != nil {
			_ = c.fileLock.Unlock()
		}
		c.mu.Unlock()
	}()
	return fn(c)
}
