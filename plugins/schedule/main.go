// Command cat-plugin-schedule emits an event at the times it is told to:
// "every day at 9", "every 30 minutes", or any cron expression.
package main

import (
	_ "time/tzdata" // Windows has no time zone database of its own

	cat "github.com/dounx/cat/sdk/go"
)

func main() { cat.Run(New()) }
