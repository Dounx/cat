// Command cat-plugin-email connects Cat to a mailbox: new mail comes in over
// IMAP, and replies and mail of the hub's own go out over SMTP.
package main

import cat "github.com/dounx/cat/sdk/go"

func main() { cat.Run(New()) }
