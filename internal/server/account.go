package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/sutantodadang/luncur/internal/store"
)

// handleChangePassword lets a logged-in user change their own password,
// gated on re-proving the current one — same Authenticate core the login
// path uses, so a stolen session token alone can't rotate credentials.
func (s *server) handleChangePassword(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if _, err := s.st.Authenticate(u.Email, req.OldPassword); errors.Is(err, store.ErrAuthFailed) {
		writeError(w, http.StatusForbidden, "wrong_password", "current password is incorrect")
		return
	} else if err != nil {
		log.Printf("change password: authenticate: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if err := s.st.UpdatePassword(u.ID, req.NewPassword); err != nil {
		var ve *store.ValidationError
		if errors.As(err, &ve) {
			writeError(w, http.StatusBadRequest, "bad_request", ve.Error())
			return
		}
		log.Printf("change password: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleChangeEmail lets a logged-in user change their own login email,
// gated on re-proving the current password.
func (s *server) handleChangeEmail(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		Password string `json:"password"`
		Email    string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if _, err := s.st.Authenticate(u.Email, req.Password); errors.Is(err, store.ErrAuthFailed) {
		writeError(w, http.StatusForbidden, "wrong_password", "current password is incorrect")
		return
	} else if err != nil {
		log.Printf("change email: authenticate: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if err := s.st.UpdateEmail(u.ID, req.Email); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: users.email") {
			writeError(w, http.StatusConflict, "email_taken", "email already exists")
			return
		}
		var ve *store.ValidationError
		if errors.As(err, &ve) {
			writeError(w, http.StatusBadRequest, "bad_request", ve.Error())
			return
		}
		log.Printf("change email: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"email": strings.ToLower(strings.TrimSpace(req.Email))})
}

// handleAdminSetPassword resets any user's password (admin only) — no old
// password required, since the admin isn't the account owner.
func (s *server) handleAdminSetPassword(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid user id")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if err := s.st.UpdatePassword(id, req.Password); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such user")
			return
		}
		var ve *store.ValidationError
		if errors.As(err, &ve) {
			writeError(w, http.StatusBadRequest, "bad_request", ve.Error())
			return
		}
		log.Printf("admin set password: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
