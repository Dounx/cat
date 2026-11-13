package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	cat "github.com/dounx/cat/sdk/go"
)

const (
	defaultBase     = "https://api.github.com"
	maxText         = 60000 // GitHub takes 65536 characters in a comment
	defaultInterval = 30    // seconds
	minInterval     = 10
	inboxInterval   = 60 // seconds: GitHub asks not to read notifications more often
	// overlap is how far behind the newest thing seen a cursor stays, for what
	// GitHub lists a little late. What that brings twice is told by Seen.
	overlap = 30 * time.Second
	// marker ends everything this plugin writes. It is an HTML comment, so
	// GitHub doesn't show it; what carries it and comes from our own account
	// is not turned into an event, or a rule that replies would answer itself.
	// Whatever else the account writes still counts: with a personal token the
	// account is the person using Cat.
	marker = "<!-- cat -->"
)

// webBase is where people sign in, for an API address: github.com for
// api.github.com, and the server itself for GitHub Enterprise Server.
func webBase(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "https://github.com"
	}
	return u.Scheme + "://" + strings.TrimPrefix(u.Host, "api.")
}

// tick is the unit of the configured interval; shortened in tests.
var tick = time.Second

type account struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

// issue is an issue or a pull request: as the issue lists give both, or a
// pull request as a webhook sends it.
type issue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	HTMLURL   string    `json:"html_url"`
	User      account   `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Labels    []struct {
		Name string `json:"name"`
	} `json:"labels"`
	PullRequest *struct{} `json:"pull_request"`
}

// comment is a comment on an issue or pull request, or one on a line of a
// pull request's diff (which names its pull request instead of its issue).
type comment struct {
	ID             int64     `json:"id"`
	Body           string    `json:"body"`
	HTMLURL        string    `json:"html_url"`
	IssueURL       string    `json:"issue_url"`
	PullRequestURL string    `json:"pull_request_url"`
	User           account   `json:"user"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// review is what a reviewer says about a pull request as a whole.
type review struct {
	ID          int64      `json:"id"`
	Body        string     `json:"body"`
	State       string     `json:"state"`
	HTMLURL     string     `json:"html_url"`
	User        account    `json:"user"`
	SubmittedAt *time.Time `json:"submitted_at"`
}

type apiError struct {
	Status  int
	Message string
	Limited bool
}

func (e *apiError) Error() string {
	switch {
	case e.Status == 401:
		return "Token 无效或已过期"
	case e.Limited:
		return "触发了 GitHub 的频率限制，过一会儿再试"
	case e.Status == 403:
		return "Token 的权限不够：" + e.Message
	case e.Status == 404:
		return "找不到，或 Token 无权访问"
	}
	return fmt.Sprintf("GitHub %d: %s", e.Status, e.Message)
}

// thread is what a comment doesn't say about the issue it is under.
type thread struct {
	title string
	pr    bool
}

// cursor is how far one list has been read. What is older than Since is
// done with. Seen holds what has been dealt with from Since on, by key and
// with its time: "acme/app#7" an issue or pull request opened, "c1" a
// comment, "r1" a comment on a diff, "v1" a review, "c1@<time>" an edit,
// "u acme/app#7@<time>" a pull request whose reviews have been read. A
// webhook puts what it delivers in here too, so that the next look at the
// list doesn't bring it a second time.
type cursor struct {
	Since time.Time            `json:"since"`
	Seen  map[string]time.Time `json:"seen,omitempty"`
	dirty bool
}

// target is something looked at on a schedule: a watched repository, or the
// account's notifications.
type target interface {
	Label() string
	Interval() time.Duration
}

type repo struct {
	name  string // owner/repo
	every time.Duration
	// problem is why the last look went wrong, to tell the rules that ask
	// for it. Under bot.wmu, like the inbox's.
	problem string
}

func (r *repo) Label() string           { return r.name }
func (r *repo) Interval() time.Duration { return r.every }

// inbox is the account's notifications, read for what mentions it in
// repositories that are not watched, when a rule wants mentions anywhere.
type inbox struct {
	every   time.Duration
	problem string
}

func (*inbox) Label() string             { return "通知" }
func (i *inbox) Interval() time.Duration { return i.every }

const inboxKey = "mentions"

type bot struct {
	token  string
	base   string
	secret string // of the webhook; without one, deliveries are refused
	client *http.Client
	me     account
	every  time.Duration // between two looks at a repository

	// What the rules ask for, from their trigger nodes.
	wmu      sync.RWMutex
	repos    []*repo
	inbox    *inbox              // nil unless some rule wants mentions anywhere
	wants    map[string][]string // repository, lower case → the triggers that watch it
	anywhere []string            // the triggers that want mentions anywhere

	mu      sync.Mutex
	etags   map[string][2]string // what was asked last: list → its URL and ETag
	cursors map[string]*cursor   // by state key
	threads map[string]thread    // by conversation id, lower case
}

var repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// parseRepos reads one repository per line, as owner/repo or as its address.
func parseRepos(s string) (names, invalid []string) {
	seen := map[string]bool{}
	for _, line := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == ',' }) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name := line
		if _, rest, ok := strings.Cut(name, "://"); ok {
			_, name, _ = strings.Cut(rest, "/")
		}
		name = strings.TrimSuffix(strings.Trim(name, "/"), ".git")
		if !repoName.MatchString(name) {
			invalid = append(invalid, line)
			continue
		}
		if key := strings.ToLower(name); !seen[key] {
			seen[key] = true
			names = append(names, name)
		}
	}
	return names, invalid
}

func newBot(cfg map[string]any) *bot {
	base := str(cfg, "api_base")
	if base == "" {
		base = defaultBase
	}
	seconds := float64(defaultInterval)
	switch v := cfg["interval"].(type) {
	case float64:
		seconds = v
	case string:
		if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			seconds = n
		}
	}
	if seconds <= 0 {
		seconds = defaultInterval
	}
	seconds = max(seconds, minInterval)
	b := &bot{
		token: str(cfg, "token"), base: strings.TrimRight(base, "/"), secret: str(cfg, "webhook_secret"),
		client: &http.Client{Timeout: 30 * time.Second},
		etags:  map[string][2]string{}, cursors: map[string]*cursor{}, threads: map[string]thread{},
		every: time.Duration(seconds * float64(tick)),
	}
	return b
}

// wanted reads what the trigger nodes of the rules ask for: the repositories
// each watches, and which of them want mentions anywhere. errs says what is
// wrong with single triggers, by id.
func wanted(list []cat.Trigger) (names []string, wants map[string][]string, anywhere []string, errs map[string]string) {
	wants, errs = map[string][]string{}, map[string]string{}
	for _, t := range list {
		repos, invalid := parseRepos(str(t.Params, "repos"))
		if len(invalid) > 0 {
			errs[t.ID] = "「" + strings.Join(invalid, "」「") + "」不是 owner/repo 的写法"
		}
		for _, name := range repos {
			key := strings.ToLower(name)
			if wants[key] == nil {
				names = append(names, name)
			}
			wants[key] = append(wants[key], t.ID)
		}
		if t.Params["mentions"] == true {
			anywhere = append(anywhere, t.ID)
		}
	}
	return names, wants, anywhere, errs
}

// watch takes what the rules ask for from now on, and says what is wrong
// with single triggers. What is watched from the start goes on where it was
// left. What comes to be watched later (not initial) begins at the present,
// and is tried once, so that a rule asking for a repository the token can't
// reach says so.
func (b *bot) watch(ctx context.Context, h *cat.Host, list []cat.Trigger, initial bool) map[string]string {
	names, wants, anywhere, errs := wanted(list)
	b.wmu.Lock()
	old := map[string]*repo{}
	for _, r := range b.repos {
		old[strings.ToLower(r.name)] = r
	}
	var repos, added []*repo
	for _, name := range names {
		r := old[strings.ToLower(name)]
		if r == nil {
			r = &repo{name: name, every: b.every}
			added = append(added, r)
		}
		repos = append(repos, r)
	}
	box, fresh := b.inbox, false
	if len(anywhere) == 0 {
		box = nil
	} else if box == nil {
		box, fresh = &inbox{every: max(b.every, time.Duration(inboxInterval*float64(tick)))}, true
	}
	b.repos, b.inbox, b.wants, b.anywhere = repos, box, wants, anywhere
	b.wmu.Unlock()
	if initial {
		return errs
	}

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var keys []string
	for _, r := range added {
		for _, kind := range []string{"issues", "issues/comments", "pulls/comments"} {
			keys = append(keys, "repo:"+strings.ToLower(r.name)+" "+kind)
		}
		_, _, err := b.call(ctx, "GET", "/repos/"+r.name, nil, nil, "")
		b.looked(r, err)
	}
	if fresh {
		keys = append(keys, inboxKey)
		_, err := b.readInbox(ctx, time.Now(), nil, "")
		b.looked(box, err)
	}
	b.mu.Lock()
	for _, key := range keys {
		b.cursors[key] = &cursor{Seen: map[string]time.Time{}, dirty: true}
	}
	b.mu.Unlock()
	b.flush(h)

	blame := func(ids []string, msg string) {
		for _, id := range ids {
			if msg != "" && errs[id] == "" {
				errs[id] = msg
			}
		}
	}
	b.wmu.RLock()
	defer b.wmu.RUnlock()
	for _, r := range repos {
		blame(wants[strings.ToLower(r.name)], r.problem)
	}
	if box != nil {
		blame(anywhere, box.problem)
	}
	return errs
}

// looked notes how a look at something watched went.
func (b *bot) looked(t target, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	switch t := t.(type) {
	case *repo:
		if msg != "" {
			msg = t.name + "：" + msg
		}
		t.problem = msg
	case *inbox:
		t.problem = msg
	}
}

// askedBy lists the triggers a message is for: those that watch its
// repository, and, when it mentions the account, those that want mentions
// anywhere.
func (b *bot) askedBy(name string, mentioned bool) []string {
	b.wmu.RLock()
	defer b.wmu.RUnlock()
	ids := append([]string{}, b.wants[strings.ToLower(name)]...)
	if mentioned {
		for _, id := range b.anywhere {
			dup := false
			for _, have := range ids {
				dup = dup || have == id
			}
			if !dup {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func (b *bot) targets() []target {
	b.wmu.RLock()
	defer b.wmu.RUnlock()
	out := make([]target, 0, len(b.repos)+1)
	for _, r := range b.repos {
		out = append(out, r)
	}
	if b.inbox != nil {
		out = append(out, b.inbox)
	}
	return out
}

// call makes one request. With a slot, a GET is conditional on what the same
// slot got for the same URL last time: when nothing has changed since, the
// status is 304, out is left alone, and GitHub doesn't count the request.
func (b *bot) call(ctx context.Context, method, path string, in, out any, slot string) (http.Header, int, error) {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return nil, 0, err
		}
		body = bytes.NewReader(data)
	}
	url := b.base + path
	if path == "/graphql" { // GitHub Enterprise Server has it beside /api/v3, not under it
		url = strings.TrimSuffix(b.base, "/v3") + path
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	if slot != "" {
		b.mu.Lock()
		if last := b.etags[slot]; last[0] == path && last[1] != "" {
			req.Header.Set("If-None-Match", last[1])
		}
		b.mu.Unlock()
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode == http.StatusNotModified {
		return resp.Header, resp.StatusCode, nil
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		limited := resp.StatusCode == 429 || (resp.StatusCode == 403 &&
			(resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != ""))
		return resp.Header, resp.StatusCode, &apiError{Status: resp.StatusCode, Message: e.Message, Limited: limited}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.Header, resp.StatusCode, fmt.Errorf("GitHub 返回了无法解析的响应（HTTP %d）", resp.StatusCode)
		}
	}
	if slot != "" {
		b.mu.Lock()
		b.etags[slot] = [2]string{path, resp.Header.Get("ETag")}
		b.mu.Unlock()
	}
	return resp.Header, resp.StatusCode, nil
}

func (b *bot) getMe(ctx context.Context) error {
	if b.token == "" {
		return errors.New("缺少 Token")
	}
	_, _, err := b.call(ctx, "GET", "/user", nil, &b.me, "")
	return err
}

// myRepos lists the repositories the account can reach, those pushed to
// last first: what there is to choose from when saying which to watch.
func (b *bot) myRepos(ctx context.Context) ([]cat.Option, error) {
	out := []cat.Option{}
	for page := 1; page <= 5; page++ {
		var items []struct {
			FullName string `json:"full_name"`
			Private  bool   `json:"private"`
			Archived bool   `json:"archived"`
		}
		if _, _, err := b.call(ctx, "GET", fmt.Sprintf("/user/repos?per_page=100&sort=pushed&page=%d", page), nil, &items, ""); err != nil {
			return out, err
		}
		for _, r := range items {
			note := ""
			if r.Private {
				note = "私有"
			}
			if !r.Archived {
				out = append(out, cat.Option{Value: r.FullName, Note: note})
			}
		}
		if len(items) < 100 {
			break
		}
	}
	return out, nil
}

// readInbox asks for the notifications changed since a time.
func (b *bot) readInbox(ctx context.Context, since time.Time, out any, slot string) (http.Header, error) {
	hdr, _, err := b.call(ctx, "GET", "/notifications?all=true&per_page=50&since="+stamp(since), nil, out, slot)
	if err != nil {
		return nil, fmt.Errorf("读取通知失败：%w（「在任何仓库被 @ 都接收」要用能读通知的令牌：在浏览器里登录得到的可以，自己建的要用勾了 repo 或 notifications 的经典令牌，细粒度令牌读不了通知）", err)
	}
	return hdr, nil
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// serverTime is GitHub's clock, as a response tells it.
func serverTime(hdr http.Header) time.Time {
	now, err := http.ParseTime(hdr.Get("Date"))
	if err != nil {
		now = time.Now()
	}
	return now.UTC().Truncate(time.Second)
}

// mentions reports whether text addresses the account by @name.
func (b *bot) mentions(text string) bool {
	if b.me.Login == "" {
		return false
	}
	text, at := strings.ToLower(text), "@"+strings.ToLower(b.me.Login)
	for {
		i := strings.Index(text, at)
		if i < 0 {
			return false
		}
		text = text[i+len(at):]
		// "@cat" is not a mention in "@cat-lover"
		if text == "" || !(text[0] == '-' || text[0] >= '0' && text[0] <= '9' || text[0] >= 'a' && text[0] <= 'z') {
			return true
		}
	}
}

func (b *bot) own(a account, body string) bool {
	return a.ID == b.me.ID && strings.Contains(body, marker)
}

var (
	convID  = regexp.MustCompile(`^([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#(\d+)$`)
	convURL = regexp.MustCompile(`^https?://[^/]+/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/(?:issues|pull)/(\d+)`)
)

// parseConv reads a conversation as owner/repo#number, or as the address of
// the issue or pull request.
func parseConv(s string) (repo, num string, err error) {
	s = strings.TrimSpace(s)
	m := convID.FindStringSubmatch(s)
	if m == nil {
		m = convURL.FindStringSubmatch(s)
	}
	if m == nil {
		return "", "", fmt.Errorf("看不懂会话「%s」：写成 owner/repo#编号，或贴 Issue、PR 的网址", s)
	}
	return m[1], m[2], nil
}

// split cuts s into pieces of at most n runes, preferring line breaks.
func split(s string, n int) []string {
	var out []string
	r := []rune(s)
	for len(r) > n {
		cut := n
		for i := n; i > n/2; i-- {
			if r[i-1] == '\n' {
				cut = i
				break
			}
		}
		out = append(out, string(r[:cut]))
		r = r[cut:]
	}
	return append(out, string(r))
}

func signed(body string) string { return strings.TrimRight(body, "\n") + "\n\n" + marker }

// comment writes under an issue or pull request.
func (b *bot) comment(ctx context.Context, conv, body string) (any, error) {
	name, num, err := parseConv(conv)
	if err != nil {
		return nil, err
	}
	if body == "" {
		return nil, errors.New("评论内容为空")
	}
	ids, url := []string{}, ""
	for _, part := range split(body, maxText) {
		var c comment
		if _, _, err := b.call(ctx, "POST", "/repos/"+name+"/issues/"+num+"/comments", map[string]any{"body": signed(part)}, &c, ""); err != nil {
			return map[string]any{"message_ids": ids}, fmt.Errorf("%s#%s：%w", name, num, err)
		}
		ids, url = append(ids, strconv.FormatInt(c.ID, 10)), c.HTMLURL
	}
	return map[string]any{"message_id": ids[len(ids)-1], "message_ids": ids, "conversation_id": name + "#" + num, "url": url}, nil
}

func (b *bot) createIssue(ctx context.Context, name, title, body, labels string) (any, error) {
	names, _ := parseRepos(name)
	if len(names) != 1 {
		return nil, errors.New("缺少仓库：写成 owner/repo")
	}
	name = names[0]
	if title == "" {
		return nil, errors.New("缺少标题")
	}
	in := map[string]any{"title": title, "body": signed(body)}
	if list := strings.FieldsFunc(labels, func(r rune) bool { return r == ',' || r == '，' }); len(list) > 0 {
		for i := range list {
			list[i] = strings.TrimSpace(list[i])
		}
		in["labels"] = list
	}
	var is issue
	if _, _, err := b.call(ctx, "POST", "/repos/"+name+"/issues", in, &is, ""); err != nil {
		return nil, fmt.Errorf("%s：%w", name, err)
	}
	return map[string]any{"number": is.Number, "url": is.HTMLURL, "conversation_id": name + "#" + strconv.Itoa(is.Number), "message_id": strconv.Itoa(is.Number)}, nil
}

// react puts an emoji on the message an event stands for.
func (b *bot) react(ctx context.Context, ev *cat.Event, content string) error {
	name, num, err := parseConv(ev.Conversation.ID)
	if err != nil {
		return err
	}
	path := "/repos/" + name + "/issues/" + num + "/reactions"
	switch id := str(ev.Payload, "message_id"); str(ev.Payload, "kind") {
	case "comment":
		path = "/repos/" + name + "/issues/comments/" + id + "/reactions"
	case "review_comment":
		path = "/repos/" + name + "/pulls/comments/" + id + "/reactions"
	}
	_, _, err = b.call(ctx, "POST", path, map[string]any{"content": content}, nil, "")
	return err
}
