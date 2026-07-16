package server

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/sutantodadang/luncur/internal/store"
)

func (s *server) handleUIUsers(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	users, err := s.st.ListUsers()
	if err != nil {
		log.Printf("ui users: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	invites, err := s.st.ListInvites()
	if err != nil {
		log.Printf("ui invites: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rows := make([]uiInviteRow, 0, len(invites))
	for _, i := range invites {
		rows = append(rows, uiInviteRow{Token: i.Token, Role: i.Role, ExpiresAt: i.ExpiresAt, Used: i.UsedBy != 0})
	}
	var mailNote string
	switch r.URL.Query().Get("mail") {
	case "sent":
		mailNote = "invite emailed"
	case "failed":
		mailNote = "invite created, but the email failed — copy the link below"
	}
	var pwNote string
	switch r.URL.Query().Get("pw") {
	case "ok":
		pwNote = "password updated"
	case "invalid":
		pwNote = "password must be at least 8 characters"
	case "missing":
		pwNote = "no such user"
	}
	s.renderPage(w, "users.html", map[string]any{
		"User": u, "Users": users, "Invites": rows, "Self": u.ID,
		"CSRF": s.csrf(w, r), "IsAdmin": u.Role == "admin",
		"MailNote": mailNote, "PwNote": pwNote,
	})
}

// handleUIUserPassword is admin-only password reset for any user — no old
// password required (the admin isn't the account owner).
func (s *server) handleUIUserPassword(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}
	if err := s.st.UpdatePassword(id, r.PostFormValue("password")); err != nil {
		var ve *store.ValidationError
		switch {
		case errors.Is(err, store.ErrNotFound):
			http.Redirect(w, r, "/ui/users?pw=missing", http.StatusSeeOther)
		case errors.As(err, &ve):
			http.Redirect(w, r, "/ui/users?pw=invalid", http.StatusSeeOther)
		default:
			log.Printf("ui admin set password: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	flash(w, "ok", "password reset")
	http.Redirect(w, r, "/ui/users?pw=ok", http.StatusSeeOther)
}

// accountNote maps the account page's fixed ok/err query params to display
// strings — never the submitted value itself, so nothing user-supplied ever
// gets echoed back into the page.
func accountNote(r *http.Request) (note, errMsg string) {
	switch r.URL.Query().Get("ok") {
	case "password":
		note = "password changed"
	case "email":
		note = "email changed — use it on your next login"
	}
	switch r.URL.Query().Get("err") {
	case "wrong":
		errMsg = "current password is incorrect"
	case "invalid":
		errMsg = "invalid input (password min 8 chars, email required)"
	case "taken":
		errMsg = "that email is already in use"
	}
	return note, errMsg
}

// handleUIAccount is the self-service account page: change own password or
// email, both gated on the current password (checked by the POST handlers,
// not here).
func (s *server) handleUIAccount(w http.ResponseWriter, r *http.Request, u store.User) {
	note, errMsg := accountNote(r)
	s.renderPage(w, "account.html", map[string]any{
		"User": u, "CSRF": s.csrf(w, r), "IsAdmin": u.Role == "admin",
		"Note": note, "Error": errMsg,
	})
}

// handleUIAccountPassword is handleChangePassword's UI twin: same
// Authenticate-then-UpdatePassword core, redirect with a fixed ?ok=/?err=
// note instead of a JSON envelope.
func (s *server) handleUIAccountPassword(w http.ResponseWriter, r *http.Request, u store.User) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if _, err := s.st.Authenticate(u.Email, r.PostFormValue("old")); errors.Is(err, store.ErrAuthFailed) {
		http.Redirect(w, r, "/ui/account?err=wrong", http.StatusSeeOther)
		return
	} else if err != nil {
		log.Printf("ui change password: authenticate: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.st.UpdatePassword(u.ID, r.PostFormValue("new")); err != nil {
		var ve *store.ValidationError
		if errors.As(err, &ve) {
			http.Redirect(w, r, "/ui/account?err=invalid", http.StatusSeeOther)
			return
		}
		log.Printf("ui change password: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "password changed")
	http.Redirect(w, r, "/ui/account?ok=password", http.StatusSeeOther)
}

// handleUIAccountEmail is handleChangeEmail's UI twin: same
// Authenticate-then-UpdateEmail core, redirect with a fixed ?ok=/?err= note
// instead of a JSON envelope.
func (s *server) handleUIAccountEmail(w http.ResponseWriter, r *http.Request, u store.User) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if _, err := s.st.Authenticate(u.Email, r.PostFormValue("password")); errors.Is(err, store.ErrAuthFailed) {
		http.Redirect(w, r, "/ui/account?err=wrong", http.StatusSeeOther)
		return
	} else if err != nil {
		log.Printf("ui change email: authenticate: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.st.UpdateEmail(u.ID, r.PostFormValue("email")); err != nil {
		var ve *store.ValidationError
		switch {
		case strings.Contains(err.Error(), "UNIQUE constraint failed: users.email"):
			http.Redirect(w, r, "/ui/account?err=taken", http.StatusSeeOther)
		case errors.As(err, &ve):
			http.Redirect(w, r, "/ui/account?err=invalid", http.StatusSeeOther)
		default:
			log.Printf("ui change email: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	flash(w, "ok", "email changed")
	http.Redirect(w, r, "/ui/account?ok=email", http.StatusSeeOther)
}

// handleUITokens lists the caller's own tokens — the UI twin of
// GET /v1/tokens. The web session rides the same table (name "session").
func (s *server) handleUITokens(w http.ResponseWriter, r *http.Request, u store.User) {
	tokens, err := s.st.ListTokens(u.ID)
	if err != nil {
		log.Printf("ui tokens: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.renderPage(w, "tokens.html", map[string]any{
		"User": u, "Tokens": tokens,
		"CSRF": s.csrf(w, r), "IsAdmin": u.Role == "admin",
	})
}

// handleUITokenRevoke revokes one of the caller's tokens. Revoking the
// current session's token logs the browser out on the next request.
func (s *server) handleUITokenRevoke(w http.ResponseWriter, r *http.Request, u store.User) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid token id", http.StatusBadRequest)
		return
	}
	if err := s.st.RevokeToken(u.ID, id); err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("ui revoke token: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "token revoked")
	http.Redirect(w, r, "/ui/tokens", http.StatusSeeOther)
}

func (s *server) handleUIInviteCreate(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	role := r.PostFormValue("role")
	if role == "" {
		role = "member"
	}
	inv, err := s.st.CreateInvite(role, u.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dest := "/ui/users"
	if email := strings.TrimSpace(r.PostFormValue("email")); email != "" {
		if err := s.emailInvite(r, email, inv); err != nil {
			log.Printf("ui invite email to %s: %v", email, err)
			dest += "?mail=failed"
		} else {
			dest += "?mail=sent"
		}
	}
	flash(w, "ok", "invite created")
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// handleUIAdopt is handleAdoptApp's UI twin: clear the ejected flag,
// best-effort re-sync, redirect back to the app page.
func (s *server) handleUIAdopt(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	if !a.Ejected {
		http.Error(w, "app is not ejected", http.StatusConflict)
		return
	}
	if err := s.st.SetAppAdopted(a.ID); err != nil {
		log.Printf("ui adopt: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.Ejected = false
	env, ok := s.uiAppEnv(w, a)
	if !ok {
		return
	}
	s.syncIfLive(r.Context(), p, env, a)
	flash(w, "ok", "app adopted")
	uiRedirect(w, r, p, a)
}

func (s *server) handleUIInviteRevoke(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if err := s.st.RevokeInvite(r.PostFormValue("token")); err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("ui revoke invite: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "invite revoked")
	http.Redirect(w, r, "/ui/users", http.StatusSeeOther)
}

func (s *server) handleUIUserDelete(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}
	if id == u.ID {
		http.Error(w, "cannot delete yourself", http.StatusBadRequest)
		return
	}
	if err := s.st.DeleteUser(id); err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("ui delete user: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "user deleted")
	http.Redirect(w, r, "/ui/users", http.StatusSeeOther)
}
