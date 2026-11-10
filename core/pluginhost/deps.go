package pluginhost

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// Dependency is an external program a plugin needs, which the host downloads,
// verifies and unpacks on the plugin's behalf.
type Dependency struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
	// Notice is shown to the user when asking for permission to download.
	Notice string `json:"notice,omitempty"`
	// Platforms maps "os/arch" (Go names, e.g. "darwin/arm64"), "os" or "any"
	// to the artifact for that platform.
	Platforms map[string]Artifact `json:"platforms"`
}

// Artifact is one downloadable file. SHA256 is mandatory: the host refuses to
// install anything it cannot verify.
type Artifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	// Format: "zip", "tar.gz" or "file" (saved as-is). Guessed from URL if empty.
	Format string `json:"format,omitempty"`
	// Strip removes this many leading path components when unpacking.
	Strip int `json:"strip,omitempty"`
	// Filename names the saved file when Format is "file".
	Filename string `json:"filename,omitempty"`
}

// ErrNoArtifact means the dependency is not offered for this platform.
var ErrNoArtifact = errors.New("该依赖不支持当前系统")

// Pick selects the artifact for the running platform.
func (d Dependency) Pick() (Artifact, error) {
	for _, key := range []string{runtime.GOOS + "/" + runtime.GOARCH, runtime.GOOS, "any"} {
		if a, ok := d.Platforms[key]; ok {
			return a, nil
		}
	}
	return Artifact{}, fmt.Errorf("%w：%s（%s/%s）", ErrNoArtifact, d.Name, runtime.GOOS, runtime.GOARCH)
}

// Deps installs dependencies under Dir/<plugin>/<name>/<version>.
type Deps struct {
	Dir    string
	Client *http.Client

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (d *Deps) lock(key string) *sync.Mutex {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.locks == nil {
		d.locks = map[string]*sync.Mutex{}
	}
	if d.locks[key] == nil {
		d.locks[key] = &sync.Mutex{}
	}
	return d.locks[key]
}

// Path is where a dependency lives once installed.
func (d *Deps) Path(pluginID string, dep Dependency) string {
	return filepath.Join(d.Dir, pluginID, dep.Name, dep.Version)
}

// Installed reports whether the dependency has been fully installed.
func (d *Deps) Installed(pluginID string, dep Dependency) bool {
	_, err := os.Stat(filepath.Join(d.Path(pluginID, dep), ".installed"))
	return err == nil
}

// Install downloads, verifies and unpacks a dependency. progress, if not nil,
// is called with bytes done and total (total is -1 when unknown).
func (d *Deps) Install(ctx context.Context, pluginID string, dep Dependency, progress func(done, total int64)) (string, error) {
	dest := d.Path(pluginID, dep)
	l := d.lock(dest)
	l.Lock()
	defer l.Unlock()
	if d.Installed(pluginID, dep) {
		return dest, nil
	}
	art, err := dep.Pick()
	if err != nil {
		return "", err
	}
	want := strings.ToLower(strings.TrimSpace(art.SHA256))
	if len(want) != 64 {
		return "", fmt.Errorf("依赖 %s 没有提供有效的 SHA-256 校验值，拒绝安装", dep.Name)
	}
	if !strings.HasPrefix(art.URL, "https://") && !isLoopbackURL(art.URL) {
		return "", fmt.Errorf("依赖 %s 的下载地址不是 HTTPS，拒绝安装", dep.Name)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), "download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	req, err := http.NewRequestWithContext(ctx, "GET", art.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "cat-event-hub")
	client := d.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载 %s 失败：%w", dep.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("下载 %s 失败：HTTP %d", dep.Name, resp.StatusCode)
	}
	sum := sha256.New()
	counter := &countingWriter{total: resp.ContentLength, report: progress}
	if _, err := io.Copy(io.MultiWriter(tmp, sum, counter), resp.Body); err != nil {
		return "", fmt.Errorf("下载 %s 失败：%w", dep.Name, err)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		return "", fmt.Errorf("依赖 %s 校验失败：期望 %s，实际 %s。文件已丢弃", dep.Name, want, got)
	}

	staging := dest + ".partial"
	_ = os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	format := art.Format
	if format == "" {
		switch {
		case strings.HasSuffix(art.URL, ".zip"):
			format = "zip"
		case strings.HasSuffix(art.URL, ".tar.gz"), strings.HasSuffix(art.URL, ".tgz"):
			format = "tar.gz"
		default:
			format = "file"
		}
	}
	switch format {
	case "zip":
		err = unzip(tmp.Name(), staging, art.Strip)
	case "tar.gz":
		err = untar(tmp.Name(), staging, art.Strip)
	case "file":
		name := art.Filename
		if name == "" {
			name = filepath.Base(art.URL)
		}
		err = copyFile(tmp.Name(), filepath.Join(staging, filepath.Base(name)), 0o755)
	default:
		err = fmt.Errorf("unknown artifact format %q", format)
	}
	if err != nil {
		return "", fmt.Errorf("解压 %s 失败：%w", dep.Name, err)
	}
	if err := os.WriteFile(filepath.Join(staging, ".installed"), []byte(want+"\n"), 0o644); err != nil {
		return "", err
	}
	_ = os.RemoveAll(dest)
	if err := os.Rename(staging, dest); err != nil {
		return "", err
	}
	return dest, nil
}

func isLoopbackURL(u string) bool {
	return strings.HasPrefix(u, "http://127.0.0.1:") || strings.HasPrefix(u, "http://localhost:")
}

type countingWriter struct {
	done, total int64
	report      func(done, total int64)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.done += int64(len(p))
	if c.report != nil {
		c.report(c.done, c.total)
	}
	return len(p), nil
}

// target maps an archive entry to a path under dest, rejecting anything that
// would escape it. It returns "" for entries removed entirely by strip.
func target(dest, name string, strip int) (string, error) {
	parts := strings.Split(filepath.ToSlash(filepath.Clean("/" + name))[1:], "/")
	if len(parts) <= strip {
		return "", nil
	}
	rel := filepath.Join(parts[strip:]...)
	full := filepath.Join(dest, rel)
	if full != dest && !strings.HasPrefix(full, dest+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive entry escapes destination: %s", name)
	}
	return full, nil
}

func unzip(src, dest string, strip int) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		path, err := target(dest, f.Name, strip)
		if err != nil {
			return err
		}
		if path == "" || path == dest {
			continue
		}
		mode := f.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
		case mode&os.ModeSymlink != 0:
			continue // never materialise links from an archive
		default:
			rc, err := f.Open()
			if err != nil {
				return err
			}
			err = writeFile(path, rc, mode.Perm())
			rc.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func untar(src, dest string, strip int) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		path, err := target(dest, hdr.Name, strip)
		if err != nil {
			return err
		}
		if path == "" || path == dest {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeFile(path, tr, os.FileMode(hdr.Mode).Perm()); err != nil {
				return err
			}
		}
	}
}

func writeFile(path string, r io.Reader, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if perm == 0 {
		perm = 0o644
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm|0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeFile(dst, in, perm)
}
