package types

// CitizenKind discriminates the two citizen sub-types:
//
//   - Human: a person. Default for rows where Kind="" — the
//     discriminator was added later, so legacy/unset rows are
//     normalized to "human" at read time.
//   - Agent: an unattended citizen owned by a human (ParentID
//     set), with its own token, that claims and executes tasks;
//     its handler may be an LLM or a script. Model attribution
//     is optional, not forced by kind — a script/lint agent
//     produces no LLM output and carries no model.
//
// A model is NOT a citizen kind. A model has no identity, no
// row, no lifecycle — it is a normalized name stamped as a
// label on the work (task_claims.model).
type CitizenKind string

const (
	CitizenKindHuman CitizenKind = "human"
	// CitizenKindAgent is the unattended-citizen kind. The wire
	// VALUE "agent" is the API/DB contract; every consumer
	// compares against this constant, never the literal, so this
	// is the single source of the string.
	CitizenKindAgent CitizenKind = "agent"
)

// IsValidCitizenKind reports whether s is one of the declared
// kinds. Empty string is rejected — callers that want to allow
// the legacy-default case explicitly normalize "" →
// CitizenKindHuman before calling.
func IsValidCitizenKind(s string) bool {
	switch CitizenKind(s) {
	case CitizenKindHuman, CitizenKindAgent:
		return true
	}
	return false
}
