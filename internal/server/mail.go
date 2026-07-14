package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/sutantodadang/luncur/internal/mail"
	"github.com/sutantodadang/luncur/internal/store"
)

// smtpMailer is the default mailer factory: it builds a mail.SMTP from
// the smtp_* install settings. mail.ErrUnconfigured when smtp_host is
// unset.
func (s *server) smtpMailer() (mail.Mailer, error) {
	host, err := s.st.GetSetting("smtp_host")
	if errors.Is(err, store.ErrNotFound) {
		return nil, mail.ErrUnconfigured
	}
	if err != nil {
		return nil, err
	}

	port := 587
	if v, err := s.st.GetSetting("smtp_port"); err == nil {
		if n, err := strconv.Atoi(v); err == nil {
			port = n
		}
	}

	user, _ := s.st.GetSetting("smtp_user")
	pass := ""
	if user != "" {
		if pass, err = s.sealedSetting("smtp_pass"); err != nil {
			return nil, err
		}
	}

	from, err := s.st.GetSetting("smtp_from")
	if errors.Is(err, store.ErrNotFound) {
		from = user
		if from == "" {
			from = "luncur@" + host
		}
	} else if err != nil {
		return nil, err
	}

	return mail.SMTP{Host: host, Port: port, User: user, Pass: pass, From: from}, nil
}

// requestBaseURL reconstructs the externally visible base URL of the
// request; luncur may sit behind a TLS-terminating proxy, so honor
// X-Forwarded-Proto.
func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// emailInvite sends the registration link for inv to addr. Shared by the
// API and UI invite-create handlers.
func (s *server) emailInvite(r *http.Request, addr string, inv store.Invite) error {
	m, err := s.mailer()
	if err != nil {
		return err
	}
	link := requestBaseURL(r) + "/ui/register?token=" + inv.Token
	text := "You have been invited to luncur (role " + inv.Role + ").\n\n" +
		"Register here: " + link + "\n\n" +
		"The link is single-use and expires " + inv.ExpiresAt + ".\n"
	html, err := renderInviteHTML(inv.Role, link, inv.ExpiresAt)
	if err != nil {
		return err
	}
	return m.Send(addr, "You're invited to luncur", text, html)
}
