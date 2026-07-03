package server

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sutantodadang/luncur/internal/registry"
	"github.com/sutantodadang/luncur/internal/store"
)

// gcReport summarizes one registry GC sweep. BytesReclaimed is -1 when the
// exec phase (du/garbage-collect in the registry pod) failed or kube is
// unavailable — the manifest-delete phase still ran and its count is
// accurate regardless.
type gcReport struct {
	DeletedManifests int
	BytesReclaimed   int64
	Warnings         []string
}

// registryKeep reads the registry_keep setting, defaulting to 10.
func (s *server) registryKeep() int {
	const defaultKeep = 10
	v, err := s.st.GetSetting("registry_keep")
	if err != nil {
		return defaultKeep
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return defaultKeep
	}
	return n
}

// deployRefs builds registry.DeployRef entries from every deployment in
// the DB whose image lives on this server's registry host: one per
// project -> app -> ListDeployments (already newest-first), Live from the
// row's status and Newest from position within its app.
func (s *server) deployRefs() ([]registry.DeployRef, error) {
	projects, err := s.st.ListProjects()
	if err != nil {
		return nil, err
	}
	prefix := s.registryHost + "/"
	var refs []registry.DeployRef
	for _, p := range projects {
		apps, err := s.st.ListApps(p.ID)
		if err != nil {
			return nil, err
		}
		for _, a := range apps {
			deployments, err := s.st.ListDeployments(a.ID) // newest first
			if err != nil {
				return nil, err
			}
			for i, d := range deployments {
				rest, ok := strings.CutPrefix(d.ImageRef, prefix)
				if !ok {
					continue
				}
				repo, tag, ok := strings.Cut(rest, ":")
				if !ok {
					continue
				}
				refs = append(refs, registry.DeployRef{
					Repo: repo, Tag: tag,
					Live:   d.Status == "live",
					Newest: i == 0,
				})
			}
		}
	}
	return refs, nil
}

// runRegistryGC computes the keep-set from the DB, deletes every manifest
// outside it (including whole repositories absent from the DB), then execs
// the exec phase (du + registry garbage-collect) to reclaim blob storage.
// Nothing is deleted unless the keep-set was built successfully; a DB
// error aborts before any DELETE call.
func (s *server) runRegistryGC(ctx context.Context) (gcReport, error) {
	var report gcReport

	refs, err := s.deployRefs()
	if err != nil {
		return gcReport{}, err
	}
	keepSet := registry.KeepTags(refs, s.registryKeep())

	reg := &registry.Client{Host: s.registryHost}
	repos, err := reg.Repositories(ctx)
	if err != nil {
		return gcReport{}, err
	}
	for _, repo := range repos {
		tags, err := reg.Tags(ctx, repo)
		if err != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("list tags %s: %v", repo, err))
			continue
		}
		keepTags := keepSet[repo]
		for _, tag := range tags {
			if keepTags[tag] {
				continue
			}
			digest, err := reg.Digest(ctx, repo, tag)
			if err != nil {
				report.Warnings = append(report.Warnings, fmt.Sprintf("digest %s:%s: %v", repo, tag, err))
				continue
			}
			if err := reg.DeleteManifest(ctx, repo, digest); err != nil {
				report.Warnings = append(report.Warnings, fmt.Sprintf("delete %s@%s: %v", repo, digest, err))
				continue
			}
			report.DeletedManifests++
		}
	}

	bytesReclaimed, err := s.execRegistryGC(ctx)
	if err != nil {
		report.Warnings = append(report.Warnings, fmt.Sprintf("blob reclamation: %v", err))
		report.BytesReclaimed = -1
	} else {
		report.BytesReclaimed = bytesReclaimed
	}

	return report, nil
}

// execRegistryGC finds the registry pod, measures /var/lib/registry usage
// with `du -sk` before and after running `registry garbage-collect`, and
// returns the bytes reclaimed. Any failure (no kube, no pod, exec error,
// unparsable du output) is returned as an error for the caller to
// downgrade to a warning — the manifest-delete phase already ran either way.
func (s *server) execRegistryGC(ctx context.Context) (int64, error) {
	if s.kube == nil || s.execer == nil {
		return 0, fmt.Errorf("kubernetes unavailable")
	}
	pods, err := s.kube.AppPods(ctx, s.systemNamespace, "registry")
	if err != nil {
		return 0, err
	}
	if len(pods) == 0 {
		return 0, fmt.Errorf("no registry pod found")
	}
	pod := pods[0]

	before, err := s.duRegistryKiB(ctx, pod)
	if err != nil {
		return 0, fmt.Errorf("du before: %w", err)
	}

	var out, errBuf bytes.Buffer
	if err := s.execer.ExecPod(ctx, s.systemNamespace, pod, "registry",
		[]string{"registry", "garbage-collect", "--delete-untagged=false", "/etc/docker/registry/config.yml"},
		&out, &errBuf); err != nil {
		return 0, fmt.Errorf("garbage-collect: %v: %s", err, strings.TrimSpace(errBuf.String()))
	}

	after, err := s.duRegistryKiB(ctx, pod)
	if err != nil {
		return 0, fmt.Errorf("du after: %w", err)
	}
	return (before - after) * 1024, nil
}

// duRegistryKiB runs `du -sk /var/lib/registry` in the registry pod and
// parses the leading KiB field of busybox du's output.
func (s *server) duRegistryKiB(ctx context.Context, pod string) (int64, error) {
	var out, errBuf bytes.Buffer
	if err := s.execer.ExecPod(ctx, s.systemNamespace, pod, "registry",
		[]string{"du", "-sk", "/var/lib/registry"}, &out, &errBuf); err != nil {
		return 0, fmt.Errorf("%v: %s", err, strings.TrimSpace(errBuf.String()))
	}
	fields := strings.Fields(out.String())
	if len(fields) == 0 {
		return 0, fmt.Errorf("empty du output")
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse du output %q: %w", out.String(), err)
	}
	return n, nil
}

func (s *server) handleRegistryGC(w http.ResponseWriter, r *http.Request, _ store.User) {
	report, err := s.runRegistryGC(r.Context())
	if err != nil {
		log.Printf("registry gc: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	warnings := report.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted_manifests": report.DeletedManifests,
		"bytes_reclaimed":   report.BytesReclaimed,
		"warnings":          warnings,
	})
}

// StartRegistryGC runs the weekly registry GC loop: every 24h, when the
// last run (tracked only in memory — a missed run after a restart just
// sweeps promptly, which is harmless) is more than 7 days old or has never
// happened, sweep the registry. Mirrors StartBackups' shape.
func (s *server) StartRegistryGC(ctx context.Context) {
	tick := time.NewTicker(24 * time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if !s.lastRegistryGC.IsZero() && s.nowFn().Sub(s.lastRegistryGC) < 7*24*time.Hour {
				continue
			}
			if _, err := s.runRegistryGC(ctx); err != nil {
				log.Printf("scheduled registry gc: %v", err)
				continue
			}
			s.lastRegistryGC = s.nowFn()
		}
	}
}
