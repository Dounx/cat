package blob

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveAndSweep(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "20261118")
	if err := os.MkdirAll(day, 0o700); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(day, "kept.png")
	old := filepath.Join(day, "old.png")
	fresh := filepath.Join(day, "fresh.png")
	for _, p := range []string{kept, old, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(kept, past, past)
	_ = os.Chtimes(old, past, past)

	got, err := Resolve(root, "20261118/kept.png")
	if err != nil {
		t.Fatal(err)
	}
	realKept, _ := filepath.EvalSymlinks(kept)
	if got != realKept {
		t.Fatalf("Resolve = %q, want %q", got, realKept)
	}
	for _, bad := range []string{"", "../secret", "20261118/../../etc/passwd", "/etc/passwd", "20261118/missing.png"} {
		if _, err := Resolve(root, bad); err == nil {
			t.Errorf("Resolve(%q) succeeded unexpectedly", bad)
		}
	}

	Sweep(root, map[string]bool{kept: true}, time.Hour)
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("kept file removed: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh file within grace removed: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old unreferenced file still present: err=%v", err)
	}
}
