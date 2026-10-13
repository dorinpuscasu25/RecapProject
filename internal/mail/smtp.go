package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

// SMTP sends mail via an SMTP relay with STARTTLS (required unless explicitly disabled for a
// local relay such as Mailpit).
type SMTP struct {
	Host     string
	Port     int
	User     string
	Password string
	From     string
	StartTLS bool
}

func (s *SMTP) Name() string { return "smtp" }

func (s *SMTP) Send(ctx context.Context, m Message) (string, error) {
	from, err := mail.ParseAddress(s.From)
	if err != nil {
		return "", &PermanentError{Err: fmt.Errorf("invalid MAIL_FROM: %w", err)}
	}
	to, err := mail.ParseAddress(m.To)
	if err != nil {
		return "", &PermanentError{Err: fmt.Errorf("invalid recipient: %w", err)}
	}
	addr := net.JoinHostPort(s.Host, fmt.Sprint(s.Port))
	d := net.Dialer{Timeout: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(60 * time.Second)
	}
	_ = conn.SetDeadline(deadline)
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return "", err
	}
	defer c.Close()
	if s.StartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return "", errors.New("smtp server does not support STARTTLS")
		}
		if err := c.StartTLS(&tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return "", err
		}
	}
	if s.User != "" {
		if err := c.Auth(smtp.PlainAuth("", s.User, s.Password, s.Host)); err != nil {
			return "", classify(err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return "", classify(err)
	}
	if err := c.Rcpt(to.Address); err != nil {
		return "", classify(err)
	}
	w, err := c.Data()
	if err != nil {
		return "", classify(err)
	}
	msgID := fmt.Sprintf("<%s@%s>", m.IdempotencyKey, hostOf(from.Address))
	var b strings.Builder
	b.WriteString("From: " + from.String() + "\r\n")
	b.WriteString("To: " + to.String() + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", m.Subject) + "\r\n")
	b.WriteString("Message-ID: " + msgID + "\r\n")
	b.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n")
	b.WriteString("Auto-Submitted: auto-generated\r\n\r\n")
	b.WriteString(strings.ReplaceAll(m.Text, "\n", "\r\n"))
	if _, err := w.Write([]byte(b.String())); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", classify(err)
	}
	_ = c.Quit()
	return msgID, nil
}

func hostOf(addr string) string {
	_, h, _ := strings.Cut(addr, "@")
	return h
}

// classify marks 5xx SMTP replies as permanent.
func classify(err error) error {
	var te *textproto.Error
	if errors.As(err, &te) && te.Code >= 500 {
		return &PermanentError{Err: err}
	}
	return err
}
