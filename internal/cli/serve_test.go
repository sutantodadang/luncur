package cli

import (
	"path/filepath"
	"testing"

	"github.com/sutantodadang/luncur/internal/store"
)

func TestBootstrapAdmin(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")

	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := bootstrapAdmin(st, "root@b.co:hunter2222"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := st.Authenticate("root@b.co", "hunter2222"); err != nil {
		t.Fatalf("admin cannot authenticate: %v", err)
	}
	// Second call is a no-op, not an error (idempotent restarts).
	if err := bootstrapAdmin(st, "other@b.co:whatever123"); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if _, err := st.Authenticate("other@b.co", "whatever123"); err == nil {
		t.Fatal("second admin should not have been created")
	}
	st.Close()
}

func TestBootstrapAdminRejectsBadSpec(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := bootstrapAdmin(st, "no-colon-here"); err == nil {
		t.Fatal("want error for spec without colon")
	}
}

func TestServeSSHFlags(t *testing.T) {
	cmd := serveCmd()
	sshListen, err := cmd.Flags().GetString("ssh-listen")
	if err != nil || sshListen != ":2222" {
		t.Fatalf("ssh-listen default = %q err=%v, want :2222", sshListen, err)
	}
	if _, err := cmd.Flags().GetString("ssh-hostkey-file"); err != nil {
		t.Fatalf("ssh-hostkey-file flag missing: %v", err)
	}
}
