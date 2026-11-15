// Package update keeps the app itself current: it reads a manifest that names
// the latest version and, per kind of package and platform, an archive with
// its SHA-256; downloads and unpacks a newer one in the background; and, when
// told to, puts its files in place of the running ones and has the app start
// again.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dounx/cat/core/pluginhost"
)

// DefaultURL is the manifest of the latest release.
const DefaultURL = "https://github.com/Dounx/cat/releases/latest/download/update.json"

// Dev is the version of a build that was not given one. Such a build is run
// from a source tree, and never updates itself.
const Dev = "dev"

// The kinds of package: the desktop app, and catd with the plugins.
const (
	Desktop = "desktop"
	Catd    = "catd"
)

// Manifest is update.json, written when a release is published.
type Manifest struct {
	Version string `json:"version"`
	// Notes is the page that says what changed.
	Notes string `json:"notes,omitempty"`
	// Packages maps a kind, then "os/arch", to the archive for it. The
	// archive holds one directory, laid out as the installed app is.
	Packages map[string]map[string]pluginhost.Artifact `json:"packages"`
}

// What an updater is doing.
const (
	Off         = "off"         // this build does not update
	Current     = "current"     // nothing newer is known
	Downloading = "downloading" // fetching Latest
	Ready       = "ready"       // Latest is unpacked: Apply installs it
	Manual      = "manual"      // Latest exists, and cannot be installed from here
	Failed      = "failed"      // the last check went wrong
	Restarting  = "restarting"  // Latest is in place, and the app is starting again
)

type Status struct {
	Version string `json:"version"` // what is running
	State   string `json:"state"`
	Latest  string `json:"latest,omitempty"`
	Notes   string `json:"notes,omitempty"`
	// Reason says why, for Manual and Failed.
	Reason string `json:"reason,omitempty"`
	// Done and Total are the bytes of a download; Total is -1 when unknown.
	Done      int64  `json:"done,omitempty"`
	Total     int64  `json:"total,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
}

type Updater struct {
	// Exe is the running executable, as it was found at start: once its file
	// has been replaced the system no longer tells.
	Exe    string
	Client *http.Client
	// Changed is told every new status.
	Changed func(Status)
	// Restart starts the app again from Exe. Without it an installed update
	// waits for the next start.
	Restart func()

	url, kind, dir string
	root, rel      string

	busy   sync.Mutex // one check or install at a time
	mu     sync.Mutex
	status Status
	staged string // the unpacked package, when Ready
}

// New makes the updater of a build of the given version and kind, reading the
// manifest at url ("" for DefaultURL, "off" for none) and keeping downloads
// in dir.
func New(url, version, kind, dir string) *Updater {
	if url == "" {
		url = DefaultURL
	}
	if version == "" {
		version = Dev
	}
	u := &Updater{url: url, kind: kind, dir: dir, status: Status{Version: version, State: Off}}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil || version == Dev || url == "off" {
		return u
	}
	u.Exe = exe
	u.root, u.rel = layout(exe)
	u.status.State = Current
	u.sweep()
	return u
}

func (u *Updater) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.status
}

func (u *Updater) set(fn func(s *Status)) Status {
	u.mu.Lock()
	fn(&u.status)
	s := u.status
	u.mu.Unlock()
	if u.Changed != nil {
		u.Changed(s)
	}
	return s
}

// Run checks shortly after start, then a few times a day, until ctx is done.
func (u *Updater) Run(ctx context.Context) {
	if u.Status().State == Off {
		return
	}
	wait := 10 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		u.Check(ctx)
		wait = 6 * time.Hour
	}
}

// Check reads the manifest and, when it names a newer version that can be
// installed here, downloads and unpacks it.
func (u *Updater) Check(ctx context.Context) Status {
	u.busy.Lock()
	defer u.busy.Unlock()
	was := u.Status()
	if was.State == Off || was.State == Restarting {
		return was
	}
	m, err := u.fetch(ctx)
	if err != nil {
		// What is already unpacked stays ready to install.
		if was.State == Ready {
			return was
		}
		return u.set(func(s *Status) { s.State, s.Reason = Failed, err.Error() })
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if !Newer(m.Version, was.Version) {
		_ = os.RemoveAll(filepath.Join(u.dir, "app"))
		return u.set(func(s *Status) { *s = Status{Version: s.Version, State: Current, CheckedAt: now} })
	}
	found := Status{Version: was.Version, Latest: m.Version, Notes: m.Notes, CheckedAt: now}
	if was.State == Ready && was.Latest == m.Version {
		return was
	}
	art, ok := m.Packages[u.kind][runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		found.State, found.Reason = Manual, "这一版没有给这个系统的安装包"
		return u.set(func(s *Status) { *s = found })
	}
	if !writable(u.root) {
		found.State, found.Reason = Manual, "Cat 装在 "+u.root+"，这里改不了它的文件"
		return u.set(func(s *Status) { *s = found })
	}
	found.State = Downloading
	u.set(func(s *Status) { *s = found })
	staged, err := u.download(ctx, m.Version, art)
	if err != nil {
		return u.set(func(s *Status) { s.State, s.Reason, s.Done, s.Total = Failed, err.Error(), 0, 0 })
	}
	u.mu.Lock()
	u.staged = staged
	u.mu.Unlock()
	return u.set(func(s *Status) { s.State, s.Done, s.Total = Ready, 0, 0 })
}

func (u *Updater) fetch(ctx context.Context) (*Manifest, error) {
	var raw []byte
	if path, ok := strings.CutPrefix(u.url, "file://"); ok {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		raw = b
	} else {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "GET", u.url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "cat-event-hub")
		client := u.Client
		if client == nil {
			client = http.DefaultClient
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("无法检查更新：%w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("无法检查更新：HTTP %d", resp.StatusCode)
		}
		if raw, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20)); err != nil {
			return nil, err
		}
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("更新信息格式错误：%w", err)
	}
	if _, ok := parse(m.Version); !ok {
		return nil, fmt.Errorf("更新信息里的版本号 %q 读不懂", m.Version)
	}
	return &m, nil
}

// download fetches, verifies and unpacks the archive, and returns the
// directory in it that is laid out as the installed app is.
func (u *Updater) download(ctx context.Context, version string, art pluginhost.Artifact) (string, error) {
	deps := &pluginhost.Deps{Dir: u.dir, Client: u.Client}
	dep := pluginhost.Dependency{Name: u.kind, Version: version, Platforms: map[string]pluginhost.Artifact{"any": art}}
	if !deps.Installed("app", dep) {
		// Whatever an earlier version left behind.
		_ = os.RemoveAll(filepath.Join(u.dir, "app"))
	}
	var last time.Time
	dest, err := deps.Install(ctx, "app", dep, func(done, total int64) {
		if time.Since(last) < 200*time.Millisecond && done != total {
			return
		}
		last = time.Now()
		u.set(func(s *Status) { s.Done, s.Total = done, total })
	})
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if st, err := os.Stat(filepath.Join(dest, e.Name(), u.rel)); err == nil && st.Mode().IsRegular() {
			return filepath.Join(dest, e.Name()), nil
		}
	}
	_ = os.RemoveAll(dest)
	return "", fmt.Errorf("下载的安装包里没有 %s，和装在这里的不是同一种", filepath.ToSlash(u.rel))
}

// Apply puts the downloaded version's files in place of the installed ones,
// then has the app start again.
func (u *Updater) Apply() error {
	u.busy.Lock()
	defer u.busy.Unlock()
	u.mu.Lock()
	state, staged := u.status.State, u.staged
	u.mu.Unlock()
	if state != Ready {
		return errors.New("没有下载好的更新")
	}
	left, err := merge(staged, u.root)
	if len(left) > 0 {
		_ = os.WriteFile(u.leftovers(), []byte(strings.Join(left, "\n")+"\n"), 0o600)
	}
	if err != nil {
		return fmt.Errorf("安装更新失败：%w", err)
	}
	seal(u.root)
	u.set(func(s *Status) { s.State = Restarting })
	if u.Restart != nil {
		// After the caller has been answered.
		go func() {
			time.Sleep(300 * time.Millisecond)
			u.Restart()
		}()
	}
	return nil
}

func (u *Updater) leftovers() string { return filepath.Join(u.dir, "leftovers") }

// sweep removes the files an update set aside and could not remove while
// they were in use (a running executable, on Windows).
func (u *Updater) sweep() {
	raw, err := os.ReadFile(u.leftovers())
	if err != nil {
		return
	}
	for _, path := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.HasSuffix(path, oldSuffix) {
			_ = os.Remove(path)
		}
	}
	_ = os.Remove(u.leftovers())
}

// seal signs a macOS bundle again, ad hoc as the release was. The signature
// of what is not a program in it (a plugin's manifest) is kept in extended
// attributes, which an archive unpacked here does not carry: without this the
// bundle is left with a broken seal. It still runs when this fails.
func seal(root string) {
	if runtime.GOOS != "darwin" || !strings.HasSuffix(root, ".app") {
		return
	}
	if out, err := exec.Command("codesign", "--force", "--deep", "--sign", "-", root).CombinedOutput(); err != nil {
		slog.Warn("signing the updated app", "err", err, "output", strings.TrimSpace(string(out)))
	}
}

// layout tells where an installed app starts and where its executable is in
// it: a macOS bundle, or else the directory the executable is in.
func layout(exe string) (root, rel string) {
	dir := filepath.Dir(exe)
	if bundle := filepath.Dir(filepath.Dir(dir)); strings.HasSuffix(bundle, ".app") &&
		filepath.Base(dir) == "MacOS" && filepath.Base(filepath.Dir(dir)) == "Contents" {
		return bundle, filepath.Join("Contents", "MacOS", filepath.Base(exe))
	}
	return dir, filepath.Base(exe)
}

func writable(dir string) bool {
	f, err := os.CreateTemp(dir, ".cat-update-*")
	if err != nil {
		return false
	}
	f.Close()
	return os.Remove(f.Name()) == nil
}

const (
	newSuffix = ".cat-new"
	oldSuffix = ".cat-old"
)

// merge puts every file under src at the same place under dest. Files that
// are only in dest are left alone: plugins added beside the app, and whatever
// else shares its directory. All of the new files are written out beside the
// ones they replace before any is swapped in, so that running out of space
// or of permission leaves the installed version whole. A replaced file is
// renamed aside first, which works on one that is running, and then removed;
// the ones that could not be removed are returned.
func merge(src, dest string) (left []string, err error) {
	var files []string
	defer func() {
		for _, rel := range files {
			_ = os.Remove(filepath.Join(dest, rel) + newSuffix)
		}
	}()
	err = filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dest, rel), 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		// "._name" is what macOS's tar makes of a file's extended attributes.
		if !info.Mode().IsRegular() || strings.HasPrefix(d.Name(), "._") {
			return nil
		}
		files = append(files, rel)
		return copyFile(path, filepath.Join(dest, rel)+newSuffix, info.Mode().Perm())
	})
	if err != nil {
		return nil, err
	}
	for _, rel := range files {
		to := filepath.Join(dest, rel)
		old := to + oldSuffix
		_ = os.Remove(old)
		had := os.Rename(to, old) == nil
		if err := os.Rename(to+newSuffix, to); err != nil {
			if had {
				_ = os.Rename(old, to)
			}
			return left, err
		}
		if had && os.Remove(old) != nil {
			left = append(left, old)
		}
	}
	return left, nil
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	// The mode asked for, whatever the umask made of it.
	if err := out.Chmod(perm); err != nil && runtime.GOOS != "windows" {
		out.Close()
		return err
	}
	return out.Close()
}

// Newer reports whether version a is later than b. Versions are numbers
// separated by dots, with or without a leading "v"; one that cannot be read
// is never newer, and anything is newer than one that cannot be read.
func Newer(a, b string) bool {
	x, ok := parse(a)
	if !ok {
		return false
	}
	y, ok := parse(b)
	if !ok {
		return true
	}
	for i := 0; i < len(x) || i < len(y); i++ {
		var p, q int
		if i < len(x) {
			p = x[i]
		}
		if i < len(y) {
			q = y[i]
		}
		if p != q {
			return p > q
		}
	}
	return false
}

func parse(v string) ([]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	// A suffix such as -rc1 or +build is not compared.
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	if v == "" {
		return nil, false
	}
	var out []int
	for _, part := range strings.Split(v, ".") {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}
