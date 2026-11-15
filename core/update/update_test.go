package update

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/dounx/cat/core/pluginhost"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.2.0", "0.1.0", true},
		{"v0.2.0", "0.2.0", false},
		{"0.10.0", "0.9.9", true},
		{"1.0", "1.0.0", false},
		{"1.0.1", "1.0", true},
		{"0.1.0", "0.2.0", false},
		{"0.2.0-rc1", "0.1.0", true},
		{"latest", "0.1.0", false},
		{"0.1.0", "dev", true},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestLayout(t *testing.T) {
	root, rel := layout(filepath.FromSlash("/Applications/Cat.app/Contents/MacOS/Cat"))
	if root != filepath.FromSlash("/Applications/Cat.app") || rel != filepath.FromSlash("Contents/MacOS/Cat") {
		t.Errorf("bundle: %q %q", root, rel)
	}
	root, rel = layout(filepath.FromSlash("/opt/cat/catd"))
	if root != filepath.FromSlash("/opt/cat") || rel != "catd" {
		t.Errorf("directory: %q %q", root, rel)
	}
}

// pack zips files under one top-level directory, as a release archive is.
func pack(t *testing.T, top string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for name, body := range files {
		h := &zip.FileHeader{Name: top + "/" + name, Method: zip.Deflate}
		h.SetMode(0o755)
		w, err := z.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// installed is an updater for version 0.1.0 of an app unpacked in a
// temporary directory, reading its manifest from the test server.
func installed(t *testing.T, files map[string]string, manifest func(base string) Manifest, archive []byte) (*Updater, string) {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/update.json":
			json.NewEncoder(w).Encode(manifest(srv.URL))
		case "/app.zip":
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	u := New(srv.URL+"/update.json", "0.1.0", Catd, t.TempDir())
	if u.Status().State != Current {
		t.Fatalf("a versioned build starts %q", u.Status().State)
	}
	// As if the test binary were the app's catd.
	u.Exe, u.root, u.rel = filepath.Join(root, "catd"), root, "catd"
	return u, root
}

func release(version string, archive []byte) func(base string) Manifest {
	sum := sha256.Sum256(archive)
	return func(base string) Manifest {
		return Manifest{Version: version, Notes: "https://example.com/notes", Packages: map[string]map[string]pluginhost.Artifact{
			Catd: {runtime.GOOS + "/" + runtime.GOARCH: {URL: base + "/app.zip", SHA256: hex.EncodeToString(sum[:])}},
		}}
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDownloadAndInstall(t *testing.T) {
	archive := pack(t, "cat-v0.2.0", map[string]string{
		"catd":                       "new catd",
		"plugins/rss/plugin.json":    "new manifest",
		"plugins/rss/cat-plugin-rss": "new plugin",
		"plugins/fresh/plugin.json":  "added in 0.2.0",
	})
	u, root := installed(t, map[string]string{
		"catd":                       "old catd",
		"plugins/rss/plugin.json":    "old manifest",
		"plugins/rss/cat-plugin-rss": "old plugin",
		"plugins/mine/plugin.json":   "put here by the user",
		"notes.txt":                  "not the app's",
	}, release("0.2.0", archive), archive)
	var seen []string
	u.Changed = func(s Status) {
		if len(seen) == 0 || seen[len(seen)-1] != s.State {
			seen = append(seen, s.State)
		}
	}
	restarted := make(chan struct{})
	u.Restart = func() { close(restarted) }

	if err := u.Apply(); err == nil {
		t.Fatal("installed before anything was downloaded")
	}
	s := u.Check(context.Background())
	if s.State != Ready || s.Latest != "0.2.0" || s.Notes == "" {
		t.Fatalf("after check: %+v", s)
	}
	if got := read(t, filepath.Join(root, "catd")); got != "old catd" {
		t.Fatalf("a download alone changed the app: %q", got)
	}
	// Asking again does not download again.
	if s := u.Check(context.Background()); s.State != Ready {
		t.Fatalf("second check: %+v", s)
	}
	if err := u.Apply(); err != nil {
		t.Fatal(err)
	}
	<-restarted
	for name, want := range map[string]string{
		"catd":                       "new catd",
		"plugins/rss/plugin.json":    "new manifest",
		"plugins/rss/cat-plugin-rss": "new plugin",
		"plugins/fresh/plugin.json":  "added in 0.2.0",
		"plugins/mine/plugin.json":   "put here by the user",
		"notes.txt":                  "not the app's",
	} {
		if got := read(t, filepath.Join(root, filepath.FromSlash(name))); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(filepath.Join(root, "catd")); st.Mode().Perm()&0o100 == 0 {
			t.Errorf("the new catd is not executable: %v", st.Mode())
		}
	}
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if ext := filepath.Ext(path); ext == newSuffix || ext == oldSuffix {
			t.Errorf("left behind: %s", path)
		}
		return nil
	})
	if got, want := seen, []string{Downloading, Ready, Restarting}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("states told: %v, want %v", got, want)
	}
	if s := u.Check(context.Background()); s.State != Restarting {
		t.Errorf("a check while restarting: %+v", s)
	}
}

func TestNothingNewer(t *testing.T) {
	archive := pack(t, "cat", map[string]string{"catd": "same"})
	u, root := installed(t, map[string]string{"catd": "old catd"}, release("0.1.0", archive), archive)
	if s := u.Check(context.Background()); s.State != Current || s.Latest != "" || s.CheckedAt == "" {
		t.Fatalf("%+v", s)
	}
	if got := read(t, filepath.Join(root, "catd")); got != "old catd" {
		t.Fatal(got)
	}
}

func TestRefusesWhatItCannotVerify(t *testing.T) {
	archive := pack(t, "cat", map[string]string{"catd": "new catd"})
	bad := func(base string) Manifest {
		m := release("0.2.0", archive)(base)
		m.Packages[Catd][runtime.GOOS+"/"+runtime.GOARCH] = pluginhost.Artifact{URL: base + "/app.zip", SHA256: hex.EncodeToString(make([]byte, 32))}
		return m
	}
	u, _ := installed(t, map[string]string{"catd": "old catd"}, bad, archive)
	if s := u.Check(context.Background()); s.State != Failed || s.Reason == "" {
		t.Fatalf("%+v", s)
	}
	if err := u.Apply(); err == nil {
		t.Fatal("installed an archive with the wrong checksum")
	}
}

func TestRefusesAnotherKindOfPackage(t *testing.T) {
	// The desktop app's archive, offered to catd.
	archive := pack(t, "Cat", map[string]string{"cat-desktop": "desktop"})
	u, root := installed(t, map[string]string{"catd": "old catd"}, release("0.2.0", archive), archive)
	if s := u.Check(context.Background()); s.State != Failed {
		t.Fatalf("%+v", s)
	}
	if _, err := os.Stat(filepath.Join(root, "cat-desktop")); err == nil {
		t.Fatal("the wrong package was installed")
	}
}

func TestNewVersionThatCannotBeInstalledHere(t *testing.T) {
	archive := pack(t, "cat", map[string]string{"catd": "new catd"})
	none := func(base string) Manifest {
		m := release("0.2.0", archive)(base)
		m.Packages = map[string]map[string]pluginhost.Artifact{Catd: {"plan9/386": {}}}
		return m
	}
	u, _ := installed(t, map[string]string{"catd": "old catd"}, none, archive)
	if s := u.Check(context.Background()); s.State != Manual || s.Latest != "0.2.0" || s.Reason == "" {
		t.Fatalf("no package for the platform: %+v", s)
	}

	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		return // no read-only directory to try
	}
	u, root := installed(t, map[string]string{"catd": "old catd"}, release("0.2.0", archive), archive)
	os.Chmod(root, 0o555)
	t.Cleanup(func() { os.Chmod(root, 0o755) })
	if s := u.Check(context.Background()); s.State != Manual || s.Reason == "" {
		t.Fatalf("installed where it cannot write: %+v", s)
	}
}

func TestDevBuildNeverUpdates(t *testing.T) {
	for _, u := range []*Updater{
		New("", Dev, Catd, t.TempDir()),
		New("", "", Catd, t.TempDir()),
		New("off", "0.1.0", Catd, t.TempDir()),
	} {
		if s := u.Check(context.Background()); s.State != Off {
			t.Errorf("%+v", s)
		}
		if err := u.Apply(); err == nil {
			t.Error("a build that does not update installed one")
		}
	}
}
