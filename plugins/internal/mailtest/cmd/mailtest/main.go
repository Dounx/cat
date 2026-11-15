// Command mailtest runs a mail server for one mailbox on this machine, to
// try the email plugin against without a mail provider: connect the plugin
// to it with "不加密连接" on, and whatever is sent through its SMTP side
// (by the plugin, or by anything else) lands in the mailbox.
//
//	go run ./plugins/internal/mailtest/cmd/mailtest -imap 127.0.0.1:1143 -smtp 127.0.0.1:1025
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/dounx/cat/plugins/internal/mailtest"
)

func main() {
	imap := flag.String("imap", "127.0.0.1:1143", "address of the IMAP server")
	smtp := flag.String("smtp", "127.0.0.1:1025", "address of the SMTP server")
	user := flag.String("user", "cat@mail.test", "the mailbox's address, and its login name")
	password := flag.String("password", "secret", "its password")
	flag.Parse()
	s, err := mailtest.Start(*imap, *smtp, *user, *password)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("mailbox %s (password %s): IMAP %s, SMTP %s, neither encrypted\n", *user, *password, s.IMAP, s.SMTP)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
	s.Close()
}
