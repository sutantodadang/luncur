package server

import (
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/sutantodadang/luncur/internal/store"
)

// handleUISSHKeys lists the logged-in user's own keys — no admin gate, same
// scoping as GET /v1/ssh-keys.
func (s *server) handleUISSHKeys(w http.ResponseWriter, r *http.Request, u store.User) {
	list, err := s.st.ListSSHKeys(u.ID)
	if err != nil {
		log.Printf("ui ssh keys: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.renderPage(w, "sshkeys.html", map[string]any{
		"User": u, "Keys": list, "CSRF": s.csrf(w, r), "IsAdmin": u.Role == "admin",
	})
}

// handleUISSHKeyAdd is handleAddSSHKey's UI twin: same store.AddSSHKey
// validation core, redirect instead of a 201 body.
func (s *server) handleUISSHKeyAdd(w http.ResponseWriter, r *http.Request, u store.User) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	name := r.PostFormValue("name")
	if name == "" {
		name = "key"
	}
	if _, err := s.st.AddSSHKey(u.ID, name, r.PostFormValue("public_key")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/ui/sshkeys", http.StatusSeeOther)
}

// handleUISSHKeyDelete is handleDeleteSSHKey's UI twin: same store.DeleteSSHKey
// core (scoped to the caller's own keys), redirect instead of a 204.
func (s *server) handleUISSHKeyDelete(w http.ResponseWriter, r *http.Request, u store.User) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid key id", http.StatusBadRequest)
		return
	}
	if err := s.st.DeleteSSHKey(u.ID, id); err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("ui delete ssh key: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/ui/sshkeys", http.StatusSeeOther)
}
