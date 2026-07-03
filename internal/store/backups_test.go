package store

import (
	"errors"
	"testing"
)

func TestBackupLifecycle(t *testing.T) {
	s := openTest(t)

	b1, err := s.CreateBackup("backups/a.tar.gz", 100, false)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := s.CreateBackup("backups/b.tar.gz", 200, true)
	if err != nil {
		t.Fatal(err)
	}

	list, err := s.ListBackups()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != b2.ID || list[1].ID != b1.ID {
		t.Fatalf("list = %+v, want newest first", list)
	}
	if list[0].Path != "backups/b.tar.gz" || list[0].SizeBytes != 200 || !list[0].Uploaded {
		t.Fatalf("newest backup = %+v", list[0])
	}
	if list[1].Path != "backups/a.tar.gz" || list[1].SizeBytes != 100 || list[1].Uploaded {
		t.Fatalf("oldest backup = %+v", list[1])
	}

	if err := s.DeleteBackup(b1.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBackup(b1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}
