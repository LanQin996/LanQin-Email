package app

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	netmail "net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

const smtpSessionTimeout = 45 * time.Second

type MIMEMessage struct {
	From        string
	FromName    string
	To          []string
	CC          []string
	BCC         []string
	Subject     string
	Text        string
	HTML        string
	MessageID   string
	Date        time.Time
	Attachments []AttachmentInput
}

func BuildMIME(m MIMEMessage) ([]byte, error) {
	var buf bytes.Buffer
	writeHeader := func(k, v string) {
		if strings.TrimSpace(v) == "" {
			return
		}
		// Defence in depth: recipients are validated upstream by dedupeEmails, but a
		// header value that still carries CR or LF would split into extra headers.
		// Folding whitespace is not produced here, so any such byte is an injection.
		v = strings.NewReplacer("\r", " ", "\n", " ").Replace(v)
		fmt.Fprintf(&buf, "%s: %s\r\n", k, v)
	}
	writeHeader("From", formatAddressHeader(m.FromName, m.From))
	writeHeader("To", strings.Join(m.To, ", "))
	writeHeader("Cc", strings.Join(m.CC, ", "))
	writeHeader("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	writeHeader("Message-ID", m.MessageID)
	writeHeader("Date", m.Date.Format(time.RFC1123Z))
	writeHeader("MIME-Version", "1.0")

	mixed := multipart.NewWriter(&buf)
	writeHeader("Content-Type", `multipart/mixed; boundary="`+mixed.Boundary()+`"`)
	buf.WriteString("\r\n")

	altBoundary := multipart.NewWriter(io.Discard).Boundary()
	altMixedHeader := textprotoMIMEHeader(map[string]string{"Content-Type": `multipart/alternative; boundary="` + altBoundary + `"`})
	altMixedPart, err := mixed.CreatePart(altMixedHeader)
	if err != nil {
		return nil, err
	}
	alt := multipart.NewWriter(altMixedPart)
	if err := alt.SetBoundary(altBoundary); err != nil {
		return nil, err
	}
	textHeader := textprotoMIMEHeader(map[string]string{"Content-Type": `text/plain; charset="utf-8"`, "Content-Transfer-Encoding": "base64"})
	textPart, err := alt.CreatePart(textHeader)
	if err != nil {
		return nil, err
	}
	if err := writeBase64(textPart, strings.NewReader(m.Text)); err != nil {
		return nil, err
	}
	htmlHeader := textprotoMIMEHeader(map[string]string{"Content-Type": `text/html; charset="utf-8"`, "Content-Transfer-Encoding": "base64"})
	htmlPart, err := alt.CreatePart(htmlHeader)
	if err != nil {
		return nil, err
	}
	if err := writeBase64(htmlPart, strings.NewReader(m.HTML)); err != nil {
		return nil, err
	}
	if err := alt.Close(); err != nil {
		return nil, err
	}

	for i := range m.Attachments {
		att := &m.Attachments[i]
		data, err := att.contentBytes()
		if err != nil {
			return nil, err
		}
		contentType := att.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		filename := mime.QEncoding.Encode("utf-8", att.Filename)
		// QEncoding leaves printable ASCII unchanged. Escape quoted-string
		// delimiters so a legitimate quote/backslash remains part of the name.
		filename = strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(filename)
		h := textprotoMIMEHeader(map[string]string{
			"Content-Type":              contentType + `; name="` + filename + `"`,
			"Content-Disposition":       `attachment; filename="` + filename + `"`,
			"Content-Transfer-Encoding": "base64",
		})
		part, err := mixed.CreatePart(h)
		if err != nil {
			return nil, err
		}
		if err := writeBase64(part, bytes.NewReader(data)); err != nil {
			return nil, err
		}
	}
	if err := mixed.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func formatAddressHeader(name, address string) string {
	address = strings.TrimSpace(address)
	name = strings.TrimSpace(name)
	if address == "" || name == "" || strings.EqualFold(name, address) {
		return address
	}
	return (&netmail.Address{Name: name, Address: address}).String()
}

func textprotoMIMEHeader(values map[string]string) textproto.MIMEHeader {
	h := textproto.MIMEHeader{}
	for k, v := range values {
		h.Set(k, v)
	}
	return h
}

func writeBase64(w io.Writer, r io.Reader) error {
	lineWriter := &base64LineWriter{w: w}
	encoder := base64.NewEncoder(base64.StdEncoding, lineWriter)
	if _, err := io.Copy(encoder, r); err != nil {
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	return lineWriter.close()
}

type base64LineWriter struct {
	w       io.Writer
	lineLen int
	wrote   bool
}

func (w *base64LineWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if w.lineLen == 76 {
			if _, err := io.WriteString(w.w, "\r\n"); err != nil {
				return written, err
			}
			w.lineLen = 0
		}
		nextLineLen := min(len(p), 76-w.lineLen)
		n, err := w.w.Write(p[:nextLineLen])
		w.lineLen += n
		written += n
		p = p[n:]
		w.wrote = w.wrote || n > 0
		if err != nil {
			return written, err
		}
		if n != nextLineLen {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func (w *base64LineWriter) close() error {
	if w.lineLen == 0 && w.wrote {
		return nil
	}
	_, err := io.WriteString(w.w, "\r\n")
	return err
}

func (a *App) sendSMTP(from string, recipients []string, mimeBytes []byte) error {
	return sendSMTPWithConfig(a.cfg, from, recipients, mimeBytes)
}

func sendSMTPWithConfig(cfg Config, from string, recipients []string, mimeBytes []byte) error {
	addr := net.JoinHostPort(cfg.SMTPHost, cfg.SMTPPort)
	var auth smtp.Auth
	if cfg.SMTPUsername != "" {
		auth = smtp.PlainAuth("", cfg.SMTPUsername, cfg.SMTPPassword, cfg.SMTPHost)
	}
	if !cfg.SMTPRequireTLS {
		return sendSMTPPlain(addr, cfg.SMTPHost, auth, from, recipients, mimeBytes)
	}
	if cfg.SMTPPort == "465" {
		return sendSMTPImplicitTLS(addr, cfg.SMTPHost, auth, from, recipients, mimeBytes)
	}
	return sendSMTPStartTLS(addr, cfg.SMTPHost, auth, from, recipients, mimeBytes)
}

func sendSMTPPlain(addr, host string, auth smtp.Auth, from string, recipients []string, mimeBytes []byte) error {
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(smtpSessionTimeout))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer client.Close()
	return sendSMTPMessage(client, auth, from, recipients, mimeBytes)
}

func sendSMTPImplicitTLS(addr, host string, auth smtp.Auth, from string, recipients []string, mimeBytes []byte) error {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(smtpSessionTimeout))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer client.Close()
	return sendSMTPMessage(client, auth, from, recipients, mimeBytes)
}

func sendSMTPStartTLS(addr, host string, auth smtp.Auth, from string, recipients []string, mimeBytes []byte) error {
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(smtpSessionTimeout))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer client.Close()
	if ok, _ := client.Extension("STARTTLS"); !ok {
		return errors.New("smtp server does not support STARTTLS")
	}
	if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
		return err
	}
	return sendSMTPMessage(client, auth, from, recipients, mimeBytes)
}

func sendSMTPMessage(client *smtp.Client, auth smtp.Auth, from string, recipients []string, mimeBytes []byte) error {
	if auth != nil {
		if err := client.Auth(auth); err != nil {
			return err
		}
	}
	if err := client.Mail(from); err != nil {
		return err
	}
	for _, rcpt := range recipients {
		if err := client.Rcpt(rcpt); err != nil {
			return err
		}
	}
	wc, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := wc.Write(mimeBytes); err != nil {
		_ = wc.Close()
		return err
	}
	if err := wc.Close(); err != nil {
		return err
	}
	return client.Quit()
}

func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\n", "<br>")
	return s
}
