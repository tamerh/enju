package types

// CitizenKind discriminates the two citizen sub-types:
//
//   - Human: a person. Default for rows where Kind="" — the
//     discriminator was added later, so legacy/unset rows are
//     normalized to "human" at read time.
//   - Bot: an unattended citizen owned by a human (ParentID
//     set), with its own token, that claims and executes tasks;
//     its handler may be an LLM or a script. It MUST attribute
//     its work to a model — the apply layer rejects agent
//     submissions with an empty model.
//
// A model is NOT a citizen kind. A model has no identity, no
// row, no lifecycle — it is a normalized name stamped as a
// label on the work (task_claims.model).
type CitizenKind string

const (
	CitizenKindHuman CitizenKind = "human"
	// CitizenKindBot is the unattended-citizen kind. The Go
	// identifier keeps the historical "Bot" name (internal-only;
	// renaming it would be pure churn) while the wire VALUE is
	// "agent". The name≠value asymmetry is deliberate, not a bug
	// — every consumer compares against this constant, never the
	// literal, so this is the single source of the string.
	CitizenKindBot CitizenKind = "agent"
)

// IsValidCitizenKind reports whether s is one of the declared
// kinds. Empty string is rejected — callers that want to allow
// the legacy-default case explicitly normalize "" →
// CitizenKindHuman before calling.
func IsValidCitizenKind(s string) bool {
	switch CitizenKind(s) {
	case CitizenKindHuman, CitizenKindBot:
		return true
	}
	return false
}
