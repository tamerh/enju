package service

import "github.com/enju-ai/enju/internal/coordinator/store"

// CitizenUsername resolves an internal citizen ID to its
// user-facing handle. Returns "" when the ID is 0 or the
// citizen row is missing — both are non-error cases (model
// attribution and "added by" lookups are best-effort metadata,
// not load-bearing). Replaces the per-package
// lookupCitizenUsername / s.citizenUsername helpers.
func CitizenUsername(s store.CoordinatorStore, id int64) string {
	if id == 0 {
		return ""
	}
	c, _ := s.GetCitizen(id)
	if c == nil {
		return ""
	}
	return c.Username
}
