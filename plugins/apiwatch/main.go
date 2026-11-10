// Command cat-plugin-apiwatch polls HTTP APIs and emits an event when what
// they return changes.
package main

import cat "github.com/dounx/cat/sdk/go"

func main() { cat.Run(New()) }
