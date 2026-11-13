package main

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"sync"
	"time"
)

type SMTPConfig struct {
	Host     string
	Port     string
	Username string
	Password string
	From     string
}

// Mailer sends plain-text notification emails. Every send is throttled per
// key so a burst of chat lines becomes one email.
type Mailer struct {
	cfg  SMTPConfig
	mu   sync.Mutex
	last map[string]time.Time
	// send is swappable for tests.
	send func(to, subject, body string) error
}

const mailThrottle = 5 * time.Minute

func NewMailer(cfg SMTPConfig) *Mailer {
	m := &Mailer{cfg: cfg, last: map[string]time.Time{}}
	m.send = m.smtpSend
	return m
}

func (m *Mailer) Enabled() bool { return m.cfg.Host != "" && m.cfg.From != "" }

// Notify sends in the background unless the same key was emailed recently.
func (m *Mailer) Notify(key, to, subject, body string) {
	if !m.Enabled() || to == "" {
		return
	}
	m.mu.Lock()
	if t, ok := m.last[key]; ok && time.Since(t) < mailThrottle {
		m.mu.Unlock()
		return
	}
	m.last[key] = time.Now()
	for k, t := range m.last {
		if time.Since(t) > time.Hour {
			delete(m.last, k)
		}
	}
	m.mu.Unlock()
	go func() {
		if err := m.send(to, subject, body); err != nil {
			slog.Error("email failed", "to", to, "err", err)
		} else {
			slog.Info("email sent", "to", to, "subject", subject)
		}
	}()
}

func headerSafe(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

func (m *Mailer) smtpSend(to, subject, body string) error {
	from := headerSafe(m.cfg.From)
	to = headerSafe(to)
	var msg strings.Builder
	fmt.Fprintf(&msg, "From: %s\r\n", from)
	fmt.Fprintf(&msg, "To: %s\r\n", to)
	fmt.Fprintf(&msg, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", headerSafe(subject)))
	fmt.Fprintf(&msg, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&msg, "Message-ID: <%s@%s>\r\n", newToken()[:24], m.cfg.Host)
	msg.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	msg.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))

	addr := net.JoinHostPort(m.cfg.Host, m.cfg.Port)
	var auth smtp.Auth
	if m.cfg.Username != "" {
		auth = smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)
	}
	envelopeFrom := from
	if i := strings.LastIndex(from, "<"); i >= 0 {
		envelopeFrom = strings.Trim(from[i:], "<> ")
	}
	if m.cfg.Port != "465" {
		// SendMail upgrades with STARTTLS when the server offers it.
		return smtp.SendMail(addr, auth, envelopeFrom, []string{to}, []byte(msg.String()))
	}
	// Port 465: implicit TLS.
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 15 * time.Second}, "tcp", addr, &tls.Config{ServerName: m.cfg.Host})
	if err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, m.cfg.Host)
	if err != nil {
		return err
	}
	defer c.Close()
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return err
		}
	}
	if err := c.Mail(envelopeFrom); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	wc, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := wc.Write([]byte(msg.String())); err != nil {
		return err
	}
	if err := wc.Close(); err != nil {
		return err
	}
	return c.Quit()
}
