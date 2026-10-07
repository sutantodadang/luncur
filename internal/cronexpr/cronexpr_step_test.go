package cronexpr

import "testing"

// "N/S" means "N-max/S" (Vixie cron, Kubernetes CronJob), so "5/15" in
// the minute field selects 5, 20, 35 and 50.
func TestStartSlashStepMeansRangeToMax(t *testing.T) {
	s, err := Parse("5/15 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []int{5, 20, 35, 50} {
		if !s.Minute[m] {
			t.Errorf("minute %d not selected by 5/15 (got %v)", m, s.Minute)
		}
	}
}
