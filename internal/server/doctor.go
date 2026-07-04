package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sutantodadang/luncur/internal/registry"
	"github.com/sutantodadang/luncur/internal/store"
)

// doctorCheck is one named diagnostic result: status is "ok", "warn", or
// "fail", and detail is a short human-readable explanation.
type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// doctorTimeout bounds each individual check so one slow dependency (a
// wedged kube API, an unreachable registry) can't hang the whole sweep.
const doctorTimeout = 5 * time.Second

// runDoctor runs every check in fixed order; a failing check never stops
// the others from running.
func (s *server) runDoctor(ctx context.Context) []doctorCheck {
	return []doctorCheck{
		s.checkDatabase(ctx),
		s.checkKubernetes(ctx),
		s.checkRegistry(ctx),
		s.checkBuilds(ctx),
		s.checkIngress(ctx),
		s.checkCertificates(ctx),
		s.checkSMTP(ctx),
		s.checkNotifications(ctx),
		s.checkBackups(ctx),
	}
}

func (s *server) checkDatabase(ctx context.Context) doctorCheck {
	_, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	if err := s.st.Ping(); err != nil {
		return doctorCheck{Name: "database", Status: "fail", Detail: err.Error()}
	}
	return doctorCheck{Name: "database", Status: "ok", Detail: "reachable"}
}

func (s *server) checkKubernetes(ctx context.Context) doctorCheck {
	if s.kube == nil {
		return doctorCheck{Name: "kubernetes", Status: "fail", Detail: "kubernetes is not configured"}
	}
	cctx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	total, notReady, err := s.kube.NodesReady(cctx)
	if err != nil {
		return doctorCheck{Name: "kubernetes", Status: "fail", Detail: err.Error()}
	}
	if len(notReady) > 0 {
		return doctorCheck{Name: "kubernetes", Status: "fail",
			Detail: fmt.Sprintf("node %s not ready", strings.Join(notReady, ", "))}
	}
	return doctorCheck{Name: "kubernetes", Status: "ok",
		Detail: fmt.Sprintf("%d node(s) ready", total)}
}

func (s *server) checkRegistry(ctx context.Context) doctorCheck {
	cctx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	reg := &registry.Client{Host: s.registryHost}
	repos, err := reg.Repositories(cctx)
	if err != nil {
		return doctorCheck{Name: "registry", Status: "fail", Detail: err.Error()}
	}
	return doctorCheck{Name: "registry", Status: "ok",
		Detail: fmt.Sprintf("%d repositories", len(repos))}
}

func (s *server) checkBuilds(ctx context.Context) doctorCheck {
	_, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	stuck, err := s.st.StuckDeployments(30)
	if err != nil {
		return doctorCheck{Name: "builds", Status: "fail", Detail: err.Error()}
	}
	if len(stuck) == 0 {
		return doctorCheck{Name: "builds", Status: "ok", Detail: "no stuck builds"}
	}
	ids := make([]string, len(stuck))
	for i, d := range stuck {
		ids[i] = fmt.Sprintf("%d", d.ID)
	}
	return doctorCheck{Name: "builds", Status: "warn",
		Detail: fmt.Sprintf("deploy(s) %s building for >30m — builder job stuck or builder image missing",
			strings.Join(ids, ", "))}
}

func (s *server) checkIngress(ctx context.Context) doctorCheck {
	if s.kube == nil {
		return doctorCheck{Name: "ingress", Status: "fail", Detail: "kubernetes is not configured"}
	}
	cctx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	ready, total, err := s.kube.ReadyPods(cctx, "kube-system", "app.kubernetes.io/name=traefik")
	if err != nil {
		return doctorCheck{Name: "ingress", Status: "fail", Detail: err.Error()}
	}
	if ready == 0 {
		if total == 0 {
			return doctorCheck{Name: "ingress", Status: "fail", Detail: "no traefik pods found"}
		}
		return doctorCheck{Name: "ingress", Status: "fail",
			Detail: fmt.Sprintf("%d/%d traefik pod(s) ready", ready, total)}
	}
	return doctorCheck{Name: "ingress", Status: "ok",
		Detail: fmt.Sprintf("%d/%d traefik pod(s) ready", ready, total)}
}

func (s *server) checkCertificates(ctx context.Context) doctorCheck {
	_, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	domains, err := s.st.AllDomains()
	if err != nil {
		return doctorCheck{Name: "certificates", Status: "fail", Detail: err.Error()}
	}
	if len(domains) == 0 {
		return doctorCheck{Name: "certificates", Status: "ok", Detail: "no custom domains"}
	}
	var failing []string
	for _, d := range domains {
		if d.CertStatus == "failed" {
			failing = append(failing, d.Hostname)
		}
	}
	if len(failing) == 0 {
		return doctorCheck{Name: "certificates", Status: "ok",
			Detail: fmt.Sprintf("%d domain(s), none failing", len(domains))}
	}
	return doctorCheck{Name: "certificates", Status: "warn",
		Detail: "failing: " + strings.Join(failing, ", ")}
}

func (s *server) checkSMTP(ctx context.Context) doctorCheck {
	_, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	if _, err := s.st.GetSetting("smtp_host"); err == nil {
		return doctorCheck{Name: "smtp", Status: "ok", Detail: "configured"}
	}
	return doctorCheck{Name: "smtp", Status: "warn", Detail: "not configured — invite emails disabled"}
}

func (s *server) checkNotifications(ctx context.Context) doctorCheck {
	_, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	if _, err := s.st.GetSetting("notify_url"); err == nil {
		return doctorCheck{Name: "notifications", Status: "ok", Detail: "configured"}
	}
	return doctorCheck{Name: "notifications", Status: "warn", Detail: "not configured"}
}

func (s *server) checkBackups(ctx context.Context) doctorCheck {
	_, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	v, err := s.st.GetSetting("backup_schedule")
	if err == nil && v == "daily" {
		return doctorCheck{Name: "backups", Status: "ok", Detail: "daily"}
	}
	return doctorCheck{Name: "backups", Status: "warn", Detail: "scheduled backups off"}
}

func (s *server) handleDoctor(w http.ResponseWriter, r *http.Request, _ store.User) {
	writeJSON(w, http.StatusOK, map[string]any{
		"server_version": s.version,
		"checks":         s.runDoctor(r.Context()),
	})
}
