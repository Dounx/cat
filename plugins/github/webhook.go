package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	cat "github.com/dounx/cat/sdk/go"
)

// ingress takes a webhook delivery. It is quicker than the lists, which go
// on being read all the same, for what a delivery that never came has left
// out; the cursors keep the two from bringing a thing twice.
func (b *bot) ingress(ctx context.Context, h *cat.Host, req cat.IngressRequest) (cat.IngressResponse, error) {
	answer := func(status int, text string) (cat.IngressResponse, error) {
		return cat.IngressResponse{Status: status, Body: []byte(text)}, nil
	}
	if b.secret == "" {
		return answer(403, "set a webhook secret on this connection first")
	}
	mac := hmac.New(sha256.New, []byte(b.secret))
	mac.Write(req.Body)
	if sig := strings.TrimPrefix(req.Headers["X-Hub-Signature-256"], "sha256="); !hmac.Equal([]byte(sig), []byte(hex.EncodeToString(mac.Sum(nil)))) {
		return answer(401, "signature mismatch")
	}
	var raw map[string]json.RawMessage
	var d struct {
		Action      string   `json:"action"`
		Issue       *issue   `json:"issue"`
		PullRequest *issue   `json:"pull_request"`
		Comment     *comment `json:"comment"`
		Review      *review  `json:"review"`
		Repository  struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if json.Unmarshal(req.Body, &raw) != nil || json.Unmarshal(req.Body, &d) != nil {
		return answer(400, "bad body")
	}
	name := d.Repository.FullName
	s := &scan{b: b, ctx: ctx, h: h}
	edited := d.Action == "edited"
	switch event := req.Headers["X-Github-Event"]; {
	case event == "ping":
		return answer(200, "pong")
	case !repoName.MatchString(name):
	case event == "issues" && d.Issue != nil && (d.Action == "opened" || edited):
		s.issue(name, *d.Issue, raw["issue"], edited)
	case event == "pull_request" && d.PullRequest != nil && (d.Action == "opened" || edited):
		d.PullRequest.PullRequest = &struct{}{}
		s.issue(name, *d.PullRequest, raw["pull_request"], edited)
	case event == "issue_comment" && d.Comment != nil && d.Issue != nil && (d.Action == "created" || edited):
		b.setThread(name, d.Issue.Number, thread{title: d.Issue.Title, pr: d.Issue.PullRequest != nil})
		s.comment(name, d.Issue.Number, *d.Comment, raw["comment"], "comment", d.Action)
	case event == "pull_request_review_comment" && d.Comment != nil && d.PullRequest != nil && (d.Action == "created" || edited):
		b.setThread(name, d.PullRequest.Number, thread{title: d.PullRequest.Title, pr: true})
		s.comment(name, d.PullRequest.Number, *d.Comment, raw["comment"], "review_comment", d.Action)
	case event == "pull_request_review" && d.Review != nil && d.PullRequest != nil && (d.Action == "submitted" || edited):
		b.setThread(name, d.PullRequest.Number, thread{title: d.PullRequest.Title, pr: true})
		s.review(name, d.PullRequest.Number, *d.Review, raw["review"], edited, time.Now().UTC().Truncate(time.Second))
	}
	return answer(200, fmt.Sprintf("%d", s.emit()))
}
