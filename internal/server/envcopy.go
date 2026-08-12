package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/sutantodadang/luncur/internal/store"
)

// envCopySummary reports what copyEnvSetup changed in the target
// environment. Warnings carry per-addon clone failures and immutable-field
// mismatches — conditions worth surfacing without failing the whole copy.
type envCopySummary struct {
	AppsCreated  int      `json:"apps_created"`
	AppsUpdated  int      `json:"apps_updated"`
	AddonsCloned int      `json:"addons_cloned"`
	Warnings     []string `json:"warnings"`
}

// copyEnvSetup copies source's setup into target: every source app is
// created in (or overwritten onto) target, then addons whose type target
// lacks are cloned. Target-only apps are never deleted and nothing is
// redeployed — the new config applies on each app's next deploy. A per-app
// failure aborts (partial progress stays, matching preview clone's
// non-atomicity); addon failures only warn.
func (s *server) copyEnvSetup(ctx context.Context, source, target store.Environment) (envCopySummary, error) {
	sum := envCopySummary{Warnings: []string{}}
	apps, err := s.st.ListAppsInEnv(source.ID)
	if err != nil {
		return sum, fmt.Errorf("list source apps: %w", err)
	}
	for _, src := range apps {
		tgt, err := s.st.GetAppInEnv(target.ID, src.Name)
		created := false
		if errors.Is(err, store.ErrNotFound) {
			tgt, err = s.st.CreateAppInEnv(target.ID, src.Name, src.Port, src.Kind, src.Schedule)
			created = true
		}
		if err != nil {
			return sum, fmt.Errorf("app %s: %w", src.Name, err)
		}
		if !created && (tgt.Kind != src.Kind || tgt.Port != src.Port || tgt.Schedule != src.Schedule) {
			sum.Warnings = append(sum.Warnings, fmt.Sprintf(
				"app %s: kind/port/schedule differ from source and are create-time-only; other config and vars copied", src.Name))
		}
		if err := s.applyAppSetup(tgt.ID, src); err != nil {
			return sum, fmt.Errorf("app %s: %w", src.Name, err)
		}
		if created {
			sum.AppsCreated++
		} else {
			sum.AppsUpdated++
		}
	}
	cloned, warns := s.cloneEnvAddons(ctx, source, target)
	sum.AddonsCloned = cloned
	sum.Warnings = append(sum.Warnings, warns...)
	return sum, nil
}

// applyAppSetup overwrites one app's mutable config and whole env-var set
// from src: replicas, resources, health path, internal, gpu, inject-s3,
// git source (when src is git), then ReplaceEnv with src's sealed vars —
// target-only vars are deleted. Every setter runs unconditionally so a
// zero/empty source value clears the target's (overwrite semantics, unlike
// clonePreviewApp's set-only-if-nonzero on a fresh app).
func (s *server) applyAppSetup(appID int64, src store.App) error {
	if err := s.st.SetReplicas(appID, src.Replicas); err != nil {
		return fmt.Errorf("set replicas: %w", err)
	}
	if err := s.st.SetResources(appID, src.CPUMilli, src.MemoryMB); err != nil {
		return fmt.Errorf("set resources: %w", err)
	}
	if err := s.st.SetHealthPath(appID, src.HealthPath); err != nil {
		return fmt.Errorf("set health path: %w", err)
	}
	if err := s.st.SetInternal(appID, src.Internal); err != nil {
		return fmt.Errorf("set internal: %w", err)
	}
	if err := s.st.SetGPU(appID, src.GPUCount); err != nil {
		return fmt.Errorf("set gpu: %w", err)
	}
	if err := s.st.SetInjectS3(appID, src.InjectS3); err != nil {
		return fmt.Errorf("set inject s3: %w", err)
	}
	if src.SourceType == "git" {
		if err := s.st.SetAppGitSource(appID, src.GitURL, src.GitBranch); err != nil {
			return fmt.Errorf("set git source: %w", err)
		}
	}
	vars, err := s.st.ListEnv(src.ID)
	if err != nil {
		return fmt.Errorf("list source env vars: %w", err)
	}
	if err := s.st.ReplaceEnv(appID, vars); err != nil {
		return fmt.Errorf("replace env vars: %w", err)
	}
	return nil
}

// handleCopyEnvSetup copies one environment's setup into another (apps'
// mutable config + sealed env vars + missing addon types). Overwrites
// matching target apps, creates missing ones, never deletes target-only
// apps, never redeploys. Write access to the project covers both envs.
func (s *server) handleCopyEnvSetup(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProjectWrite(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	var req struct {
		Source string `json:"source"`
		Target string `json:"target"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Source == req.Target {
		writeError(w, http.StatusBadRequest, "bad_request", "source and target must differ")
		return
	}
	source, err := s.st.GetEnvironment(p.ID, req.Source)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such source environment")
			return
		}
		log.Printf("copy env setup: get source: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	target, err := s.st.GetEnvironment(p.ID, req.Target)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such target environment")
			return
		}
		log.Printf("copy env setup: get target: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	sum, err := s.copyEnvSetup(r.Context(), source, target)
	if err != nil {
		log.Printf("copy env setup %s -> %s: %v", source.Name, target.Name, err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, sum)
}
