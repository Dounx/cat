// Command cat-plugin-rss watches RSS / Atom feeds and RSSHub routes and emits
// an event for every new item.
package main

import cat "github.com/dounx/cat/sdk/go"

func main() { cat.Run(New()) }
