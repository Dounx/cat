// Command cat-plugin-telegram connects Cat to Telegram through the Bot API.
package main

import cat "github.com/dounx/cat/sdk/go"

func main() { cat.Run(New()) }
