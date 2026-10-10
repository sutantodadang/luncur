package store

import (
	"path/filepath"
	"testing"
)

func TestAppPolicyDefaultsUpsertCopy(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p, _ := st.CreateProject("shop")
	if err := st.SeedProjectEnvironments(p.ID); err != nil {
		t.Fatal(err)
	}
	p, _ = st.GetProjectByID(p.ID)
	env, _ := st.GetEnvironment(p.ID, p.DefaultEnv)
	a, _ := st.CreateAppInEnv(env.ID, "web", 8080, "web", "")
	b, _ := st.CreateAppInEnv(env.ID, "web2", 8080, "web", "")

	got, err := st.GetAppPolicy(a.ID)
	if err != nil || got.Strategy != "rolling" || !got.AutoRollback || len(got.CanarySteps) != 2 {
		t.Fatalf("default = %+v %v", got, err)
	}
	got.Strategy, got.CanarySteps, got.AutoRollback = "canary", []int{5, 20, 70}, false
	if err := st.SetAppPolicy(got); err != nil {
		t.Fatal(err)
	}
	got.CanarySteps = []int{50, 10}
	if err := st.SetAppPolicy(got); err == nil {
		t.Fatal("decreasing steps accepted")
	}
	if err := st.CopyAppPolicy(a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	cp, _ := st.GetAppPolicy(b.ID)
	if cp.Strategy != "canary" || cp.AutoRollback || FormatCanarySteps(cp.CanarySteps) != "5,20,70" {
		t.Fatalf("copied = %+v", cp)
	}

	d, _ := st.CreateDeployment(a.ID, "deploying", "x:1", 0)
	st.SetDeployProgress(d.ID, 1, 3)
	st.SetDeployFailReason(d.ID, "crash-looping: Error")
	got2, _ := st.GetDeployment(d.ID)
	o, _ := st.GetDeployOutcome(d.ID)
	if got2.FailReason != "crash-looping: Error" || o.Ready != 1 || o.Want != 3 {
		t.Fatalf("outcome = %+v / %+v", got2, o)
	}
}

func TestParseCanarySteps(t *testing.T) {
	for in, ok := range map[string]bool{"10,50": true, "10%, 50%": true, "": false, "0": false, "100": false, "50,50": false, "1,2,3,4,5,6": false} {
		if _, err := ParseCanarySteps(in); (err == nil) != ok {
			t.Errorf("%q: err=%v", in, err)
		}
	}
}
