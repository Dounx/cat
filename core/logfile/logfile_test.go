package logfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAppendsAcrossOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "cat.log")
	for _, line := range []string{"one\n", "two\n"} {
		w, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
		w.Close()
	}
	if got := read(t, path); got != "one\ntwo\n" {
		t.Fatalf("got %q", got)
	}
}

func TestRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cat.log")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.MaxSize, w.Keep = 10, 2
	// Each line fills the file, so every write after the first rotates.
	for _, line := range []string{"aaaaaaaa\n", "bbbbbbbb\n", "cccccccc\n", "dddddddd\n"} {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	for file, want := range map[string]string{path: "d", path + ".1": "c", path + ".2": "b"} {
		if got := read(t, file); !strings.HasPrefix(got, want) {
			t.Errorf("%s: got %q, want it to start with %q", filepath.Base(file), got, want)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Errorf("cat.log.3 should not be kept: %v", err)
	}
}
