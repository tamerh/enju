package gitcli

import (
	"testing"
)

// Subprocess-overhead microbench. The doc claim is "negligible
// at enju's call volume (dozens/min, not thousands/sec)" —
// these numbers back that claim.
//
// Run with: go test -bench=. -run=^$ ./internal/fatclient/enjugit/internal/gitcli/
//
// Measured (i7-1185G7, Linux, git 2.43, 2026-05-12):
//
//   BenchmarkResolveRefHEAD      ~780 µs/op   (rev-parse --verify)
//   BenchmarkHead                ~1.4 ms/op   (rev-parse + symbolic-ref)
//   BenchmarkLocalBranchHash     ~700 µs/op   (single rev-parse)
//   BenchmarkState               ~1.7 ms/op   (symbolic-ref + status)
//
// Enju's hot path runs ~5-20 of these per minute under active
// load. 20 ops/min × 1.5 ms/op = 30 ms/minute = 0.05% CPU —
// invisible. Confirms the design choice of fresh-subprocess-
// per-call over long-running git plumbing reuse.

func BenchmarkResolveRefHEAD(b *testing.B) {
	dir := b.TempDir()
	// Use the gitInit helper from scaffolding_test.go (same pkg).
	cmd := []string{"init", "-b", "main", dir}
	if _, err := runGit("", cmd, runOpts{}); err != nil {
		b.Fatal(err)
	}
	if _, err := InitLocal(dir, "", nullLogger()); err != nil {
		b.Fatal(err)
	}
	c, err := OpenClone(dir, "", nullLogger())
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.ResolveRef("main"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHead(b *testing.B) {
	dir := b.TempDir()
	if _, err := runGit("", []string{"init", "-b", "main", dir}, runOpts{}); err != nil {
		b.Fatal(err)
	}
	if _, err := InitLocal(dir, "", nullLogger()); err != nil {
		b.Fatal(err)
	}
	c, _ := OpenClone(dir, "", nullLogger())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := c.Head(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLocalBranchHash(b *testing.B) {
	dir := b.TempDir()
	if _, err := runGit("", []string{"init", "-b", "main", dir}, runOpts{}); err != nil {
		b.Fatal(err)
	}
	if _, err := InitLocal(dir, "", nullLogger()); err != nil {
		b.Fatal(err)
	}
	c, _ := OpenClone(dir, "", nullLogger())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.LocalBranchHash("main"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkState(b *testing.B) {
	dir := b.TempDir()
	if _, err := runGit("", []string{"init", "-b", "main", dir}, runOpts{}); err != nil {
		b.Fatal(err)
	}
	if _, err := InitLocal(dir, "", nullLogger()); err != nil {
		b.Fatal(err)
	}
	c, _ := OpenClone(dir, "", nullLogger())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.State()
	}
}
