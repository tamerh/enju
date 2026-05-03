package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/enju-ai/enju/internal/coordinator/service"
)

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// writeFailErr maps service sentinels for fail-task to HTTP
// status codes that match the historical REST behavior.
func writeFailErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidArgument):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, service.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, service.ErrForbidden):
		writeError(w, http.StatusForbidden, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// writeMembershipErr maps the service-layer membership-mutation
// sentinels to HTTP status codes. Used by handleSetProjectDefaultBranch
// + handleAdd/RemoveProjectMember + handleSetProjectMemberRole.
func writeMembershipErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidArgument):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, service.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, service.ErrForbidden):
		writeError(w, http.StatusForbidden, err.Error())
	case err == service.ErrNotMember:
		writeError(w, http.StatusForbidden, "not a member of this project")
	case err == service.ErrNotFound:
		writeError(w, http.StatusNotFound, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}
