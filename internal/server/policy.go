package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/sutantodadang/luncur/internal/store"
)

// policyPatch is a partial app policy update: nil fields keep their value.
type policyPatch struct {
	AutoRollback     *bool   `json:"auto_rollback"`
	RolloutTimeout   *int    `json:"rollout_timeout"`
	Probe            *string `json:"probe"`
	Security         *string `json:"security"`
	Strategy         *string `json:"strategy"`
	CanarySteps      *string `json:"canary_steps"`
	CanaryInterval   *int    `json:"canary_interval"`
	CanaryMinSuccess *int    `json:"canary_min_success"`
	BlueGreenKeep    *int    `json:"bluegreen_keep"`
}

func (pp policyPatch) apply(p store.AppPolicy) (store.AppPolicy, error) {
	if pp.AutoRollback != nil {
		p.AutoRollback = *pp.AutoRollback
	}
	if pp.RolloutTimeout != nil {
		p.RolloutTimeout = *pp.RolloutTimeout
	}
	if pp.Probe != nil {
		p.Probe = *pp.Probe
	}
	if pp.Security != nil {
		p.Security = *pp.Security
	}
	if pp.Strategy != nil {
		p.Strategy = *pp.Strategy
	}
	if pp.CanarySteps != nil {
		steps, err := store.ParseCanarySteps(*pp.CanarySteps)
		if err != nil {
			return p, err
		}
		p.CanarySteps = steps
	}
	if pp.CanaryInterval != nil {
		p.CanaryInterval = *pp.CanaryInterval
	}
	if pp.CanaryMinSuccess != nil {
		p.CanaryMinSuccess = *pp.CanaryMinSuccess
	}
	if pp.BlueGreenKeep != nil {
		p.BlueGreenKeep = *pp.BlueGreenKeep
	}
	return p, p.Validate()
}

// strategyError explains why an app can't use a non-rolling strategy.
func (s *server) strategyError(a store.App, strategy string) error {
	if strategy == "rolling" {
		return nil
	}
	if a.Kind != "" && a.Kind != "web" {
		return fmt.Errorf("%s deploys need a web app (this one is %s)", strategy, a.Kind)
	}
	if vols, err := s.st.ListVolumes(a.ID); err == nil && len(vols) > 0 {
		return fmt.Errorf("%s deploys can't run two copies of an app with volumes (ReadWriteOnce storage)", strategy)
	}
	if a.GPUCount > 0 {
		return fmt.Errorf("%s deploys would need twice the GPUs; use rolling for GPU apps", strategy)
	}
	return nil
}

// setAppPolicy is the shared core of the policy API and UI: merge, validate,
// persist, then re-sync a live app so render-affecting fields (probe,
// security, rollout timeout) reach the cluster.
func (s *server) setAppPolicy(ctx context.Context, p store.Project, env store.Environment, a store.App, patch policyPatch) (store.AppPolicy, error) {
	if a.Ejected {
		return store.AppPolicy{}, errAppEjected
	}
	cur, err := s.st.GetAppPolicy(a.ID)
	if err != nil {
		return store.AppPolicy{}, err
	}
	next, err := patch.apply(cur)
	if err != nil {
		return store.AppPolicy{}, err
	}
	if err := s.strategyError(a, next.Strategy); err != nil {
		return store.AppPolicy{}, err
	}
	if err := s.st.SetAppPolicy(next); err != nil {
		return store.AppPolicy{}, err
	}
	if next.Probe != cur.Probe || next.Security != cur.Security || next.RolloutTimeout != cur.RolloutTimeout {
		s.syncIfLive(ctx, p, env, a)
	}
	return next, nil
}

func (s *server) handleGetPolicy(w http.ResponseWriter, r *http.Request, u store.User) {
	p, env, ok := s.requireEnv(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, env, r.PathValue("app"))
	if !ok {
		return
	}
	pol, err := s.st.GetAppPolicy(a.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, pol)
}

func (s *server) handlePutPolicy(w http.ResponseWriter, r *http.Request, u store.User) {
	p, env, ok := s.requireEnvWrite(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, env, r.PathValue("app"))
	if !ok {
		return
	}
	var patch policyPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	pol, err := s.setAppPolicy(r.Context(), p, env, a, patch)
	if err != nil {
		if errors.Is(err, errAppEjected) {
			writeError(w, http.StatusConflict, "app_ejected", errAppEjected.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pol)
}

// handleUIPolicy is handlePutPolicy's UI twin (Wire tab "Rollout" card).
// Checkbox semantics: an unchecked auto_rollback box is absent from the
// form, so the card always posts auto_rollback_present to say "this form
// carries the toggle".
func (s *server) handleUIPolicy(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	env, ok := s.uiAppEnv(w, a)
	if !ok {
		return
	}
	var patch policyPatch
	if r.PostFormValue("auto_rollback_present") != "" {
		on := r.PostFormValue("auto_rollback") != ""
		patch.AutoRollback = &on
	}
	intField := func(name string) (*int, error) {
		v := strings.TrimSpace(r.PostFormValue(name))
		if v == "" {
			return nil, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("%s must be a whole number", strings.ReplaceAll(name, "_", " "))
		}
		return &n, nil
	}
	strField := func(name string) *string {
		if v := strings.TrimSpace(r.PostFormValue(name)); v != "" {
			return &v
		}
		return nil
	}
	var err error
	if patch.RolloutTimeout, err = intField("rollout_timeout"); err == nil {
		if patch.CanaryInterval, err = intField("canary_interval"); err == nil {
			if patch.CanaryMinSuccess, err = intField("canary_min_success"); err == nil {
				patch.BlueGreenKeep, err = intField("bluegreen_keep")
			}
		}
	}
	if err == nil {
		patch.Probe, patch.Security, patch.Strategy, patch.CanarySteps =
			strField("probe"), strField("security"), strField("strategy"), strField("canary_steps")
		_, err = s.setAppPolicy(r.Context(), p, env, a, patch)
	}
	if err != nil {
		flash(w, "err", err.Error())
		uiRedirect(w, r, p, a, tabWire)
		return
	}
	flash(w, "ok", "rollout policy saved")
	uiRedirect(w, r, p, a, tabWire)
}

// uiPolicy is the Wire tab's view of an app's policy.
type uiPolicy struct {
	store.AppPolicy
	Steps string
	// StrategyBlocked explains why canary/blue-green are unavailable ("" =
	// available).
	StrategyBlocked string
}

func (s *server) appPolicyView(a store.App) uiPolicy {
	pol, err := s.st.GetAppPolicy(a.ID)
	if err != nil {
		pol = store.DefaultAppPolicy(a.ID)
	}
	v := uiPolicy{AppPolicy: pol, Steps: store.FormatCanarySteps(pol.CanarySteps)}
	if err := s.strategyError(a, "canary"); err != nil {
		v.StrategyBlocked = err.Error()
	}
	return v
}
