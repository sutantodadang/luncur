package store

import (
	"path/filepath"
	"testing"
	"time"
)

func uptimeStore(t *testing.T) (*Store, App, Project) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p, _ := st.CreateProject("shop")
	st.SeedProjectEnvironments(p.ID)
	p, _ = st.GetProjectByID(p.ID)
	env, _ := st.GetEnvironment(p.ID, p.DefaultEnv)
	a, _ := st.CreateAppInEnv(env.ID, "web", 8080, "web", "")
	return st, a, p
}

func TestP95(t *testing.T) {
	for _, c := range []struct {
		in   []int
		want int
	}{{nil, 0}, {[]int{7}, 7}, {[]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 100}, 19}, {[]int{5, 1}, 5}} {
		if got := p95(c.in); got != c.want {
			t.Errorf("p95(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestUptimeSamplesStatsAndDays(t *testing.T) {
	st, a, _ := uptimeStore(t)
	now := time.Now().UTC()
	for i := 0; i < 10; i++ {
		ok := i != 3
		if err := st.AddUptimeSample(a.ID, UptimeSample{At: now.Add(-time.Duration(10-i) * time.Minute), OK: ok, LatencyMS: 10 + i, Code: 200}); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := st.GetUptimeStats(a.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pct24h != 90 || stats.Pct30d != 90 || stats.P95MS != 19 || len(stats.Recent) == 0 {
		t.Fatalf("stats = %+v", stats)
	}
	days, err := st.UptimeDays(a.ID, 90, now)
	if err != nil || len(days) != 90 {
		t.Fatalf("days = %d %v", len(days), err)
	}
	today := days[89]
	if today.Total != 10 || today.Pct != 90 || !today.Outage || !days[0].NoData {
		t.Fatalf("today = %+v first = %+v", today, days[0])
	}
	if err := st.RollupUptimeP95(now); err != nil {
		t.Fatal(err)
	}
	days, _ = st.UptimeDays(a.ID, 1, now)
	if days[0].P95MS != 19 {
		t.Fatalf("p95 roll-up = %d", days[0].P95MS)
	}
}

func TestIncidentsAndStatusPages(t *testing.T) {
	st, a, p := uptimeStore(t)
	in, err := st.OpenIncident(p.ID, a.ID, "web is down", true)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := st.OpenAutoIncident(a.ID); err != nil || got.ID != in.ID || got.App != "web" {
		t.Fatalf("open auto = %+v %v", got, err)
	}
	st.AddIncidentUpdate(in.ID, "investigating")
	if err := st.ResolveIncident(in.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.ResolveIncident(in.ID); err == nil {
		t.Fatal("double resolve accepted")
	}
	list, _ := st.ListIncidents(p.ID, 10)
	if len(list) != 1 || list[0].Status != "resolved" || len(list[0].Updates) != 1 {
		t.Fatalf("incidents = %+v", list)
	}

	if err := st.SetStatusPage(StatusPage{ProjectID: p.ID, Slug: "Bad Slug"}); err == nil {
		t.Fatal("bad slug accepted")
	}
	if err := st.SetStatusPage(StatusPage{ProjectID: p.ID, Slug: "acme", Title: "Acme", Apps: []string{"web"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	other, _ := st.CreateProject("other")
	if err := st.SetStatusPage(StatusPage{ProjectID: other.ID, Slug: "acme"}); err != ErrSlugTaken {
		t.Fatalf("taken slug = %v", err)
	}
	sp, err := st.GetStatusPageBySlug("acme")
	if err != nil || sp.Title != "Acme" || len(sp.Apps) != 1 {
		t.Fatalf("page = %+v %v", sp, err)
	}
}
