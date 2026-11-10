// Command cat-plugin-webwatch watches web pages and emits an event when their
// content changes.
package main

import cat "github.com/dounx/cat/sdk/go"

func main() { cat.Run(New()) }
