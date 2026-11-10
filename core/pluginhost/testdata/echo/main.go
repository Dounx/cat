// A minimal plugin used by the host's tests: it echoes actions back as events.
package main

import (
	"context"
	"errors"
	"os"
	"strconv"

	cat "github.com/dounx/cat/sdk/go"
)

func main() {
	cat.Run(cat.Plugin{
		Start: func(ctx context.Context, h *cat.Host) error {
			if h.String("fail") == "yes" {
				return errors.New("configured to fail")
			}
			n := 0
			if v, ok := h.StateGet("starts"); ok {
				n, _ = strconv.Atoi(v)
			}
			_ = h.StateSet("starts", strconv.Itoa(n+1))
			_, err := h.Emit(cat.Event{Type: "echo.started", Payload: map[string]any{"starts": n + 1, "greeting": h.String("greeting")}})
			return err
		},
		Actions: map[string]cat.ActionFunc{
			"echo": func(_ context.Context, h *cat.Host, p map[string]any, actx cat.ActionContext) (any, error) {
				id, err := h.Emit(cat.Event{Type: "echo.said", Payload: map[string]any{"text": p["text"]},
					Actor: &cat.Actor{ID: "echo"}, Conversation: &cat.Conversation{ID: "c1", Kind: "private"}})
				trigger := ""
				if actx.Event != nil {
					trigger = actx.Event.Type
				}
				return map[string]any{"text": p["text"], "event_id": id, "trigger": trigger}, err
			},
			"fail": func(context.Context, *cat.Host, map[string]any, cat.ActionContext) (any, error) {
				return map[string]any{"partial": true}, errors.New("boom")
			},
			"crash": func(context.Context, *cat.Host, map[string]any, cat.ActionContext) (any, error) {
				os.Exit(7)
				return nil, nil
			},
			"wait": func(ctx context.Context, _ *cat.Host, _ map[string]any, _ cat.ActionContext) (any, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		},
		Ingress: func(_ context.Context, _ *cat.Host, req cat.IngressRequest) (cat.IngressResponse, error) {
			return cat.IngressResponse{Status: 200, Body: append([]byte("got:"), req.Body...)}, nil
		},
		Setup: func(ctx context.Context, s *cat.Setup) (map[string]any, error) {
			s.Progress("preparing", 0.5)
			out, err := s.Ask(ctx, cat.Step{Kind: cat.StepForm, Title: "Greeting", Schema: map[string]any{
				"type": "object", "properties": map[string]any{"greeting": map[string]any{"type": "string"}},
			}})
			if err != nil {
				return nil, err
			}
			ok, err := s.Confirm(ctx, "Save?", "")
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, cat.ErrCancelled
			}
			return map[string]any{"greeting": out["greeting"]}, nil
		},
	})
}
