package sweep

import (
	"math/rand"
	"testing"
)

// Integer choices of a million and up must reach the trial env in plain
// decimal form ("1000000", not "1e+06") so int(os.environ[...]) parses them.
func TestGridIntegerChoiceKeepsIntegerForm(t *testing.T) {
	space, err := ParseParams([]byte("steps: [100000, 1000000, 2500000]\n"))
	if err != nil {
		t.Fatal(err)
	}
	sets, _, err := Expand(space, 10, rand.New(rand.NewSource(1)))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"100000", "1000000", "2500000"}
	for i, s := range sets {
		if s["steps"] != want[i] {
			t.Errorf("trial %d steps = %q, want %q", i, s["steps"], want[i])
		}
	}
}

// A huge grid (63 binary params) must truncate to maxTrials instead of
// overflowing the grid-size product and panicking in makeslice.
func TestGridSizeOverflowDoesNotPanic(t *testing.T) {
	space := map[string]Param{}
	for i := 0; i < 63; i++ {
		space[string(rune('a'+i%26))+string(rune('a'+i/26))] = Param{Choices: []string{"0", "1"}}
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Expand panicked: %v", r)
		}
	}()
	sets, truncated, err := Expand(space, 500, rand.New(rand.NewSource(1)))
	if err != nil {
		return
	}
	if len(sets) != 500 || !truncated {
		t.Fatalf("got %d sets truncated=%v, want 500 truncated=true", len(sets), truncated)
	}
}
