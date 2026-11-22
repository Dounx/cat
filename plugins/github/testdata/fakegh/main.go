// Command fakegh is a cross-platform stand-in for the GitHub CLI used by
// plugins/github tests. Signing in succeeds unless a file named "deny" lies
// next to this binary; the machine's own login is whatever "own-token" holds.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	exe, _ := os.Executable()
	state := filepath.Dir(exe)
	args := os.Args[1:]
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "fake gh: unsupported: %s\n", strings.Join(args, " "))
		os.Exit(1)
	}
	switch args[0] + " " + args[1] {
	case "auth login":
		cfgDir := os.Getenv("GH_CONFIG_DIR")
		if cfgDir == "" {
			fmt.Fprintln(os.Stderr, "fake gh: asked to sign in over the machine's own login")
			os.Exit(1)
		}
		joined := strings.Join(args, " ")
		_ = os.WriteFile(filepath.Join(state, "login-args"), []byte(joined+"\n"), 0o644)
		fmt.Fprintln(os.Stderr)
		if strings.Contains(joined, "--clipboard") {
			fmt.Fprintln(os.Stderr, "! One-time code (WDJB-MJHT) copied to clipboard")
		} else {
			fmt.Fprintln(os.Stderr, "! First copy your one-time code: WDJB-MJHT")
		}
		fmt.Fprintln(os.Stderr, "Open this URL to continue in your web browser: https://github.test/login/device")
		if _, err := os.Stat(filepath.Join(state, "deny")); err == nil {
			fmt.Fprintln(os.Stderr, "error: authorization was denied")
			os.Exit(1)
		}
		_ = os.WriteFile(filepath.Join(cfgDir, "token"), []byte("good-token\n"), 0o600)
		fmt.Fprintln(os.Stderr, "✓ Authentication complete.")
	case "auth token":
		if cfgDir := os.Getenv("GH_CONFIG_DIR"); cfgDir != "" {
			b, err := os.ReadFile(filepath.Join(cfgDir, "token"))
			if err != nil {
				os.Exit(1)
			}
			os.Stdout.Write(b)
			return
		}
		b, err := os.ReadFile(filepath.Join(state, "own-token"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "no oauth token found")
			os.Exit(1)
		}
		os.Stdout.Write(b)
	default:
		fmt.Fprintf(os.Stderr, "fake gh: unsupported: %s\n", strings.Join(args, " "))
		os.Exit(1)
	}
}
