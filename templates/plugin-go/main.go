// A minimal Cat plugin. Build with:
//
//	go build -o cat-plugin-hello .
//
// then start Cat with --plugins pointing at the directory that contains this one.
package main

import (
	"context"
	"fmt"
	"time"

	cat "github.com/dounx/cat/sdk/go"
)

func main() { cat.Run(New()) }

// New returns the plugin; keeping it separate from main makes it testable.
func New() cat.Plugin {
	return cat.Plugin{
		// Start runs once per instance. Return when ready; keep working in
		// goroutines until ctx is cancelled.
		Start: func(ctx context.Context, h *cat.Host) error {
			interval := 60 * time.Second
			if n, ok := h.Config["interval"].(float64); ok && n > 0 {
				interval = time.Duration(n * float64(time.Second))
			}
			go func() {
				ticker := time.NewTicker(interval)
				defer ticker.Stop()
				for count := 1; ; count++ {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						if _, err := h.Emit(cat.Event{Type: "hello.tick", Payload: map[string]any{"count": count}}); err != nil {
							h.Log("error", "emit: %v", err)
						}
					}
				}
			}()
			return nil
		},
		Actions: map[string]cat.ActionFunc{
			// The returned object is the step's output: later steps read it
			// as {{ steps.<id>.text }}.
			"greet": func(_ context.Context, h *cat.Host, p map[string]any, _ cat.ActionContext) (any, error) {
				greeting := h.String("greeting")
				if greeting == "" {
					greeting = "你好"
				}
				return map[string]any{"text": fmt.Sprintf("%s，%v！", greeting, p["name"])}, nil
			},
		},
	}
}
