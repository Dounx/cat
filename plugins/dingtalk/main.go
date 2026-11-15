// Command cat-plugin-dingtalk connects Cat to DingTalk through the bot of an
// internal app, over the open platform's Stream mode.
package main

import cat "github.com/dounx/cat/sdk/go"

func main() { cat.Run(New()) }
