package pluginhost

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func zipOf(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	return buf.Bytes()
}

func tgzOf(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func serve(t *testing.T, files map[string][]byte) (*httptest.Server, *int) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		b, ok := files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func dep(url, sha string, strip int) Dependency {
	return Dependency{Name: "tool", Version: "1.0", Platforms: map[string]Artifact{"any": {URL: url, SHA256: sha, Strip: strip}}}
}

func TestInstallZipAndTarGz(t *testing.T) {
	z := zipOf(t, map[string]string{"pkg/bin/run": "#!/bin/sh\n", "pkg/readme.txt": "hi"})
	g := tgzOf(t, map[string]string{"bin/run": "x"})
	srv, hits := serve(t, map[string][]byte{"a.zip": z, "b.tar.gz": g})
	d := &Deps{Dir: t.TempDir()}

	var last int64
	path, err := d.Install(context.Background(), "p", dep(srv.URL+"/a.zip", sum(z), 1), func(done, total int64) { last = done })
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(path, "readme.txt")); string(b) != "hi" {
		t.Fatalf("strip=1 should drop the top folder; got %q", b)
	}
	if last != int64(len(z)) {
		t.Errorf("progress reported %d of %d bytes", last, len(z))
	}
	// A second install is a no-op: nothing is downloaded again.
	if _, err := d.Install(context.Background(), "p", dep(srv.URL+"/a.zip", sum(z), 1), nil); err != nil || *hits != 1 {
		t.Fatalf("reinstall: err=%v hits=%d", err, *hits)
	}
	tgz := dep(srv.URL+"/b.tar.gz", sum(g), 0)
	tgz.Name = "other"
	path, err = d.Install(context.Background(), "p", tgz, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(path, "bin", "run")); err != nil || st.Mode()&0o100 == 0 {
		t.Fatalf("tar.gz entry missing or not executable: %v", err)
	}
}

func TestInstallRefusesWhatItCannotVerify(t *testing.T) {
	z := zipOf(t, map[string]string{"f": "x"})
	srv, hits := serve(t, map[string][]byte{"a.zip": z})
	d := &Deps{Dir: t.TempDir()}
	ctx := context.Background()

	wrong := dep(srv.URL+"/a.zip", strings.Repeat("0", 64), 0)
	if _, err := d.Install(ctx, "p", wrong, nil); err == nil || !strings.Contains(err.Error(), "校验失败") {
		t.Fatalf("checksum mismatch: %v", err)
	}
	if d.Installed("p", wrong) {
		t.Fatal("a file that failed verification must not be installed")
	}
	if entries, _ := os.ReadDir(filepath.Join(d.Dir, "p", "tool")); len(entries) != 0 {
		t.Fatalf("leftovers after a failed install: %v", entries)
	}

	before := *hits
	if _, err := d.Install(ctx, "p", dep(srv.URL+"/a.zip", "", 0), nil); err == nil || *hits != before {
		t.Fatalf("missing checksum must be refused before downloading: err=%v", err)
	}
	if _, err := d.Install(ctx, "p", dep("http://example.com/a.zip", sum(z), 0), nil); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("plain http must be refused: %v", err)
	}
	none := Dependency{Name: "tool", Version: "1", Platforms: map[string]Artifact{"plan9/mips": {}}}
	if _, err := d.Install(ctx, "p", none, nil); err == nil {
		t.Fatal("unsupported platform should fail")
	}
}

func TestInstallRejectsPathTraversal(t *testing.T) {
	for _, name := range []string{"../../escape.txt", "/abs/escape.txt", "ok/../../escape.txt"} {
		z := zipOf(t, map[string]string{name: "x", "fine.txt": "y"})
		srv, _ := serve(t, map[string][]byte{"a.zip": z})
		root := t.TempDir()
		d := &Deps{Dir: filepath.Join(root, "deps")}
		path, err := d.Install(context.Background(), "p", dep(srv.URL+"/a.zip", sum(z), 0), nil)
		// Either the entry is neutralised inside the destination, or the install fails;
		// in no case may anything be written outside it.
		var outside []string
		filepath.Walk(root, func(p string, info os.FileInfo, _ error) error {
			if info != nil && !info.IsDir() && strings.HasSuffix(p, "escape.txt") && (path == "" || !strings.HasPrefix(p, path)) {
				outside = append(outside, p)
			}
			return nil
		})
		if len(outside) > 0 {
			t.Fatalf("%q escaped the destination: %v (err=%v)", name, outside, err)
		}
	}
}
