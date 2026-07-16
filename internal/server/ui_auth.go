package server

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/sutantodadang/luncur/internal/store"
)

func (s *server) handleUILoginPage(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, "login.html", map[string]any{"CSRF": s.csrf(w, r)})
}

func (s *server) handleUILogin(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderPage(w, "login.html", map[string]any{"Error": "invalid form", "CSRF": s.csrf(w, r)})
		return
	}
	u, err := s.st.Authenticate(r.PostFormValue("email"), r.PostFormValue("password"))
	if errors.Is(err, store.ErrAuthFailed) {
		s.renderPage(w, "login.html", map[string]any{"Error": "wrong email or password", "CSRF": s.csrf(w, r)})
		return
	}
	if err != nil {
		log.Printf("ui login: %v", err)
		s.renderPage(w, "login.html", map[string]any{"Error": "internal error", "CSRF": s.csrf(w, r)})
		return
	}
	tok, err := s.st.CreateSessionToken(u.ID, "session")
	if err != nil {
		log.Printf("ui session token: %v", err)
		s.renderPage(w, "login.html", map[string]any{"Error": "internal error", "CSRF": s.csrf(w, r)})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Expires: time.Now().Add(7 * 24 * time.Hour),
	})
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

func (s *server) handleUILogout(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(w, r) {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}

// registerPageData builds register.html's view-model for a given token,
// looking the invite up fresh so GET and the POST error paths render
// identically.
func (s *server) registerPageData(w http.ResponseWriter, r *http.Request, token string, extra map[string]any) map[string]any {
	inv, err := s.st.GetValidInvite(token)
	data := map[string]any{"CSRF": s.csrf(w, r), "Token": token, "Valid": err == nil, "Role": inv.Role}
	for k, v := range extra {
		data[k] = v
	}
	return data
}

// handleUIRegisterPage is session-less: anyone with a valid invite link can
// reach it without being logged in.
func (s *server) handleUIRegisterPage(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, "register.html", s.registerPageData(w, r, r.URL.Query().Get("token"), nil))
}

// handleUIRegister is session-less, like handleUIRegisterPage: checkCSRF
// first, then re-validate the token (it may have been burned or expired
// since the form was loaded), create the user with the INVITE's role (never
// a client-supplied one), burn the invite, and log the new user straight in
// with the exact session-cookie shape handleUILogin uses.
func (s *server) handleUIRegister(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(w, r) {
		return
	}
	token := r.PostFormValue("token")
	inv, err := s.st.GetValidInvite(token)
	if err != nil {
		s.renderPage(w, "register.html", s.registerPageData(w, r, token, nil))
		return
	}

	u, err := s.st.CreateUser(r.PostFormValue("email"), r.PostFormValue("password"), inv.Role)
	if err != nil {
		errMsg := "internal error"
		var ve *store.ValidationError
		switch {
		case strings.Contains(err.Error(), "UNIQUE constraint failed: users.email"):
			errMsg = "an account with that email already exists"
		case errors.As(err, &ve):
			errMsg = ve.Error()
		default:
			log.Printf("ui register create user: %v", err)
		}
		// Invite stays unburned: re-render the same form the user just
		// filled in, with an error, so the token is still usable.
		s.renderPage(w, "register.html", s.registerPageData(w, r, token, map[string]any{"Error": errMsg}))
		return
	}

	if err := s.st.MarkInviteUsed(token, u.ID); err != nil {
		// The user account already exists at this point; a failure here
		// just means the invite could be replayed, not that registration
		// failed, so we log and continue rather than error out.
		log.Printf("ui register mark invite used: %v", err)
	}

	tok, err := s.st.CreateSessionToken(u.ID, "session")
	if err != nil {
		log.Printf("ui register session token: %v", err)
		s.renderPage(w, "register.html", s.registerPageData(w, r, token, map[string]any{"Error": "internal error"}))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Expires: time.Now().Add(7 * 24 * time.Hour),
	})
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}
