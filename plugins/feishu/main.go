// Command cat-plugin-feishu connects Cat to Feishu (Lark) through a bot of a
// self-built app, over the open platform's long connection.
package main

import (
	"fmt"
	"io"
	"log"
	"os"

	cat "github.com/dounx/cat/sdk/go"
)

func main() {
	// Cat is spoken to over stdout, and the Feishu SDK prints there on its
	// own account. So stdout is kept for Cat alone, and whatever else writes
	// to it lands on stderr.
	out := os.Stdout
	os.Stdout = os.Stderr
	// Its sending code also narrates every message through the standard
	// logger, whatever logger the client was given.
	log.SetOutput(io.Discard)
	if err := cat.Serve(New(), os.Stdin, out); err != nil && err != io.EOF {
		fmt.Fprintln(os.Stderr, "plugin:", err)
		os.Exit(1)
	}
}
