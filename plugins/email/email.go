package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // mail in other charsets than UTF-8
	"github.com/emersion/go-message/mail"
	"golang.org/x/net/html"

	cat "github.com/dounx/cat/sdk/go"
)

const (
	// sentHeader marks the mail this plugin sent, so that a copy that comes
	// back to the same mailbox does not start a rule again.
	sentHeader = "X-Cat-Sent"
	// maxFetch is how much of a message is read; maxBody how much of its
	// text is kept.
	maxFetch = 2 << 20
	maxBody  = 20000
)

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

type account struct {
	address, username, password, name string
	imap, smtp, mailbox               string
	insecure                          bool
	every                             time.Duration
}

func newAccount(cfg map[string]any) (*account, error) {
	a := &account{
		address: strings.ToLower(str(cfg, "address")), username: str(cfg, "username"), password: str(cfg, "password"),
		name: str(cfg, "name"), imap: str(cfg, "imap"), smtp: str(cfg, "smtp"), mailbox: str(cfg, "mailbox"),
		every: 30 * time.Second,
	}
	a.insecure, _ = cfg["insecure"].(bool)
	if !strings.Contains(a.address, "@") {
		return nil, errors.New("邮箱地址不对")
	}
	if a.password == "" {
		return nil, errors.New("缺少密码或授权码")
	}
	for _, at := range []*string{&a.imap, &a.smtp} {
		if _, _, err := net.SplitHostPort(*at); err != nil {
			return nil, fmt.Errorf("服务器要写成「主机:端口」，%q 不是", *at)
		}
	}
	if a.username == "" {
		a.username = a.address
	}
	if a.mailbox == "" {
		a.mailbox = "INBOX"
	}
	if n, ok := cfg["interval"].(float64); ok && n >= 5 {
		a.every = time.Duration(n) * time.Second
	}
	return a, nil
}

// open connects to the IMAP server, logs in and selects the mailbox to read.
func (a *account) open() (*imapclient.Client, *imap.SelectData, error) {
	host, port, _ := net.SplitHostPort(a.imap)
	opts := &imapclient.Options{TLSConfig: &tls.Config{ServerName: host}}
	var c *imapclient.Client
	var err error
	switch {
	case a.insecure:
		c, err = imapclient.DialInsecure(a.imap, opts)
	case port == "993":
		c, err = imapclient.DialTLS(a.imap, opts)
	default:
		c, err = imapclient.DialStartTLS(a.imap, opts)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("连不上 IMAP 服务器 %s：%w", a.imap, err)
	}
	if err := c.Login(a.username, a.password).Wait(); err != nil {
		c.Close()
		return nil, nil, fmt.Errorf("IMAP 登录失败：%w。多数邮箱要先在设置里开启 IMAP，并用授权码代替登录密码", err)
	}
	// Some servers (163, 126) refuse to open a mailbox for a client that has
	// not said what it is.
	if c.Caps().Has(imap.CapID) {
		_, _ = c.ID(&imap.IDData{Name: "Cat", Version: "1.0"}).Wait()
	}
	box, err := c.Select(a.mailbox, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		c.Close()
		return nil, nil, fmt.Errorf("打不开文件夹 %s：%w", a.mailbox, err)
	}
	return c, box, nil
}

// A letter is what is kept of a message.
type letter struct {
	uid                     imap.UID
	subject, body           string
	from, replyTo           *mail.Address
	to, cc                  []string
	id                      string
	inReplyTo, references   []string
	date                    time.Time
	attachments             []map[string]any
	ours, automatic, noBody bool
}

func addresses(h mail.Header, key string) []string {
	list, _ := h.AddressList(key)
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, strings.ToLower(a.Address))
	}
	return out
}

// read parses a message as it came from the server.
func read(raw []byte) (*letter, error) {
	mr, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil && !message.IsUnknownCharset(err) {
		return nil, err
	}
	h := mr.Header
	l := &letter{to: addresses(h, "To"), cc: addresses(h, "Cc")}
	l.subject, _ = h.Subject()
	l.id, _ = h.MessageID()
	l.inReplyTo, _ = h.MsgIDList("In-Reply-To")
	l.references, _ = h.MsgIDList("References")
	l.date, _ = h.Date()
	if from, _ := h.AddressList("From"); len(from) > 0 {
		l.from = from[0]
	}
	if to, _ := h.AddressList("Reply-To"); len(to) > 0 {
		l.replyTo = to[0]
	}
	l.ours = h.Get(sentHeader) != ""
	// what a machine sent on its own: answering it invites an answer back
	auto := strings.ToLower(h.Get("Auto-Submitted"))
	precedence := strings.ToLower(h.Get("Precedence"))
	l.automatic = (auto != "" && auto != "no") || precedence == "bulk" || precedence == "junk" || precedence == "list" ||
		h.Get("List-Id") != "" || h.Get("List-Unsubscribe") != ""

	var plain, rich string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil && !message.IsUnknownCharset(err) {
			break // what was read so far still counts
		}
		switch ph := p.Header.(type) {
		case *mail.InlineHeader:
			kind, _, _ := ph.ContentType()
			text, _ := io.ReadAll(io.LimitReader(p.Body, maxFetch))
			if kind == "text/html" && rich == "" {
				rich = string(text)
			} else if strings.HasPrefix(kind, "text/") && plain == "" {
				plain = string(text)
			}
		case *mail.AttachmentHeader:
			name, _ := ph.Filename()
			kind, _, _ := ph.ContentType()
			l.attachments = append(l.attachments, map[string]any{"name": name, "type": kind})
		}
	}
	l.body = strings.TrimSpace(strings.ReplaceAll(plain, "\r\n", "\n"))
	if l.body == "" && rich != "" {
		l.body = textOf(rich)
	}
	if r := []rune(l.body); len(r) > maxBody {
		l.body = string(r[:maxBody]) + "\n…（正文太长，后面的省略了）"
	}
	return l, nil
}

// textOf gives the words of an HTML message, a line for each block.
func textOf(doc string) string {
	root, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		return ""
	}
	var b strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "head":
				return
			case "br", "p", "div", "tr", "li", "h1", "h2", "h3", "h4", "blockquote":
				b.WriteString("\n")
			}
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	var lines []string
	for _, line := range strings.Split(b.String(), "\n") {
		if line = strings.Join(strings.Fields(line), " "); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// event is a letter as a rule sees it: a message from its sender, who is
// also the conversation, since that is who an answer goes to.
func (l *letter) event() cat.Event {
	from := &mail.Address{}
	if l.from != nil {
		from = l.from
	}
	addr := strings.ToLower(from.Address)
	text := l.body
	if l.subject != "" {
		text = strings.TrimSpace(l.subject + "\n\n" + l.body)
	}
	payload := map[string]any{
		"text": text, "subject": l.subject, "body": l.body, "from": addr, "to": l.to, "message_id": l.id,
		"uid": strconv.FormatUint(uint64(l.uid), 10),
		// mail is addressed to its reader, as a private message is
		"mentioned": true,
	}
	if len(l.cc) > 0 {
		payload["cc"] = l.cc
	}
	if l.replyTo != nil {
		payload["reply_to"] = strings.ToLower(l.replyTo.Address)
	}
	if len(l.inReplyTo) > 0 {
		payload["in_reply_to"] = l.inReplyTo[0]
	}
	if len(l.references) > 0 {
		payload["references"] = l.references
	}
	if l.attachments != nil {
		payload["attachments"] = l.attachments
	}
	if l.automatic {
		payload["automatic"] = true
	}
	ev := cat.Event{
		Type:         "message.created",
		Conversation: &cat.Conversation{ID: addr, Kind: "private", Name: from.Name},
		Actor:        &cat.Actor{ID: addr, Name: from.Name},
		Payload:      payload,
	}
	if !l.date.IsZero() {
		ev.Time = &l.date
	}
	return ev
}

// fetch reads the messages with a UID above last, oldest first.
func fetch(c *imapclient.Client, last imap.UID) ([]*letter, error) {
	section := &imap.FetchItemBodySection{Peek: true, Partial: &imap.SectionPartial{Offset: 0, Size: maxFetch}}
	msgs, err := c.Fetch(imap.UIDSet{imap.UIDRange{Start: last + 1, Stop: 0}},
		&imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}}).Collect()
	if err != nil {
		return nil, err
	}
	var out []*letter
	for _, m := range msgs {
		if m.UID <= last { // "n:*" always gives the newest message, also when it is older than n
			continue
		}
		raw := m.FindBodySection(section)
		if len(raw) == 0 && len(m.BodySection) > 0 {
			raw = m.BodySection[0].Bytes
		}
		l, err := read(raw)
		if err != nil {
			l = &letter{noBody: true}
		}
		l.uid = m.UID
		out = append(out, l)
	}
	return out, nil
}

// watch looks for new mail every so often and emits it. What was in the
// mailbox before the first look is left alone.
func (a *account) watch(ctx context.Context, h *cat.Host) {
	healthy := true
	for {
		err := a.check(h)
		if ctx.Err() != nil {
			return
		}
		if err != nil && healthy {
			h.SetStatus("error", "收信出错，稍后重试："+err.Error())
		} else if err == nil && !healthy {
			h.SetStatus("running", "")
		}
		healthy = err == nil
		select {
		case <-ctx.Done():
			return
		case <-time.After(a.every):
		}
	}
}

// check looks once. Where it got to is kept as "<uidvalidity>:<uid>": UIDs
// only mean something while the mailbox's validity stays the same.
func (a *account) check(h *cat.Host) error {
	c, box, err := a.open()
	if err != nil {
		return err
	}
	defer c.Close()
	defer c.Logout()
	// a server that does not say which UID comes next is asked every time
	newest := imap.UID(1<<32 - 1)
	if box.UIDNext > 0 {
		newest = box.UIDNext - 1
	}
	validity, last := uint32(0), imap.UID(0)
	if seen, ok := h.StateGet("seen"); ok {
		v, u, _ := strings.Cut(seen, ":")
		n, _ := strconv.ParseUint(v, 10, 32)
		m, _ := strconv.ParseUint(u, 10, 32)
		validity, last = uint32(n), imap.UID(m)
	}
	save := func(uid imap.UID) error {
		return h.StateSet("seen", fmt.Sprintf("%d:%d", box.UIDValidity, uid))
	}
	if validity != box.UIDValidity { // the first look, or the mailbox was rebuilt
		if box.UIDNext == 0 {
			newest = 0
			if box.NumMessages > 0 {
				msgs, err := c.Fetch(imap.SeqSetNum(box.NumMessages), &imap.FetchOptions{UID: true}).Collect()
				if err != nil || len(msgs) == 0 {
					return fmt.Errorf("读不出邮箱里最新的一封：%v", err)
				}
				newest = msgs[0].UID
			}
		}
		return save(newest)
	}
	if newest <= last {
		return nil
	}
	letters, err := fetch(c, last)
	if err != nil {
		return fmt.Errorf("读不出新邮件：%w", err)
	}
	for _, l := range letters {
		if !l.ours && !l.noBody && l.from != nil && strings.ToLower(l.from.Address) != "" {
			if _, err := h.Emit(l.event()); err != nil {
				return err
			}
		}
		if err := save(l.uid); err != nil {
			return err
		}
	}
	return nil
}

func text(p map[string]any, key string) string {
	switch v := p[key].(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}

// list splits addresses typed with commas, semicolons or spaces between.
func list(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '，' || r == ';' || r == '；' || r == ' ' || r == '\n' }) {
		if strings.Contains(f, "@") {
			out = append(out, strings.ToLower(f))
		}
	}
	return out
}

func strs(v any) []string {
	var out []string
	if items, ok := v.([]any); ok {
		for _, it := range items {
			if s, ok := it.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// New builds the plugin.
func New() cat.Plugin {
	var a *account
	return cat.Plugin{
		Start: func(ctx context.Context, h *cat.Host) error {
			made, err := newAccount(h.Config)
			if err != nil {
				return err
			}
			if err := made.check(h); err != nil {
				return err
			}
			a = made
			h.Log("info", "已连接 %s", a.address)
			go func() {
				select {
				case <-ctx.Done():
				case <-time.After(a.every):
					a.watch(ctx, h)
				}
			}()
			return nil
		},
		Actions: map[string]cat.ActionFunc{
			"reply": func(ctx context.Context, h *cat.Host, p map[string]any, actx cat.ActionContext) (any, error) {
				ev := actx.Event
				if ev == nil || ev.Conversation == nil || !strings.Contains(ev.Conversation.ID, "@") {
					return nil, errors.New("「回复」需要由一封邮件触发；要主动发邮件请用「发一封邮件」")
				}
				to := []string{ev.Conversation.ID}
				if r, _ := ev.Payload["reply_to"].(string); r != "" {
					to = []string{r}
				}
				var cc []string
				if all, _ := p["reply_all"].(bool); all {
					for _, addr := range append(strs(ev.Payload["to"]), strs(ev.Payload["cc"])...) {
						if addr != a.address && addr != to[0] {
							cc = append(cc, addr)
						}
					}
				}
				subject := strings.TrimSpace(text(p, "subject"))
				if subject == "" {
					subject, _ = ev.Payload["subject"].(string)
					if !strings.HasPrefix(strings.ToLower(subject), "re:") {
						subject = "Re: " + subject
					}
				}
				id, _ := ev.Payload["message_id"].(string)
				return a.send(ctx, outgoing{to: to, cc: cc, subject: subject, body: text(p, "text"),
					inReplyTo: id, references: append(strs(ev.Payload["references"]), id)})
			},
			"send": func(ctx context.Context, h *cat.Host, p map[string]any, _ cat.ActionContext) (any, error) {
				to := list(text(p, "to"))
				if len(to) == 0 {
					return nil, errors.New("缺少收件人的邮箱地址")
				}
				return a.send(ctx, outgoing{to: to, subject: strings.TrimSpace(text(p, "subject")), body: text(p, "text")})
			},
			"typing": func(context.Context, *cat.Host, map[string]any, cat.ActionContext) (any, error) {
				return map[string]any{}, nil
			},
		},
		Setup: setup,
	}
}
