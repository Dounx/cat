package cat

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveBlobAndFiles(t *testing.T) {
	dir := t.TempDir()
	h := &Host{BlobsDir: dir}
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	att, err := h.SaveBlob("../../evil:report?.png", "", bytes.NewReader(png), 0)
	if err != nil {
		t.Fatal(err)
	}
	if att.Kind != "image" || att.MIME != "image/png" || att.Size != int64(len(png)) {
		t.Fatalf("att = %+v", att)
	}
	if strings.Contains(att.Name, "..") || strings.Contains(att.Name, "/") {
		t.Fatalf("unsafe name %q", att.Name)
	}
	if !strings.HasPrefix(att.Path, filepath.Clean(dir)) {
		t.Fatalf("path %q outside blobs dir %q", att.Path, dir)
	}
	if got, err := os.ReadFile(att.Path); err != nil || !bytes.Equal(got, png) {
		t.Fatalf("stored bytes = %v, err = %v", got, err)
	}

	if _, err := h.SaveBlob("big.bin", "", bytes.NewReader([]byte("12345")), 4); err == nil {
		t.Fatal("expected size limit error")
	}

	got := Files(map[string]any{"files": []any{
		att.Map(),
		" /tmp/other.txt ",
		map[string]any{"name": "no-path.txt"},
	}})
	if len(got) != 2 || got[0] != att.Path || got[1] != "/tmp/other.txt" {
		t.Fatalf("Files slice = %v", got)
	}
	gotStr := Files(map[string]any{"files": "/a.png\n\n /b.pdf \n"})
	if len(gotStr) != 2 || gotStr[0] != "/a.png" || gotStr[1] != "/b.pdf" {
		t.Fatalf("Files string = %v", gotStr)
	}
}
