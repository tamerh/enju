package types

// CitizenKind discriminates the three citizen sub-types used
// by the operator/model attribution design (docs/operator-model.md).
//
//   - Human: a person submitting work directly. Default for
//     pre-Phase-1.1 rows where Kind="" — the human/bot/model
//     discriminator was added later, so legacy rows with
//     empty kind are normalized to "human" at read time.
//   - Bot: an automation parented by a human (ParentID set).
//     Bots MUST attribute submissions to a model; the apply
//     layer rejects bot submissions with empty Model.
//   - Model: a catalog entry representing an LLM (claude-opus-4-7,
//     gpt-4o, etc.). Model citizens have a synthetic
//     "model:<username>" token that does NOT authenticate —
//     the placeholder rule in store.upsertModelCitizen
//     security comment is load-bearing.
type CitizenKind string

const (
	CitizenKindHuman CitizenKind = "human"
	CitizenKindBot   CitizenKind = "bot"
	CitizenKindModel CitizenKind = "model"
)

// IsValidCitizenKind reports whether s is one of the three
// declared kinds. Empty string is rejected — callers that
// want to allow the legacy-default case explicitly normalize
// "" → CitizenKindHuman before calling.
func IsValidCitizenKind(s string) bool {
	switch CitizenKind(s) {
	case CitizenKindHuman, CitizenKindBot, CitizenKindModel:
		return true
	}
	return false
}
