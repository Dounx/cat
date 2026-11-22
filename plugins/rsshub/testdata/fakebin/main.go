// Command fakebin stands in for docker, node and pnpm in plugins/rsshub tests.
// The test copies this binary under whichever tool names it wants on PATH.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

func main() {
	exe, _ := os.Executable()
	state := filepath.Dir(exe)
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	args := os.Args[1:]
	switch name {
	case "docker":
		runDocker(state, args)
	case "node":
		runNode(state, args)
	case "pnpm":
		runPnpm(state, args)
	default:
		fmt.Fprintf(os.Stderr, "fakebin: unknown tool %q\n", name)
		os.Exit(1)
	}
}

func appendFile(path, line string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		fmt.Fprintln(f, line)
		f.Close()
	}
}

func fakeHubPath(state string) string {
	p := filepath.Join(state, "fakersshub")
	if runtime.GOOS == "windows" {
		p += ".exe"
	}
	return p
}

func runDocker(state string, args []string) {
	appendFile(filepath.Join(state, "docker.log"), strings.Join(args, " "))
	if len(args) == 0 {
		os.Exit(1)
	}
	switch args[0] {
	case "info":
		fmt.Println("0.0.0-fake")
	case "image":
		if _, err := os.Stat(filepath.Join(state, "pulled")); err != nil {
			os.Exit(1)
		}
	case "pull":
		if len(args) > 1 {
			fmt.Println("Pulling", args[1])
		}
		_ = os.WriteFile(filepath.Join(state, "pulled"), nil, 0o644)
	case "rm":
		if len(args) > 2 {
			pidFile := filepath.Join(state, args[2]+".pid")
			if b, err := os.ReadFile(pidFile); err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
					if proc, err := os.FindProcess(pid); err == nil {
						_ = proc.Kill()
					}
				}
				_ = os.Remove(pidFile)
			}
		}
	case "run":
		var name, port string
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--name":
				if i+1 < len(args) {
					name = args[i+1]
					i++
				}
			case "-p":
				if i+1 < len(args) {
					parts := strings.Split(args[i+1], ":")
					if len(parts) >= 2 {
						port = parts[1]
					}
					i++
				}
			case "-e":
				i++
			}
		}
		cmd := exec.Command(fakeHubPath(state))
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		cmd.Env = append(os.Environ(), "PORT="+port, "FAKE_RSSHUB_LOG="+filepath.Join(state, "hub.log"))
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if name != "" {
			_ = os.WriteFile(filepath.Join(state, name+".pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
		}
		if err := cmd.Wait(); err != nil {
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "fake docker: unsupported: %s\n", strings.Join(args, " "))
		os.Exit(1)
	}
}

func runNode(state string, args []string) {
	if len(args) == 0 {
		os.Exit(1)
	}
	switch {
	case args[0] == "--version":
		b, err := os.ReadFile(filepath.Join(state, "node-version"))
		if err != nil {
			os.Exit(1)
		}
		os.Stdout.Write(b)
	case filepath.ToSlash(args[0]) == "dist/index.mjs":
		if _, err := os.Stat(args[0]); err != nil {
			fmt.Fprintln(os.Stderr, "Cannot find module dist/index.mjs")
			os.Exit(1)
		}
		cmd := exec.Command(fakeHubPath(state))
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		cmd.Env = append(os.Environ(), "FAKE_RSSHUB_LOG="+filepath.Join(state, "hub.log"))
		if err := cmd.Run(); err != nil {
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "fake node: unsupported: %s\n", strings.Join(args, " "))
		os.Exit(1)
	}
}

func runPnpm(state string, args []string) {
	appendFile(filepath.Join(state, "pnpm.log"), fmt.Sprintf("%s (skip-browser=%s)", strings.Join(args, " "), os.Getenv("PUPPETEER_SKIP_DOWNLOAD")))
	if len(args) == 0 {
		return
	}
	switch args[0] {
	case "install":
		fmt.Println("Done in 0.1s")
	case "build":
		if _, err := os.Stat(filepath.Join(state, "build-fails")); err == nil {
			fmt.Fprintln(os.Stderr, "ERR_PNPM build exploded")
			os.Exit(1)
		}
		_ = os.MkdirAll("dist", 0o755)
		_ = os.WriteFile(filepath.Join("dist", "index.mjs"), []byte("// built\n"), 0o644)
	}
}
