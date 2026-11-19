package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/smtp"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"

	cat "github.com/dounx/cat/sdk/go"
)

// outgoing is a mail to send.
type outgoing struct {
	to, cc        []string
	subject, body string
	files         []cat.FileRef
	// set on a reply, so that mail programs keep it with what it answers
	inReplyTo  string
	references []string
}

// loginAuth is the LOGIN way of signing in, which some servers offer in
// place of PLAIN.
type loginAuth struct{ username, password string }

func (a loginAuth) Start(*smtp.ServerInfo) (string, []byte, error) { return "LOGIN", nil, nil }
func (a loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	if strings.Contains(strings.ToLower(string(fromServer)), "user") {
		return []byte(a.username), nil
	}
	return []byte(a.password), nil
}

// dial connects to the SMTP server and signs in.
func (a *account) dial(ctx context.Context) (*smtp.Client, error) {
	host, port, _ := net.SplitHostPort(a.smtp)
	d := &net.Dialer{Timeout: 20 * time.Second}
	var conn net.Conn
	var err error
	if port == "465" && !a.insecure {
		conn, err = tls.DialWithDialer(d, "tcp", a.smtp, &tls.Config{ServerName: host})
	} else {
		conn, err = d.DialContext(ctx, "tcp", a.smtp)
	}
	if err != nil {
		return nil, fmt.Errorf("连不上 SMTP 服务器 %s：%w", a.smtp, err)
	}
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP 服务器 %s 没有按规矩应答：%w", a.smtp, err)
	}
	if port != "465" && !a.insecure {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			c.Close()
			return nil, errors.New("SMTP 服务器不支持加密（STARTTLS），没有继续")
		}
		if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
			c.Close()
			return nil, fmt.Errorf("SMTP 加密握手失败：%w", err)
		}
	}
	if ok, mechs := c.Extension("AUTH"); ok {
		var auth smtp.Auth = smtp.PlainAuth("", a.username, a.password, host)
		if !strings.Contains(mechs, "PLAIN") && strings.Contains(mechs, "LOGIN") {
			auth = loginAuth{a.username, a.password}
		}
		if err := c.Auth(auth); err != nil {
			c.Close()
			return nil, fmt.Errorf("SMTP 登录失败：%w。多数邮箱要先在设置里开启 SMTP，并用授权码代替登录密码", err)
		}
	}
	return c, nil
}

// messageID makes an id for a mail of ours, under the sender's domain.
func (a *account) messageID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	_, domain, _ := strings.Cut(a.address, "@")
	return hex.EncodeToString(b) + "@" + domain
}

// compose writes the mail out: plain text in UTF-8, base64 so that no line
// is too long and nothing in it is taken for a command.
func (a *account) compose(m outgoing, id string) []byte {
	var b bytes.Buffer
	line := func(key, value string) { fmt.Fprintf(&b, "%s: %s\r\n", key, value) }
	from := a.address
	if a.name != "" {
		from = mime.QEncoding.Encode("utf-8", a.name) + " <" + a.address + ">"
	}
	line("From", from)
	line("To", strings.Join(m.to, ", "))
	if len(m.cc) > 0 {
		line("Cc", strings.Join(m.cc, ", "))
	}
	line("Subject", mime.QEncoding.Encode("utf-8", m.subject))
	line("Date", time.Now().Format(time.RFC1123Z))
	line("Message-ID", "<"+id+">")
	if m.inReplyTo != "" {
		line("In-Reply-To", "<"+m.inReplyTo+">")
	}
	if refs := m.references; len(refs) > 0 {
		if len(refs) > 20 { // a long thread: the start of it and its latest
			refs = append(refs[:1:1], refs[len(refs)-19:]...)
		}
		line("References", "<"+strings.Join(refs, "> <")+">")
	}
	line(sentHeader, "1")
	line("MIME-Version", "1.0")
	if len(m.files) == 0 {
		line("Content-Type", "text/plain; charset=utf-8")
		line("Content-Transfer-Encoding", "base64")
		b.WriteString("\r\n")
		writeBase64Lines(&b, []byte(strings.ReplaceAll(strings.ReplaceAll(m.body, "\r\n", "\n"), "\n", "\r\n")))
		return b.Bytes()
	}
	mw := multipart.NewWriter(&b)
	line("Content-Type", fmt.Sprintf("multipart/mixed; boundary=%q", mw.Boundary()))
	b.WriteString("\r\n")
	if strings.TrimSpace(m.body) != "" {
		pw, _ := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {"text/plain; charset=utf-8"},
			"Content-Transfer-Encoding": {"base64"},
		})
		writeBase64Lines(pw, []byte(strings.ReplaceAll(strings.ReplaceAll(m.body, "\r\n", "\n"), "\n", "\r\n")))
	}
	for _, f := range m.files {
		raw, err := os.ReadFile(f.Path)
		if err != nil {
			continue
		}
		name := f.Name
		if name == "" {
			name = filepath.Base(f.Path)
		}
		ct := mime.TypeByExtension(filepath.Ext(name))
		if ct == "" {
			ct = http.DetectContentType(raw)
		}
		encName := mime.QEncoding.Encode("utf-8", name)
		pw, _ := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {ct + `; name="` + encName + `"`},
			"Content-Disposition":       {`attachment; filename="` + encName + `"`},
			"Content-Transfer-Encoding": {"base64"},
		})
		writeBase64Lines(pw, raw)
	}
	_ = mw.Close()
	return b.Bytes()
}

func writeBase64Lines(w interface{ Write([]byte) (int, error) }, data []byte) {
	enc := base64.StdEncoding.EncodeToString(data)
	for len(enc) > 76 {
		_, _ = w.Write([]byte(enc[:76] + "\r\n"))
		enc = enc[76:]
	}
	_, _ = w.Write([]byte(enc + "\r\n"))
}

// send delivers a mail and says under which id it went.
func (a *account) send(ctx context.Context, m outgoing) (any, error) {
	if strings.TrimSpace(m.body) == "" && len(m.files) == 0 {
		return nil, errors.New("正文和附件不能同时为空")
	}
	for _, f := range m.files {
		info, err := os.Stat(f.Path)
		if err != nil || info.IsDir() {
			return nil, fmt.Errorf("找不到附件文件：%s", f.Path)
		}
		if info.Size() > 15<<20 {
			name := f.Name
			if name == "" {
				name = filepath.Base(f.Path)
			}
			return nil, fmt.Errorf("附件 %s 超过 15 MB 限制", name)
		}
	}
	if len(m.to) == 0 {
		return nil, errors.New("缺少收件人")
	}
	c, err := a.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	id := a.messageID()
	if err := c.Mail(a.address); err != nil {
		return nil, fmt.Errorf("SMTP 服务器不接受发件人 %s：%w", a.address, err)
	}
	for _, to := range append(append([]string{}, m.to...), m.cc...) {
		if err := c.Rcpt(to); err != nil {
			return nil, fmt.Errorf("SMTP 服务器不接受收件人 %s：%w", to, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(a.compose(m, id)); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("邮件没有发出去：%w", err)
	}
	_ = c.Quit()
	return map[string]any{"message_id": id, "to": m.to, "subject": m.subject}, nil
}
