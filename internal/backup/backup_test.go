package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-octo/octo-agent/internal/datahome"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExportPreviewRestore(t *testing.T) {
	src := t.TempDir()
	t.Setenv(datahome.PortableRootEnv, src)
	t.Setenv(datahome.ProfileEnv, "")
	dataDir, err := datahome.Dir()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dataDir, "User", "config.yml"), "endpoints: []\n")
	writeFile(t, filepath.Join(dataDir, "Records", "sessions", "a.jsonl"), `{"type":"meta"}`)
	writeFile(t, filepath.Join(dataDir, "Tools", "skills", "demo.md"), "# demo\n")

	res, err := Export(ExportOptions{Sections: []string{"user", "records", "tools"}, Password: "correct horse", App: "test"})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if !strings.HasSuffix(res.Name, Ext) || res.Files != 3 {
		t.Fatalf("unexpected result: %+v", res)
	}
	head := make([]byte, len(Magic))
	f, err := os.Open(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Read(head); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if string(head) != Magic {
		t.Fatalf("container magic = %q", head)
	}

	if _, _, err := ReadPreview(res.Path, "wrong password"); err == nil {
		t.Fatal("wrong password must fail")
	}
	sum, zipPath, err := ReadPreview(res.Path, "correct horse")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	os.Remove(zipPath)
	if len(sum.Restorable) != 3 || sum.Files != 3 {
		t.Fatalf("summary = %+v", sum)
	}

	// Restore into a second data root: import replaces the folder outright.
	dst := t.TempDir()
	t.Setenv(datahome.PortableRootEnv, dst)
	dstData, err := datahome.Dir()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dstData, "Records", "old.jsonl"), "old")
	res2, err := Apply(res.Path, "correct horse", []string{"records"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res2.Sections) != 1 || res2.Files != 1 {
		t.Fatalf("apply result = %+v", res2)
	}
	if _, err := os.Stat(filepath.Join(dstData, "Records", "sessions", "a.jsonl")); err != nil {
		t.Fatalf("restored file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dstData, "Records", "old.jsonl")); err == nil {
		t.Fatal("old file should have been deleted by the replace")
	}
	// Sections that were not requested stay untouched.
	if _, err := os.Stat(filepath.Join(dstData, "User", "config.yml")); err == nil {
		t.Fatal("unrequested section should not be restored")
	}

	// An empty region is a valid target: the package may carry no Work files,
	// and the restore must still create Work instead of failing on a missing
	// staging folder.
	emptyPkg, err := Export(ExportOptions{Sections: []string{"work"}, Password: "correct horse", App: "test"})
	if err != nil {
		t.Fatalf("export empty work: %v", err)
	}
	if _, err := Apply(emptyPkg.Path, "correct horse", []string{"work"}); err != nil {
		t.Fatalf("apply empty work: %v", err)
	}
	if st, err := os.Stat(filepath.Join(dstData, "Work")); err != nil || !st.IsDir() {
		t.Fatalf("empty Work not restored: %v", err)
	}
}
