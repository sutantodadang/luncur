package server

import (
	"fmt"
	"strings"

	"github.com/sutantodadang/luncur/internal/store"
)

// uiTreeApp is one app row in the workspace tree: name, its env-scoped link
// (always the /envs/<env>/apps/<app> form, per Slice 1's disambiguating
// twin — same-named apps in two envs never collide), a status-dot class,
// and whether it's the page currently being viewed.
type uiTreeApp struct {
	Name     string
	URL      string
	DotClass string
	Active   bool
}

// uiTreeEnv is one environment row: its own apps (capped at
// uiTreeMaxAppsPerEnv, with MoreCount/MoreURL covering the rest so a busy
// project can't blow up the sidebar).
type uiTreeEnv struct {
	Name      string
	URL       string
	Preview   bool
	Apps      []uiTreeApp
	MoreCount int
	MoreURL   string
}

// uiTreeProject is one project row: a <details> in the template, Open when
// it's the project the current page belongs to (or there's only one
// project total — see uiTreeData).
type uiTreeProject struct {
	Name string
	URL  string
	Open bool
	Envs []uiTreeEnv
}

// uiTreeMaxAppsPerEnv caps how many app rows an environment renders before
// collapsing the rest into a "+N more" link — the ponytail guard DESIGN.md
// calls for so a project with dozens of apps in one env can't turn the
// sidebar into its own scroll.
const uiTreeMaxAppsPerEnv = 8

// uiTreeDotClass maps a latest-deployment status to the tree's status-dot
// class, reusing the same status buckets as the app page's chipData/
// status-* CSS (live=ok, building/deploying/queued=pulsing build, failed=
// fail, anything else including "never deployed"=muted) rather than
// inventing a parallel mapping.
func uiTreeDotClass(status string) string {
	switch status {
	case "live":
		return "tree-dot-ok"
	case "building", "deploying", "queued", "pending":
		return "tree-dot-build"
	case "failed":
		return "tree-dot-fail"
	default:
		return "tree-dot-muted"
	}
}

// uiTreeData builds the sidebar's workspace tree for u, rooted at the same
// project set visibleProjects already scopes UI listings to (all projects
// for admins, membership-scoped for everyone else — so a member can never
// see, via the tree, a project they don't belong to).
//
// activePath is the current request's URL path (no query string); it
// decides which project renders <details open> and which app row gets the
// active left-border. A project with 0 or 1 total projects always opens
// (nothing to disambiguate), matching every other project page's own
// single-project convenience.
//
// Per project this issues exactly 3 queries (environments, apps, latest
// deployment statuses) — the batch LatestStatusesForProject query keeps
// status resolution to one round trip per project instead of one per app.
func (s *server) uiTreeData(u store.User, activePath string) ([]uiTreeProject, error) {
	projects, err := s.visibleProjects(u)
	if err != nil {
		return nil, err
	}

	out := make([]uiTreeProject, 0, len(projects))
	for _, p := range projects {
		envs, err := s.st.ListEnvironments(p.ID)
		if err != nil {
			return nil, fmt.Errorf("list environments for %s: %w", p.Name, err)
		}
		apps, err := s.st.ListApps(p.ID)
		if err != nil {
			return nil, fmt.Errorf("list apps for %s: %w", p.Name, err)
		}
		statuses, err := s.st.LatestStatusesForProject(p.ID)
		if err != nil {
			return nil, fmt.Errorf("latest statuses for %s: %w", p.Name, err)
		}

		projectURL := "/ui/projects/" + p.Name
		treeEnvs := make([]uiTreeEnv, 0, len(envs))
		envIdxByID := map[int64]int{}
		defaultIdx := -1
		for i, e := range envs {
			treeEnvs = append(treeEnvs, uiTreeEnv{
				Name:    e.Name,
				URL:     projectURL + "/envs/" + e.Name,
				Preview: e.Kind == "preview",
			})
			envIdxByID[e.ID] = i
			if e.IsDefault {
				defaultIdx = i
			}
		}
		// Legacy apps written before environments existed carry
		// EnvironmentID 0, which never matches a real environments row
		// (autoincrement starts at 1) — see App.EnvironmentID's doc
		// comment. Route those into the project's default env row, same
		// as uiEnv's own env-less fallback; synthesize one if the project
		// somehow has no environments row at all yet.
		fallbackIdx := func() int {
			if defaultIdx >= 0 {
				return defaultIdx
			}
			treeEnvs = append(treeEnvs, uiTreeEnv{
				Name: p.DefaultEnv,
				URL:  projectURL + "/envs/" + p.DefaultEnv,
			})
			defaultIdx = len(treeEnvs) - 1
			return defaultIdx
		}

		for _, a := range apps {
			idx, ok := envIdxByID[a.EnvironmentID]
			if !ok {
				idx = fallbackIdx()
			}
			env := treeEnvs[idx]
			if len(env.Apps) >= uiTreeMaxAppsPerEnv {
				treeEnvs[idx].MoreCount++
				treeEnvs[idx].MoreURL = projectURL
				continue
			}
			appURL := env.URL + "/apps/" + a.Name
			treeEnvs[idx].Apps = append(treeEnvs[idx].Apps, uiTreeApp{
				Name:     a.Name,
				URL:      appURL,
				DotClass: uiTreeDotClass(statuses[a.ID]),
				Active:   activePath == appURL || (idx == defaultIdx && activePath == projectURL+"/apps/"+a.Name),
			})
		}

		open := len(projects) <= 1 || activePath == projectURL || strings.HasPrefix(activePath, projectURL+"/")
		out = append(out, uiTreeProject{
			Name: p.Name, URL: projectURL, Open: open, Envs: treeEnvs,
		})
	}
	return out, nil
}
