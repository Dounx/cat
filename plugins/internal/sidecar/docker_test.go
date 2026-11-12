package sidecar

import "testing"

func TestDockerLaunchersFollowTheContext(t *testing.T) {
	for _, c := range []struct{ goos, ctx, first string }{
		{"darwin", "desktop-linux", "Docker"},
		{"darwin", "orbstack", "OrbStack"},
		{"darwin", "rancher-desktop", "Rancher Desktop"},
		{"darwin", "colima", "start"},
		{"darwin", "default", "Docker"},
		{"windows", "desktop-linux", "start"},
		{"linux", "default", "docker"},
		{"linux", "desktop-linux", "start"},
	} {
		l := dockerLaunchers(c.goos, c.ctx)
		if len(l) == 0 {
			t.Fatalf("%s/%s: nothing to try", c.goos, c.ctx)
		}
		if got := l[0][len(l[0])-1]; got != c.first {
			t.Errorf("%s/%s: first launcher ends in %q, want %q", c.goos, c.ctx, got, c.first)
		}
	}
}
