package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/sutantodadang/luncur/internal/up"
)

func testDownOpts() downOpts {
	return downOpts{
		RegistriesPath: "/etc/rancher/k3s/registries.yaml",
		K3sUninstall:   "/usr/local/bin/k3s-uninstall.sh",
		BackupPath:     backupPath("/home/tester", time.Unix(1700000000, 0)),
	}
}

func descs(steps []downStep) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Desc
	}
	return out
}

func containsSubstr(all []string, substr string) int {
	for i, d := range all {
		if strings.Contains(d, substr) {
			return i
		}
	}
	return -1
}

func TestBuildDownPlanDefaultOrder(t *testing.T) {
	plan := buildDownPlan(testDownOpts())
	all := descs(plan)

	iBackup := containsSubstr(all, "backup SQLite DB")
	iStop := containsSubstr(all, "stop luncur")
	iNamespaces := containsSubstr(all, "delete luncur-managed namespaces")
	iData := containsSubstr(all, "data volume")
	iRegistries := containsSubstr(all, "registries config")

	if iBackup == -1 || iStop == -1 || iNamespaces == -1 || iData == -1 || iRegistries == -1 {
		t.Fatalf("missing expected step(s), got: %v", all)
	}
	if !(iBackup < iStop && iStop < iNamespaces && iNamespaces < iData && iData < iRegistries) {
		t.Fatalf("steps out of order: %v", all)
	}
	if containsSubstr(all, "uninstall K3s") != -1 {
		t.Fatalf("default plan must not include the k3s-uninstall step, got: %v", all)
	}
}

func TestBuildDownPlanAllAppendsK3sStep(t *testing.T) {
	opts := testDownOpts()
	opts.All = true
	plan := buildDownPlan(opts)
	all := descs(plan)

	iRegistries := containsSubstr(all, "registries config")
	iK3s := containsSubstr(all, "uninstall K3s")
	if iK3s == -1 {
		t.Fatalf("--all plan missing k3s-uninstall step, got: %v", all)
	}
	if iK3s != len(plan)-1 {
		t.Fatalf("k3s-uninstall step must be last, got: %v", all)
	}
	if iRegistries == -1 || iRegistries >= iK3s {
		t.Fatalf("k3s step must come after registries step, got: %v", all)
	}
}

func TestBuildDownPlanNoBackupOmitsBackupStep(t *testing.T) {
	opts := testDownOpts()
	opts.NoBackup = true
	plan := buildDownPlan(opts)
	all := descs(plan)

	if containsSubstr(all, "backup SQLite DB") != -1 {
		t.Fatalf("--no-backup plan must omit the backup step, got: %v", all)
	}
	// everything else still present
	for _, want := range []string{"stop luncur", "delete luncur-managed namespaces", "data volume", "registries config"} {
		if containsSubstr(all, want) == -1 {
			t.Fatalf("missing step %q with --no-backup, got: %v", want, all)
		}
	}
}

func TestBackupPathDeterministic(t *testing.T) {
	now := time.Unix(1700000000, 0)
	got := backupPath("/home/tester", now)
	want := "/home/tester/luncur-final-backup-1700000000.db"
	if got != want && got != strings.ReplaceAll(want, "/", "\\") {
		t.Fatalf("backupPath = %q, want %q", got, want)
	}
}

// TestDownDryRunPrintsPlanAndTouchesNothing exercises the real cobra
// command. It never wires a KubeClient/Runner (RunE only does that after
// the --dry-run branch returns), so if dry-run ever accidentally invoked a
// step's Run, it would nil-pointer-panic on opts.KubeClient — the absence
// of a panic, plus a clean exit, is the proof no side effects occurred.
func TestDownDryRunPrintsPlanAndTouchesNothing(t *testing.T) {
	cmd := downCmd()
	buf := &strings.Builder{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--dry-run"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("dry-run returned error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "dry run") {
		t.Fatalf("expected dry-run banner, got: %s", out)
	}
	for _, want := range []string{"backup SQLite DB", "stop luncur", "delete luncur-managed namespaces", "data volume", "registries config"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "uninstall K3s") {
		t.Fatalf("dry-run without --all must not print the k3s step:\n%s", out)
	}
}

func TestDownDryRunAllPrintsK3sStep(t *testing.T) {
	cmd := downCmd()
	buf := &strings.Builder{}
	cmd.SetOut(buf)
	cmd.SetArgs([]string{"--dry-run", "--all"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("dry-run --all returned error: %v", err)
	}
	if !strings.Contains(buf.String(), "uninstall K3s") {
		t.Fatalf("dry-run --all must print the k3s step:\n%s", buf.String())
	}
}

// TestDownConfirmationAbort feeds a wrong confirmation word on stdin and
// checks the command aborts with a non-nil error before reaching any
// cluster-dependent step. This test only runs on linux, matching down's
// own platform guard (same as `luncur up`).
func TestDownConfirmationAbort(t *testing.T) {
	cmd := downCmd()
	buf := &strings.Builder{}
	cmd.SetOut(buf)
	cmd.SetIn(strings.NewReader("nope\n"))
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected abort error, got nil")
	}
	if !strings.Contains(err.Error(), "confirmation") && !strings.Contains(err.Error(), "linux") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDownConfirmationSummaryMentionsBackupAndK3sTier(t *testing.T) {
	opts := testDownOpts()
	s := downConfirmationSummary(opts)
	if !strings.Contains(s, opts.BackupPath) || !strings.Contains(s, "K3s stays") {
		t.Fatalf("default-tier summary wrong: %q", s)
	}
	opts.All = true
	s = downConfirmationSummary(opts)
	if !strings.Contains(s, "ALL CLUSTER DATA") {
		t.Fatalf("--all summary wrong: %q", s)
	}
}

// sanity: up.RegistriesPath / up.K3sKubeconfig still exist with the shape
// down.go depends on (guards against a future rename in internal/up
// silently breaking down's defaults without a compile error, since down.go
// only references up.RegistriesPath, not up.K3sKubeconfig, at plan-build
// time).
func TestUpConstantsDownDependsOn(t *testing.T) {
	if up.RegistriesPath == "" || up.K3sKubeconfig == "" {
		t.Fatal("internal/up constants down.go depends on are empty")
	}
}
