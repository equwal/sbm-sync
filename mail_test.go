package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/textproto"
	"strings"
	"sync"
	"testing"
)

// fakeSMTP plays an SMTP server and records what the client sends. With
// tls set, it offers STARTTLS, or it uses TLS from the start when implicit
// is set.
type fakeSMTP struct {
	ln       net.Listener
	tls      *tls.Config
	implicit bool

	mu       sync.Mutex
	auth     string // "user password" of AUTH PLAIN
	authTLS  bool   // the connection had TLS at AUTH
	from, to string // arguments of MAIL and RCPT
	data     string
}

func newFakeSMTP(t *testing.T, tc *tls.Config, implicit bool) *fakeSMTP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeSMTP{ln: ln, tls: tc, implicit: implicit}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.session(conn)
		}
	}()
	return f
}

func (f *fakeSMTP) session(conn net.Conn) {
	defer func() { conn.Close() }()
	secure := f.implicit
	if secure {
		conn = tls.Server(conn, f.tls)
	}
	tp := textproto.NewConn(conn)
	tp.PrintfLine("220 fake ESMTP")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		f.mu.Lock()
		switch strings.ToUpper(verb) {
		case "EHLO":
			tp.PrintfLine("250-fake")
			if f.tls != nil && !secure {
				tp.PrintfLine("250-STARTTLS")
			}
			tp.PrintfLine("250 AUTH PLAIN")
		case "STARTTLS":
			tp.PrintfLine("220 go ahead")
			conn = tls.Server(conn, f.tls)
			tp = textproto.NewConn(conn)
			secure = true
		case "AUTH":
			_, resp, _ := strings.Cut(arg, " ")
			b, _ := base64.StdEncoding.DecodeString(resp)
			f.auth = strings.TrimPrefix(strings.ReplaceAll(string(b), "\x00", " "), " ")
			f.authTLS = secure
			tp.PrintfLine("235 ok")
		case "MAIL":
			f.from = arg
			tp.PrintfLine("250 ok")
		case "RCPT":
			f.to = arg
			tp.PrintfLine("250 ok")
		case "DATA":
			tp.PrintfLine("354 go on")
			b, _ := tp.ReadDotBytes()
			f.data = string(b)
			tp.PrintfLine("250 ok")
		case "QUIT":
			tp.PrintfLine("221 bye")
			f.mu.Unlock()
			return
		default:
			tp.PrintfLine("502 no such command")
		}
		f.mu.Unlock()
	}
}

// testTLS gives a certificate for 127.0.0.1 and a pool that trusts it.
func testTLS(t *testing.T) (*tls.Config, *x509.CertPool) {
	ts := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(ts.Close)
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	return &tls.Config{Certificates: ts.TLS.Certificates}, pool
}

func testMailer(f *fakeSMTP) *Mailer {
	return &Mailer{Addr: f.ln.Addr().String(), Hello: "localhost",
		From: &mail.Address{Name: "sbm Sync", Address: "sync@example.org"}}
}

func TestMailerSends(t *testing.T) {
	f := newFakeSMTP(t, nil, false)
	if err := testMailer(f).send("you@example.org", "Hello", "line 1\n.line 2\n"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.from != "FROM:<sync@example.org>" || f.to != "TO:<you@example.org>" {
		t.Errorf("envelope: %q %q", f.from, f.to)
	}
	for _, want := range []string{
		"From: \"sbm Sync\" <sync@example.org>\n",
		"To: you@example.org\n",
		"Subject: Hello\n",
		"Message-ID: <",
		"@example.org>\n",
		"Content-Type: text/plain; charset=utf-8\n",
		"\n\nline 1\n.line 2\n",
	} {
		if !strings.Contains(f.data, want) {
			t.Errorf("the email lacks %q:\n%s", want, f.data)
		}
	}
}

func TestMailerStartTLS(t *testing.T) {
	tc, pool := testTLS(t)
	f := newFakeSMTP(t, tc, false)
	m := testMailer(f)
	m.User, m.Password, m.Roots = "me", "secret", pool
	if err := m.send("you@example.org", "Hello", "hi\n"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.auth != "me secret" || !f.authTLS {
		t.Errorf("auth %q, with TLS %v", f.auth, f.authTLS)
	}
}

func TestMailerTLSFromTheStart(t *testing.T) {
	tc, pool := testTLS(t)
	f := newFakeSMTP(t, tc, true)
	m := testMailer(f)
	m.TLS, m.User, m.Password, m.Roots = true, "me", "secret", pool
	if err := m.send("you@example.org", "Hello", "hi\n"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.auth != "me secret" || !f.authTLS || !strings.Contains(f.data, "hi\n") {
		t.Errorf("auth %q, with TLS %v, data %q", f.auth, f.authTLS, f.data)
	}
}

// A certificate that the system does not trust stops the email before the
// password goes out.
func TestMailerChecksTheCertificate(t *testing.T) {
	tc, _ := testTLS(t)
	f := newFakeSMTP(t, tc, false)
	m := testMailer(f)
	m.User, m.Password = "me", "secret"
	if err := m.send("you@example.org", "Hello", "hi\n"); err == nil {
		t.Error("the mailer trusts any certificate")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.auth != "" {
		t.Errorf("the password went out: %q", f.auth)
	}
}
