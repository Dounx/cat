// Command fakecli stands in for harness CLIs (claude, codex, dsh) in plugin
// tests so they run on every OS without shell scripts or real accounts.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	args := os.Args[1:]
	switch name {
	case "claude":
		runClaude(args)
	case "codex":
		runCodex(args)
	case "dsh":
		runDsh(args)
	default:
		fmt.Fprintf(os.Stderr, "fakecli: unknown persona %q\n", name)
		os.Exit(2)
	}
}

func readStdin() string {
	b, _ := io.ReadAll(os.Stdin)
	return strings.TrimRight(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
}

func appendLog(lines ...string) int {
	path := os.Getenv("FAKE_LOG")
	if path == "" {
		return 1
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		for _, l := range lines {
			fmt.Fprintln(f, l)
		}
		f.Close()
	}
	b, _ := os.ReadFile(path)
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "ARGS:") {
			n++
		}
	}
	return n
}

func runClaude(args []string) {
	if len(args) > 0 && args[0] == "--version" {
		fmt.Println("9.9.9 (Claude Code)")
		return
	}
	prompt := readStdin()
	joined := strings.Join(args, " ")
	cwd, _ := os.Getwd()
	n := appendLog(
		"ARGS:"+joined,
		"PROMPT:"+prompt,
		fmt.Sprintf("ENV:%s|%s|%s", os.Getenv("ANTHROPIC_BASE_URL"), os.Getenv("ANTHROPIC_AUTH_TOKEN"), os.Getenv("ANTHROPIC_MODEL")),
		"CWD:"+cwd,
	)
	if strings.Contains(joined, "--resume gone") {
		fmt.Println(`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"No conversation found with session ID: gone","session_id":""}`)
		os.Exit(1)
	}
	if strings.Contains(prompt, "explode") {
		fmt.Println(`{"type":"result","subtype":"success","is_error":true,"result":"Failed to authenticate","session_id":"sess-err"}`)
		os.Exit(1)
	}
	fmt.Println("some warning on stdout")
	fmt.Printf(`{"type":"result","subtype":"success","is_error":false,"result":"reply %d","session_id":"sess-%d","total_cost_usd":0.01,"num_turns":1,"duration_ms":5}`+"\n", n, n)
}

func runCodex(args []string) {
	if len(args) > 0 && args[0] == "--version" {
		fmt.Println("codex-cli 0.0.0")
		return
	}
	prompt := readStdin()
	joined := strings.Join(args, " ")
	n := appendLog(
		"ARGS:"+joined,
		"PROMPT:"+strings.ReplaceAll(prompt, "\n", "~"),
	)
	outPath := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-o" {
			outPath = args[i+1]
		}
	}
	if strings.Contains(joined, "resume") && strings.Contains(joined, "gone") {
		fmt.Println(`{"type":"error","message":"session not found"}`)
		os.Exit(1)
	}
	if strings.Contains(prompt, "explode") {
		fmt.Println(`{"type":"thread.started","thread_id":"t-x"}`)
		fmt.Println(`{"type":"turn.failed","error":{"message":"quota exceeded"}}`)
		os.Exit(1)
	}
	fmt.Printf(`{"type":"thread.started","thread_id":"thread-%d"}`+"\n", n)
	fmt.Println(`{"type":"item.completed","item":{"id":"item_0","type":"error","message":"Codex is ignoring 2 unrecognized configuration settings."}}`)
	fmt.Println(`{"type":"turn.started"}`)
	fmt.Printf(`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"answer %d"}}`+"\n", n)
	fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}}`)
	if outPath != "" {
		_ = os.WriteFile(outPath, []byte(fmt.Sprintf("answer %d", n)), 0o644)
	}
}

func runDsh(args []string) {
	prompt := readStdin()
	joined := strings.Join(args, " ")
	key := os.Getenv("DEEPSEEK_API_KEY")
	n := appendLog(
		"ARGS:"+joined,
		"PROMPT:"+strings.ReplaceAll(prompt, "\n", "~"),
		"KEY:"+key,
	)
	if key == "" {
		fmt.Println(`{"type":"session","sessionId":"session-x","cwd":"/tmp"}`)
		fmt.Println(`{"type":"final","sessionId":"session-x","reason":{"kind":"error","error":{"message":"llm-deepseek: no API key","code":"MISSING_CREDENTIAL"}},"text":""}`)
		return
	}
	fmt.Printf(`{"type":"session","sessionId":"session-%d","cwd":"/tmp"}`+"\n", n)
	fmt.Println(`{"type":"status","phase":"turn_start","turn":1}`)
	fmt.Printf(`{"type":"final","sessionId":"session-%d","reason":{"kind":"completed"},"text":"done %d"}`+"\n", n, n)
}
