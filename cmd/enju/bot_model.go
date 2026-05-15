package main

import (
	"path/filepath"
	"strings"
)

// nonLLMModelName derives the provenance "model" name for a
// non-LLM (script) handler bot. The coord auto-registers this
// string and validates it under the SAME charset rule as a
// username (store.ValidateUsername): lowercase [a-z0-9-], no
// leading/trailing hyphen. A raw `script:lint-bot.sh` fails it
// (`:` and `.` are illegal) — that was the bug. So slugify the
// handler basename the same way store.SlugifyName does
// (lowercase; non-alphanumerics → hyphen; collapse repeats;
// trim hyphens) and prefix `script-`.
//
// Basename, not full path, by intent: provenance is attribution
// ("named producer"), not identity. Two handlers a/run.sh and
// b/run.sh both derive script-run-sh — collision tolerated; the
// coord only requires a bot name SOMETHING and a readable
// trailer value beats a path. Widen to a path slug only if audit
// ever needs to disambiguate which script ran.
//
// The "script-" prefix guarantees a valid result even for a
// pathological handler: the prefix is alphanumeric so the name
// never starts with a hyphen, and an empty slug falls back to
// "nonllm" so it never ends with one either.
func nonLLMModelName(handler string) string {
	base := filepath.Base(strings.TrimSpace(handler))

	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(base) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevHyphen = false
			continue
		}
		if !prevHyphen {
			b.WriteByte('-')
			prevHyphen = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		slug = "nonllm"
	}
	return "script-" + slug
}
