// Command cat-plugin-rsshub watches routes on an RSSHub instance and emits an
// event for every new item.
package main

import (
	"github.com/dounx/cat/plugins/internal/feedwatch"
	cat "github.com/dounx/cat/sdk/go"
)

func main() { cat.Run(feedwatch.New(feedwatch.RSSHub)) }
