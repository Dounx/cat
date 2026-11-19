package cat

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultMaxBlob = 10 << 20

// Attachment describes a downloaded media item or file kept in Cat's blobs
// directory so rules and downstream actions can read or forward it.
type Attachment struct {
	Kind string `json:"kind"` // image | audio | video | file
	Name string `json:"name"`
	MIME string `json:"mime"`
	Size int64  `json:"size"`
	Path string `json:"path"`
	Rel  string `json:"rel"`
}

// Map converts an Attachment into the payload map placed under
// event.payload.attachments.
func (a Attachment) Map() map[string]any {
	return map[string]any{
		"kind": a.Kind,
		"name": a.Name,
		"mime": a.MIME,
		"type": a.MIME,
		"size": a.Size,
		"path": a.Path,
		"rel":  a.Rel,
	}
}

// SaveBlob reads an attachment from r (up to maxBytes; 10 MiB when <= 0),
// writes it under Host.BlobsDir (or DataDir/blobs), and returns its metadata.
func (h *Host) SaveBlob(name, mimeType string, r io.Reader, maxBytes int64) (Attachment, error) {
	dir := h.BlobsDir
	if dir == "" && h.DataDir != "" {
		dir = filepath.Join(h.DataDir, "blobs")
	}
	if dir == "" {
		return Attachment{}, errors.New("blobs directory is not set")
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxBlob
	}
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return Attachment{}, err
	}
	if int64(len(data)) > maxBytes {
		return Attachment{}, fmt.Errorf("附件超过 %d MB 限制", maxBytes>>20)
	}
	mimeType, _, _ = strings.Cut(strings.ToLower(strings.TrimSpace(mimeType)), ";")
	if mimeType == "" || mimeType == "application/octet-stream" {
		if ext := filepath.Ext(name); ext != "" {
			if byExt := mime.TypeByExtension(ext); byExt != "" {
				mimeType, _, _ = strings.Cut(strings.ToLower(byExt), ";")
			}
		}
	}
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
		mimeType, _, _ = strings.Cut(strings.ToLower(mimeType), ";")
	}
	cleanName := sanitizeBlobName(name, mimeType)
	kind := "file"
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		kind = "image"
	case strings.HasPrefix(mimeType, "audio/"):
		kind = "audio"
	case strings.HasPrefix(mimeType, "video/"):
		kind = "video"
	}
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	day := time.Now().UTC().Format("20060102")
	rel := day + "/" + hex.EncodeToString(rnd[:]) + "-" + cleanName
	abs := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return Attachment{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".blob-*")
	if err != nil {
		return Attachment{}, err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return Attachment{}, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return Attachment{}, err
	}
	_ = os.Chmod(tmpName, 0o600)
	if err := os.Rename(tmpName, abs); err != nil {
		_ = os.Remove(tmpName)
		return Attachment{}, err
	}
	return Attachment{
		Kind: kind,
		Name: cleanName,
		MIME: mimeType,
		Size: int64(len(data)),
		Path: abs,
		Rel:  rel,
	}, nil
}

func sanitizeBlobName(name, mimeType string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		if r < 32 || r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			b.WriteRune('_')
		} else {
			b.WriteRune(r)
		}
	}
	clean := strings.Trim(b.String(), ". ")
	if clean == "" || clean == "." || clean == ".." {
		clean = "attachment"
	}
	if r := []rune(clean); len(r) > 80 {
		ext := filepath.Ext(clean)
		stem := string(r[:max(1, 80-len([]rune(ext)))])
		clean = stem + ext
	}
	if filepath.Ext(clean) == "" && mimeType != "" {
		if exts, _ := mime.ExtensionsByType(mimeType); len(exts) > 0 {
			clean += exts[0]
		}
	}
	return clean
}

// FileRef identifies a local file path and its display filename.
type FileRef struct {
	Path string
	Name string
}

// Files extracts local file paths from action params["files"], accepting either
// newline-separated strings, JSON arrays/objects, attachment objects with "path",
// or slices thereof (for example {{ payload.attachments }}).
func Files(params map[string]any) []string {
	refs := NamedFiles(params)
	if len(refs) == 0 {
		return nil
	}
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.Path
	}
	return out
}

// NamedFiles extracts FileRefs from action params["files"], preserving original
// attachment filenames when available.
func NamedFiles(params map[string]any) []FileRef {
	if params == nil {
		return nil
	}
	return extractFileRefs(params["files"])
}

func extractFileRefs(v any) []FileRef {
	var out []FileRef
	switch t := v.(type) {
	case string:
		trimmed := strings.TrimSpace(t)
		if strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{") {
			var parsed any
			if json.Unmarshal([]byte(trimmed), &parsed) == nil {
				return extractFileRefs(parsed)
			}
		}
		for _, line := range strings.Split(t, "\n") {
			if s := strings.TrimSpace(line); s != "" {
				out = append(out, FileRef{Path: s})
			}
		}
	case []string:
		for _, s := range t {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, FileRef{Path: s})
			}
		}
	case Attachment:
		if s := strings.TrimSpace(t.Path); s != "" {
			out = append(out, FileRef{Path: s, Name: strings.TrimSpace(t.Name)})
		}
	case map[string]any:
		if p, _ := t["path"].(string); strings.TrimSpace(p) != "" {
			n, _ := t["name"].(string)
			out = append(out, FileRef{Path: strings.TrimSpace(p), Name: strings.TrimSpace(n)})
		}
	case []map[string]any:
		for _, item := range t {
			out = append(out, extractFileRefs(item)...)
		}
	case []any:
		for _, item := range t {
			out = append(out, extractFileRefs(item)...)
		}
	}
	return out
}
