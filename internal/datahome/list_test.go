package datahome

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListReportsTheRootsThatExist(t *testing.T) {
	root := t.TempDir()
	t.Setenv(PortableRootEnv, root)
	for _, name := range []string{"Data", "Data-work", "Data-lab", "Data-", "Data-bad name", "Database"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A file that looks like a root is not one.
	if err := os.WriteFile(filepath.Join(root, "Data-file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := List()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"", "lab", "work"}
	if len(got) != len(want) {
		t.Fatalf("List() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("List() = %#v, want %#v", got, want)
		}
	}
}

func TestListOmitsTheDefaultRootUntilItExists(t *testing.T) {
	root := t.TempDir()
	t.Setenv(PortableRootEnv, root)
	if err := os.MkdirAll(filepath.Join(root, "Data-work"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "work" {
		t.Errorf("List() = %#v, want just [work]", got)
	}
}

func TestSharedPathIgnoresTheProfile(t *testing.T) {
	root := t.TempDir()
	t.Setenv(PortableRootEnv, root)
	t.Setenv(ProfileEnv, "work")
	got, err := SharedPath("desktop-profile")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "Data", "desktop-profile"); got != want {
		t.Errorf("SharedPath = %q, want %q", got, want)
	}
}
