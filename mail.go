package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// Mailer sends email through an SMTP server. With TLS set (port 465), the
// connection uses TLS from the start. Else the mailer starts TLS when the
// server offers STARTTLS. net/smtp never sends the password over a
// connection without TLS, except to localhost.
type Mailer struct {
	Addr     string // host:port of the SMTP server
	TLS      bool
	User     string // empty for a server that needs no sign-in
	Password string
	From     *mail.Address
	Hello    string         // name of this host in EHLO
	Roots    *x509.CertPool // nil for the roots of the system; tests replace it
}

// send sends a plain text email. The lines of body end in "\n".
func (m *Mailer) send(to, subject, body string) error {
	host, _, err := net.SplitHostPort(m.Addr)
	if err != nil {
		return err
	}
	tc := &tls.Config{ServerName: host, RootCAs: m.Roots}
	d := &net.Dialer{Timeout: 20 * time.Second}
	var conn net.Conn
	if m.TLS {
		conn, err = tls.DialWithDialer(d, "tcp", m.Addr, tc)
	} else {
		conn, err = d.Dial("tcp", m.Addr)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	if err := c.Hello(m.Hello); err != nil {
		return err
	}
	if ok, _ := c.Extension("STARTTLS"); ok && !m.TLS {
		if err := c.StartTLS(tc); err != nil {
			return err
		}
	}
	if m.User != "" {
		if err := c.Auth(smtp.PlainAuth("", m.User, m.Password, host)); err != nil {
			return err
		}
	}
	if err := c.Mail(m.From.Address); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(m.message(to, subject, body)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// message gives the email with its header. The lines end in CRLF, as SMTP
// needs.
func (m *Mailer) message(to, subject, body string) []byte {
	_, domain, _ := strings.Cut(m.From.Address, "@")
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", m.From)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@%s>\r\n", random(16), domain)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	return []byte(b.String())
}
