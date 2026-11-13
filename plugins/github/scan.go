package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	cat "github.com/dounx/cat/sdk/go"
)

// cursorFor names the cursor that keeps track of one kind of thing ("issues",
// "issues/comments", "pulls/comments") in a repository: the repository's own
// when it is watched, the notifications' when those are read, and none
// otherwise, where only a webhook brings anything.
func (b *bot) cursorFor(name, kind string) string {
	b.wmu.RLock()
	defer b.wmu.RUnlock()
	if key := strings.ToLower(name); b.wants[key] != nil {
		return "repo:" + key + " " + kind
	}
	if b.inbox != nil {
		return inboxKey
	}
	return ""
}

// cur gives the cursor kept under a state key. The caller holds b.mu.
func (b *bot) cur(h *cat.Host, key string) *cursor {
	c := b.cursors[key]
	if c == nil {
		c = &cursor{}
		if v, ok := h.StateGet(key); ok {
			_ = json.Unmarshal([]byte(v), c)
		}
		if c.Seen == nil {
			c.Seen = map[string]time.Time{}
		}
		b.cursors[key] = c
	}
	return c
}

func (b *bot) since(h *cat.Host, key string) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cur(h, key).Since
}

// fresh reports whether something is still to be dealt with.
func (b *bot) fresh(h *cat.Host, ckey, key string, at time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.cur(h, ckey)
	_, seen := c.Seen[key]
	return !seen && !at.Before(c.Since)
}

// claim takes something to deal with: true the first time it is asked about
// a key, and never again. When strict, what is older than the cursor is not
// to be had either: a list also brings old things that only changed. Without
// a cursor there is nothing to tell apart, and everything is news.
func (b *bot) claim(h *cat.Host, ckey, key string, at time.Time, strict bool) bool {
	if ckey == "" {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.cur(h, ckey)
	if _, seen := c.Seen[key]; seen || (strict && at.Before(c.Since)) {
		return false
	}
	c.Seen[key], c.dirty = at, true
	return true
}

// advance moves a cursor up to the newest thing a list has shown.
func (b *bot) advance(h *cat.Host, ckey string, latest time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.cur(h, ckey)
	if next := latest.Add(-overlap); next.After(c.Since) {
		c.Since, c.dirty = next.UTC(), true
	}
	for key, at := range c.Seen {
		if at.Before(c.Since) {
			delete(c.Seen, key)
			c.dirty = true
		}
	}
}

// flush stores the cursors that have changed.
func (b *bot) flush(h *cat.Host) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for key, c := range b.cursors {
		if c.dirty {
			data, _ := json.Marshal(c)
			if err := h.StateSet(key, string(data)); err != nil {
				h.Log("error", "保存进度失败：%v", err)
				continue
			}
			c.dirty = false
		}
	}
}

// number is the last segment of an API address: the issue or pull request a
// comment belongs to.
func number(url string) int {
	n, _ := strconv.Atoi(url[strings.LastIndex(url, "/")+1:])
	return n
}

func conv(name string, n int) string { return name + "#" + strconv.Itoa(n) }

func (b *bot) setThread(name string, n int, t thread) {
	b.mu.Lock()
	b.threads[strings.ToLower(conv(name, n))] = t
	b.mu.Unlock()
}

// thread finds what a comment's issue is called, asking GitHub when no
// earlier message has told.
func (b *bot) thread(ctx context.Context, name string, n int) thread {
	b.mu.Lock()
	t, ok := b.threads[strings.ToLower(conv(name, n))]
	b.mu.Unlock()
	if ok {
		return t
	}
	var is issue
	if _, _, err := b.call(ctx, "GET", "/repos/"+name+"/issues/"+strconv.Itoa(n), nil, &is, ""); err != nil {
		return thread{}
	}
	t = thread{title: is.Title, pr: is.PullRequest != nil}
	b.setThread(name, n, t)
	return t
}

type found struct {
	at time.Time
	ev cat.Event
}

// scan turns what GitHub lists or delivers into events, each thing once.
type scan struct {
	b   *bot
	ctx context.Context
	h   *cat.Host
	// strict is for lists, which also bring old things that have only
	// changed; a webhook delivers nothing but news.
	strict bool
	// only, when set, says which texts are wanted.
	only func(string) bool
	news []found
}

func (s *scan) add(name string, n int, t thread, typ, kind, id, body, url string, a account, at time.Time, raw any) map[string]any {
	c := conv(name, n)
	label := c
	if t.title != "" {
		label += " " + t.title
	}
	mentioned := s.b.mentions(body)
	payload := map[string]any{
		"text": body, "message_id": id, "mentioned": mentioned, "username": a.Login,
		"kind": kind, "repo": name, "number": n, "title": t.title, "url": url, "is_pull_request": t.pr,
	}
	s.news = append(s.news, found{at, cat.Event{
		Type:         typ,
		Time:         &at,
		Conversation: &cat.Conversation{ID: c, Kind: "group", Name: label},
		Actor:        &cat.Actor{ID: strconv.FormatInt(a.ID, 10), Name: a.Login, IsBot: a.Type == "Bot"},
		Payload:      payload,
		Raw:          raw,
		Triggers:     s.b.askedBy(name, mentioned),
	}})
	return payload
}

// digest tells one text from another, in a key.
func digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:6])
}

// issue takes an issue or pull request: newly opened, or with its text
// edited, at the time is.UpdatedAt. An edit is known by the text it leaves,
// so that a webhook and a list telling of the same one make one event.
func (s *scan) issue(name string, is issue, raw any, edited bool) {
	t := thread{title: is.Title, pr: is.PullRequest != nil}
	s.b.setThread(name, is.Number, t)
	ckey, c := s.b.cursorFor(name, "issues"), conv(name, is.Number)
	text := strings.TrimSpace(is.Title + "\n\n" + is.Body)
	edit := "e " + c + "@" + digest(text)
	typ, at := "message.created", is.CreatedAt
	if edited {
		if !s.b.claim(s.h, ckey, edit, is.UpdatedAt, s.strict) {
			return
		}
		typ, at = "message.edited", is.UpdatedAt
	} else {
		if !s.b.claim(s.h, ckey, c, is.CreatedAt, s.strict) {
			return
		}
		s.b.claim(s.h, ckey, edit, is.UpdatedAt, false) // what is read now is the edited text already
	}
	if s.b.own(is.User, is.Body) || (s.only != nil && !s.only(text)) {
		return
	}
	labels := []string{}
	for _, l := range is.Labels {
		labels = append(labels, l.Name)
	}
	p := s.add(name, is.Number, t, typ, "issue", strconv.Itoa(is.Number), text, is.HTMLURL, is.User, at, raw)
	p["labels"], p["state"] = labels, is.State
}

// comment takes a comment under issue or pull request n: of kind "comment",
// or "review_comment" for one on a diff. action is "created" or "edited" as
// a webhook says, and empty for a list, which has to tell by the times.
func (s *scan) comment(name string, n int, c comment, raw any, kind, action string) {
	list, key := "issues/comments", "c"
	if kind == "review_comment" {
		list, key = "pulls/comments", "r"
	}
	ckey := s.b.cursorFor(name, list)
	key += strconv.FormatInt(c.ID, 10)
	edit := key + "@" + stamp(c.UpdatedAt)
	changed := c.UpdatedAt.After(c.CreatedAt)
	typ, at := "message.created", c.CreatedAt
	switch {
	case action == "edited":
		s.b.claim(s.h, ckey, key, c.CreatedAt, false) // it is no news to a list later
		if !s.b.claim(s.h, ckey, edit, c.UpdatedAt, false) {
			return
		}
		typ, at = "message.edited", c.UpdatedAt
	case s.b.claim(s.h, ckey, key, c.CreatedAt, s.strict):
		if changed { // what is read now is the edited text already
			s.b.claim(s.h, ckey, edit, c.UpdatedAt, false)
		}
	case action == "" && changed && s.b.claim(s.h, ckey, edit, c.UpdatedAt, s.strict):
		typ, at = "message.edited", c.UpdatedAt
	default:
		return
	}
	if s.b.own(c.User, c.Body) || (s.only != nil && !s.only(c.Body)) {
		return
	}
	t := s.b.thread(s.ctx, name, n)
	if kind == "review_comment" {
		t.pr = true
	}
	s.add(name, n, t, typ, kind, strconv.FormatInt(c.ID, 10), c.Body, c.HTMLURL, c.User, at, raw)
}

// review takes what a reviewer wrote about pull request n as a whole: newly
// submitted, or edited at the given time. One that says nothing in words (a
// bare approval) is no message.
func (s *scan) review(name string, n int, rv review, raw any, edited bool, at time.Time) {
	if rv.SubmittedAt == nil || strings.EqualFold(rv.State, "pending") {
		return
	}
	ckey, key := s.b.cursorFor(name, "issues"), "v"+strconv.FormatInt(rv.ID, 10)
	edit := key + "@" + digest(rv.Body)
	typ := "message.edited"
	if edited {
		if !s.b.claim(s.h, ckey, edit, at, s.strict) {
			return
		}
	} else {
		typ, at = "message.created", *rv.SubmittedAt
		if !s.b.claim(s.h, ckey, key, at, s.strict) {
			return
		}
		s.b.claim(s.h, ckey, edit, at, false)
	}
	if strings.TrimSpace(rv.Body) == "" || s.b.own(rv.User, rv.Body) || (s.only != nil && !s.only(rv.Body)) {
		return
	}
	t := s.b.thread(s.ctx, name, n)
	t.pr = true
	p := s.add(name, n, t, typ, "review", strconv.FormatInt(rv.ID, 10), rv.Body, rv.HTMLURL, rv.User, at, raw)
	p["review_state"] = strings.ToLower(rv.State)
}

// reviews reads the reviews of a pull request: the new ones, and those among
// edited (by id, with the time of the edit) whose text has changed.
func (s *scan) reviews(name string, n int, edited map[int64]time.Time) error {
	var items []json.RawMessage
	if _, _, err := s.b.call(s.ctx, "GET", "/repos/"+name+"/pulls/"+strconv.Itoa(n)+"/reviews?per_page=100", nil, &items, ""); err != nil {
		return err
	}
	for _, raw := range items {
		var rv review
		if json.Unmarshal(raw, &rv) != nil {
			continue
		}
		s.review(name, n, rv, raw, false, time.Time{})
		if at, ok := edited[rv.ID]; ok {
			s.review(name, n, rv, raw, true, at)
		}
	}
	return nil
}

// edits asks when the texts of the issues and pull requests that have
// changed since a time were last edited, and those of their reviews: the
// lists say that something about an issue has changed, not what. Only
// GitHub's GraphQL API tells.
func (b *bot) edits(ctx context.Context, name string, since time.Time) (texts map[int]time.Time, reviews map[int]map[int64]time.Time, err error) {
	type node struct {
		Number       int        `json:"number"`
		LastEditedAt *time.Time `json:"lastEditedAt"`
		Reviews      struct {
			Nodes []struct {
				ID           int64      `json:"databaseId"`
				LastEditedAt *time.Time `json:"lastEditedAt"`
			} `json:"nodes"`
		} `json:"reviews"`
	}
	var out struct {
		Data struct {
			Repository struct {
				Issues       struct{ Nodes []node } `json:"issues"`
				PullRequests struct{ Nodes []node } `json:"pullRequests"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	owner, short, _ := strings.Cut(name, "/")
	_, _, err = b.call(ctx, "POST", "/graphql", map[string]any{
		"query": `query($owner:String!,$name:String!,$since:DateTime!){repository(owner:$owner,name:$name){` +
			`issues(first:100,orderBy:{field:UPDATED_AT,direction:DESC},filterBy:{since:$since}){nodes{number lastEditedAt}} ` +
			`pullRequests(first:50,orderBy:{field:UPDATED_AT,direction:DESC}){nodes{number lastEditedAt reviews(last:30){nodes{databaseId lastEditedAt}}}}}}`,
		"variables": map[string]any{"owner": owner, "name": short, "since": stamp(since)},
	}, &out, "")
	if err != nil {
		return nil, nil, err
	}
	if len(out.Errors) > 0 {
		return nil, nil, errors.New("GitHub GraphQL：" + out.Errors[0].Message)
	}
	texts, reviews = map[int]time.Time{}, map[int]map[int64]time.Time{}
	for _, n := range append(out.Data.Repository.Issues.Nodes, out.Data.Repository.PullRequests.Nodes...) {
		if n.LastEditedAt != nil {
			texts[n.Number] = *n.LastEditedAt
		}
		for _, rv := range n.Reviews.Nodes {
			if rv.LastEditedAt != nil {
				if reviews[n.Number] == nil {
					reviews[n.Number] = map[int64]time.Time{}
				}
				reviews[n.Number][rv.ID] = *rv.LastEditedAt
			}
		}
	}
	return texts, reviews, nil
}

// emit sends what was found, oldest first, and says how many.
func (s *scan) emit() int {
	sort.SliceStable(s.news, func(i, j int) bool { return s.news[i].at.Before(s.news[j].at) })
	n := 0
	for _, f := range s.news {
		if _, err := s.h.Emit(f.ev); err != nil {
			s.h.Log("error", "emit: %v", err)
			continue
		}
		n++
	}
	s.b.flush(s.h)
	return n
}

// check looks once at a repository or at the notifications and emits what
// is new there.
func (b *bot) check(ctx context.Context, h *cat.Host, t target) (int, error) {
	s := &scan{b: b, ctx: ctx, h: h, strict: true}
	var err error
	switch t := t.(type) {
	case *repo:
		err = s.repo(t.name)
	case *inbox:
		s.only = b.mentions
		err = s.inbox()
	}
	if ctx.Err() == nil {
		b.looked(t, err)
	}
	return s.emit(), err
}

// begin sets cursors that have never been used to GitHub's present, and
// reports whether it did: on first sight only what comes later counts.
func (s *scan) begin(now func() (time.Time, error), keys ...string) (bool, error) {
	if !s.b.since(s.h, keys[0]).IsZero() {
		return false, nil
	}
	at, err := now()
	if err != nil {
		return false, err
	}
	s.b.mu.Lock()
	for _, key := range keys {
		c := s.b.cur(s.h, key)
		c.Since, c.dirty = at, true
	}
	s.b.mu.Unlock()
	return true, nil
}

// list fetches what has changed in one list since its cursor, newest first:
// nothing when it is all as it was at the last look.
func (s *scan) list(path, ckey, extra string) (items []json.RawMessage, err error) {
	// A second early: whether "since" takes in the second itself is not
	// something to depend on.
	path += "?per_page=100&since=" + stamp(s.b.since(s.h, ckey).Add(-time.Second)) + extra
	_, _, err = s.b.call(s.ctx, "GET", path, nil, &items, path[:strings.Index(path, "?")])
	return items, err
}

func (s *scan) repo(name string) error {
	b := s.b
	issues, comments, diffs := b.cursorFor(name, "issues"), b.cursorFor(name, "issues/comments"), b.cursorFor(name, "pulls/comments")
	first, err := s.begin(func() (time.Time, error) {
		hdr, _, err := b.call(s.ctx, "GET", "/repos/"+name, nil, nil, "")
		return serverTime(hdr), err
	}, issues, comments, diffs)
	if first || err != nil {
		return err
	}

	since := b.since(s.h, issues)
	items, err := s.list("/repos/"+name+"/issues", issues, "&state=all&sort=updated&direction=desc")
	if err != nil {
		return err
	}
	// Each issue that has changed is looked into once per change: for an
	// edited text, and for the reviews of a pull request.
	type change struct {
		is  issue
		raw json.RawMessage
		key string
	}
	var changed []change
	var latest time.Time
	for _, raw := range items {
		var is issue
		if json.Unmarshal(raw, &is) != nil {
			continue
		}
		s.issue(name, is, raw, false)
		if key := "u " + conv(name, is.Number) + "@" + stamp(is.UpdatedAt); b.fresh(s.h, issues, key, is.UpdatedAt) {
			changed = append(changed, change{is, raw, key})
		}
		if is.UpdatedAt.After(latest) {
			latest = is.UpdatedAt
		}
	}
	if len(changed) > 0 {
		texts, reviews, err := b.edits(s.ctx, name, since.Add(-time.Second))
		if err != nil {
			return err
		}
		for _, c := range changed {
			updated := c.is.UpdatedAt
			if at, ok := texts[c.is.Number]; ok {
				c.is.UpdatedAt = at
				s.issue(name, c.is, c.raw, true)
			}
			if c.is.PullRequest != nil {
				if err := s.reviews(name, c.is.Number, reviews[c.is.Number]); err != nil {
					return err
				}
			}
			b.claim(s.h, issues, c.key, updated, false)
		}
	}
	b.advance(s.h, issues, latest)

	for _, l := range []struct{ list, ckey, kind string }{{"issues/comments", comments, "comment"}, {"pulls/comments", diffs, "review_comment"}} {
		items, err := s.list("/repos/"+name+"/"+l.list, l.ckey, "&sort=updated&direction=desc")
		if err != nil {
			return err
		}
		if err := s.comments(name, 0, items, l.kind, l.ckey); err != nil {
			return err
		}
	}
	return nil
}

// comments takes a list of comments: under issue or pull request n, or, when
// n is 0, each under the one it names. With a cursor of its own (ckey) the
// list moves it.
func (s *scan) comments(name string, n int, items []json.RawMessage, kind, ckey string) error {
	var latest time.Time
	for _, raw := range items {
		var c comment
		if json.Unmarshal(raw, &c) != nil {
			continue
		}
		under := n
		if under == 0 {
			if under = number(c.IssueURL); kind == "review_comment" {
				under = number(c.PullRequestURL)
			}
		}
		if under == 0 {
			continue
		}
		s.comment(name, under, c, raw, kind, "")
		if c.UpdatedAt.After(latest) {
			latest = c.UpdatedAt
		}
	}
	if ckey != "" {
		s.b.advance(s.h, ckey, latest)
	}
	return s.ctx.Err()
}

// inbox reads the notifications for issues and pull requests of repositories
// that are not watched, and looks through each that has changed.
func (s *scan) inbox() error {
	b := s.b
	first, err := s.begin(func() (time.Time, error) {
		hdr, err := b.readInbox(s.ctx, time.Now(), nil, "")
		return serverTime(hdr), err
	}, inboxKey)
	if first || err != nil {
		return err
	}
	since := b.since(s.h, inboxKey)
	var threads []struct {
		UpdatedAt  time.Time `json:"updated_at"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Subject struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"subject"`
	}
	if _, err := b.readInbox(s.ctx, since.Add(-time.Second), &threads, inboxKey); err != nil {
		return err
	}
	var latest time.Time
	for _, th := range threads {
		name, n := th.Repository.FullName, number(th.Subject.URL)
		pr := th.Subject.Type == "PullRequest"
		if th.UpdatedAt.After(latest) {
			latest = th.UpdatedAt
		}
		if (th.Subject.Type != "Issue" && !pr) || n == 0 || b.cursorFor(name, "issues") != inboxKey {
			continue
		}
		key := "u " + conv(name, n) + "@" + stamp(th.UpdatedAt)
		if !b.fresh(s.h, inboxKey, key, th.UpdatedAt) {
			continue
		}
		if err := s.conversation(name, n, pr, since); err != nil {
			return err
		}
		b.claim(s.h, inboxKey, key, th.UpdatedAt, false)
	}
	b.advance(s.h, inboxKey, latest)
	return nil
}

// conversation looks through one issue or pull request for what has come
// since a time.
func (s *scan) conversation(name string, n int, pr bool, since time.Time) error {
	b, base := s.b, "/repos/"+name+"/issues/"+strconv.Itoa(n)
	var raw json.RawMessage
	if _, _, err := b.call(s.ctx, "GET", base, nil, &raw, ""); err != nil {
		return err
	}
	var is issue
	if err := json.Unmarshal(raw, &is); err != nil {
		return err
	}
	s.issue(name, is, raw, false)
	query := "?per_page=100&since=" + stamp(since.Add(-time.Second))
	var items []json.RawMessage
	if _, _, err := b.call(s.ctx, "GET", base+"/comments"+query, nil, &items, ""); err != nil {
		return err
	}
	if err := s.comments(name, n, items, "comment", ""); err != nil || !pr {
		return err
	}
	items = nil
	if _, _, err := b.call(s.ctx, "GET", "/repos/"+name+"/pulls/"+strconv.Itoa(n)+"/comments"+query+"&sort=updated&direction=desc", nil, &items, ""); err != nil {
		return err
	}
	if err := s.comments(name, n, items, "review_comment", ""); err != nil {
		return err
	}
	return s.reviews(name, n, nil)
}
