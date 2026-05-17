package service

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
	"github.com/google/uuid"
)

// ErrConflict is the sentinel for already-taken usernames /
// duplicate emails. Transports map to 409.
var ErrConflict = errors.New("conflict")

// ErrForbidden is the sentinel for ownership-violation
// rejections (e.g. revoking a token you don't own).
// Transports map to 403.
var ErrForbidden = errors.New("forbidden")

// UpdateProfileResponse is the wire shape for
// enju_update_profile.
type UpdateProfileResponse struct {
	Status string `json:"status"`
}

// UpdateProfile updates the caller's display name and/or email
// (whichever non-nil). nil pointers leave the field untouched.
// Empty Name string is rejected (matches register flow's "name
// is required"); empty Email is honored as a clear.
//
// Caller can only update their own profile — the username
// parameter must match caller.Username. The legacy api endpoint
// took a URL path username and didn't enforce this; service
// closes that gap.
func UpdateProfile(s store.CoordinatorStore, caller *store.CitizenRecord, username string, name, email *string) (*UpdateProfileResponse, error) {
	if username == "" {
		username = caller.Username
	}
	if username != caller.Username {
		return nil, fmt.Errorf("%w: cannot update another citizen's profile", ErrForbidden)
	}
	if name != nil && *name == "" {
		return nil, fmt.Errorf("%w: name cannot be empty", ErrInvalidArgument)
	}
	citizen, err := s.GetCitizenByUsername(username)
	if err != nil {
		return nil, err
	}
	if citizen == nil {
		return nil, ErrNotFound
	}
	if _, err := s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.UpdateCitizenProfile{CitizenID: citizen.ID, Name: name, Email: email},
		},
	}); err != nil {
		if strings.Contains(err.Error(), "email already exists") {
			return nil, fmt.Errorf("%w: %s", ErrConflict, err.Error())
		}
		return nil, err
	}
	return &UpdateProfileResponse{Status: "updated"}, nil
}

// RegisterBotParams bundles the new-bot fields. Empty Username
// triggers slug-from-name auto-generation; empty Role defaults
// to "citizen". An optional Label retags the auto-issued token.
type RegisterBotParams struct {
	Name     string
	Username string
	Role     string
	Label    string
}

// RegisterBotResponse is the wire shape — includes the token,
// which is shown EXACTLY ONCE (no recovery path).
type RegisterBotResponse struct {
	ID         int64              `json:"id"`
	Username   string             `json:"username"`
	Name       string             `json:"name"`
	Kind       store.CitizenKind  `json:"kind"`
	ParentID   int64              `json:"parent_id"`
	ParentName string             `json:"parent_name"`
	Token      string             `json:"token"`
	Label      string             `json:"label,omitempty"`
	Warning    string             `json:"warning"`
}

// RegisterBot creates a new kind='bot' citizen parented by the
// caller, plus its initial token. Returns ErrInvalidArgument on
// missing name; ErrConflict on username collision.
func RegisterBot(s store.CoordinatorStore, caller *store.CitizenRecord, p RegisterBotParams) (*RegisterBotResponse, error) {
	if p.Name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidArgument)
	}
	username := p.Username
	if username != "" {
		if err := store.ValidateUsername(username); err != nil {
			return nil, fmt.Errorf("%w: %s", ErrInvalidArgument, err.Error())
		}
	} else {
		username = generateUniqueUsername(s, p.Name)
	}
	role := p.Role
	if role == "" {
		role = "citizen"
	}
	token := uuid.New().String()
	now := time.Now()
	res, err := s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.CreateCitizen{
				Citizen: store.CitizenRecord{
					Username:     username,
					Name:         p.Name,
					Role:         role,
					RegisteredAt: now,
					LastSeen:     now,
					Kind:         store.CitizenKindBot,
					ParentID:     &caller.ID,
				},
				Token:      token,
				TokenLabel: p.Label,
			},
		},
	})
	if err != nil {
		if strings.Contains(err.Error(), "already taken") {
			return nil, fmt.Errorf("%w: %s", ErrConflict, err.Error())
		}
		return nil, err
	}
	id := res.CitizenID
	return &RegisterBotResponse{
		ID:         id,
		Username:   username,
		Name:       p.Name,
		Kind:       store.CitizenKindBot,
		ParentID:   caller.ID,
		ParentName: caller.Username,
		Token:      token,
		Label:      p.Label,
		Warning:    "Stash this token now — it cannot be retrieved later. Revoke + re-issue if lost.",
	}, nil
}

// RevokeTokenResponse is the wire shape.
type RevokeTokenResponse struct {
	Revoked bool `json:"revoked"`
}

// RevokeToken marks a token as revoked. Caller must own the
// token directly OR parent the bot that owns it. Returns
// ErrInvalidArgument when neither identifier is provided;
// ErrNotFound when the token doesn't exist; ErrForbidden when
// the caller doesn't own it.
func RevokeToken(s store.CoordinatorStore, caller *store.CitizenRecord, token string, tokenID int64) (*RevokeTokenResponse, error) {
	if token == "" && tokenID == 0 {
		return nil, fmt.Errorf("%w: either token or token_id is required", ErrInvalidArgument)
	}
	ownerID, err := s.LookupTokenOwner(token, tokenID)
	if err != nil {
		return nil, err
	}
	if ownerID == 0 {
		return nil, ErrNotFound
	}
	if ownerID != caller.ID {
		owner, _ := s.GetCitizen(ownerID)
		if owner == nil || owner.Kind != store.CitizenKindBot || owner.ParentID == nil || *owner.ParentID != caller.ID {
			return nil, fmt.Errorf("%w: you don't own this token", ErrForbidden)
		}
	}
	var muts []store.Mutation
	if tokenID != 0 {
		muts = []store.Mutation{store.RevokeToken{TokenID: tokenID}}
	} else {
		muts = []store.Mutation{store.RevokeTokenByValue{Token: token}}
	}
	if _, err := s.ApplyPlan(store.Plan{
		Version:   engine.EngineVersion,
		Mutations: muts,
	}); err != nil {
		return nil, err
	}
	return &RevokeTokenResponse{Revoked: true}, nil
}

// generateUniqueUsername picks an unused slug derived from
// displayName. Mirrors api.Server.generateUniqueUsername.
func generateUniqueUsername(s store.CoordinatorStore, displayName string) string {
	base := store.SlugifyName(displayName)
	if base == "" {
		base = "user"
	}
	candidate := base
	for i := 2; ; i++ {
		c, _ := s.GetCitizenByUsername(candidate)
		if c == nil {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
		if i > 1000 {
			return fmt.Sprintf("%s-%s", base, uuid.New().String()[:6])
		}
	}
}
