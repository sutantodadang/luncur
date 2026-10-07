package store

import "testing"

func TestAIUsageAccumulates(t *testing.T) {
	s := openTest(t)
	u, err := s.CreateUser("a@b.co", "pw-123456", "member")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.AddAIUsage(u.ID, "chat", 100, 20); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddAIUsage(0, "notify", 5, 5); err != nil {
		t.Fatal(err)
	}
	n, err := s.AITokensToday()
	if err != nil || n != 250 {
		t.Fatalf("today = %d, %v; want 250", n, err)
	}
	rows, err := s.AIUsage(7)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	for _, r := range rows {
		if r.Workflow == "chat" && (r.UserEmail != "a@b.co" || r.Requests != 2 || r.InputTokens != 200) {
			t.Fatalf("chat row = %+v", r)
		}
	}
}
