// Package mailtest is a mail server for one mailbox, kept in memory: an IMAP
// server to read it, and an SMTP server that puts whatever it is given into
// its INBOX, whoever it is addressed to. It stands in for a mail provider
// when the email plugin is tested, in tests and by hand (cmd/mailtest).
// Neither side is encrypted.
package mailtest

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

type Server struct {
	// IMAP and SMTP are the addresses the two servers listen on.
	IMAP, SMTP string

	user     *imapmemserver.User
	imap     *imapserver.Server
	smtp     net.Listener
	password string

	mu   sync.Mutex
	sent []Mail
}

// Mail is one message the SMTP server took.
type Mail struct {
	From string
	To   []string
	Data string
}

// Start serves a mailbox for username and password at the given addresses;
// "127.0.0.1:0" picks free ports.
func Start(imapAddr, smtpAddr, username, password string) (*Server, error) {
	s := &Server{user: imapmemserver.NewUser(username, password), password: password}
	if err := s.user.Create("INBOX", nil); err != nil {
		return nil, err
	}
	mem := imapmemserver.New()
	mem.AddUser(s.user)
	s.imap = imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
		InsecureAuth: true,
		Logger:       quiet{},
	})
	ln, err := net.Listen("tcp", imapAddr)
	if err != nil {
		return nil, err
	}
	s.IMAP = ln.Addr().String()
	go s.imap.Serve(ln)
	if s.smtp, err = net.Listen("tcp", smtpAddr); err != nil {
		s.imap.Close()
		return nil, err
	}
	s.SMTP = s.smtp.Addr().String()
	go func() {
		for {
			conn, err := s.smtp.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s, nil
}

func (s *Server) Close() {
	s.imap.Close()
	s.smtp.Close()
}

type quiet struct{}

func (quiet) Printf(string, ...interface{}) {}

type literal struct{ *bytes.Reader }

func (l literal) Size() int64 { return l.Reader.Size() }

// Deliver puts a message into the INBOX, as if it had come in from outside.
func (s *Server) Deliver(raw string) error {
	raw = strings.ReplaceAll(strings.ReplaceAll(raw, "\r\n", "\n"), "\n", "\r\n")
	_, err := s.user.Append("INBOX", literal{bytes.NewReader([]byte(raw))}, &imap.AppendOptions{})
	return err
}

// Sent lists what the SMTP server took, oldest first.
func (s *Server) Sent() []Mail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Mail(nil), s.sent...)
}

// serve speaks just enough SMTP for a client that signs in and sends.
func (s *Server) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	say := func(format string, args ...any) { fmt.Fprintf(conn, format+"\r\n", args...) }
	say("220 mailtest ready")
	var m Mail
	authed := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO", "HELO":
			say("250-mailtest")
			say("250 AUTH PLAIN")
		case "AUTH": // PLAIN, with the credentials on the same line
			authed = strings.HasPrefix(strings.ToUpper(arg), "PLAIN ") && plainPassword(arg[6:]) == s.password
			if !authed {
				say("535 authentication failed")
				continue
			}
			say("235 ok")
		case "MAIL":
			if !authed {
				say("530 authentication required")
				continue
			}
			m = Mail{From: between(arg)}
			say("250 ok")
		case "RCPT":
			m.To = append(m.To, between(arg))
			say("250 ok")
		case "DATA":
			say("354 go ahead")
			var data strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				data.WriteString(strings.TrimPrefix(l, "."))
			}
			m.Data = data.String()
			s.mu.Lock()
			s.sent = append(s.sent, m)
			s.mu.Unlock()
			if err := s.Deliver(m.Data); err != nil {
				say("451 %v", err)
				continue
			}
			say("250 ok")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

// between gives what stands in angle brackets.
func between(s string) string {
	_, s, _ = strings.Cut(s, "<")
	s, _, _ = strings.Cut(s, ">")
	return s
}

// plainPassword reads the password out of an AUTH PLAIN argument.
func plainPassword(b64 string) string {
	raw, err := io.ReadAll(newBase64(b64))
	if err != nil {
		return ""
	}
	parts := bytes.Split(raw, []byte{0})
	if len(parts) != 3 {
		return ""
	}
	return string(parts[2])
}
