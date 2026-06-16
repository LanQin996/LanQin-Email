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
		if strings.TrimSpace(v) != "" {
			fmt.Fprintf(&buf, "%s: %s\r\n", k, v)
		}
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

	var altBuf bytes.Buffer
	alt := multipart.NewWriter(&altBuf)
	textHeader := textprotoMIMEHeader(map[string]string{"Content-Type": `text/plain; charset="utf-8"`, "Content-Transfer-Encoding": "base64"})
	textPart, err := alt.CreatePart(textHeader)
	if err != nil {
		return nil, err
	}
	writeBase64(textPart, []byte(m.Text))
	htmlHeader := textprotoMIMEHeader(map[string]string{"Content-Type": `text/html; charset="utf-8"`, "Content-Transfer-Encoding": "base64"})
	htmlPart, err := alt.CreatePart(htmlHeader)
	if err != nil {
		return nil, err
	}
	writeBase64(htmlPart, []byte(m.HTML))
	if err := alt.Close(); err != nil {
		return nil, err
	}

	altMixedHeader := textprotoMIMEHeader(map[string]string{"Content-Type": `multipart/alternative; boundary="` + alt.Boundary() + `"`})
	altMixedPart, err := mixed.CreatePart(altMixedHeader)
	if err != nil {
		return nil, err
	}
	if _, err := altMixedPart.Write(altBuf.Bytes()); err != nil {
		return nil, err
	}

	for _, att := range m.Attachments {
		data, err := base64.StdEncoding.DecodeString(att.ContentBase64)
		if err != nil {
			return nil, err
		}
		contentType := att.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		filename := mime.QEncoding.Encode("utf-8", att.Filename)
		h := textprotoMIMEHeader(map[string]string{
			"Content-Type":              contentType + `; name="` + filename + `"`,
			"Content-Disposition":       `attachment; filename="` + filename + `"`,
			"Content-Transfer-Encoding": "base64",
		})
		part, err := mixed.CreatePart(h)
		if err != nil {
			return nil, err
		}
		writeBase64(part, data)
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

func writeBase64(w io.Writer, data []byte) {
	encoded := make([]byte, base64.StdEncoding.EncodedLen(len(data)))
	base64.StdEncoding.Encode(encoded, data)
	for len(encoded) > 76 {
		_, _ = w.Write(encoded[:76])
		_, _ = w.Write([]byte("\r\n"))
		encoded = encoded[76:]
	}
	_, _ = w.Write(encoded)
	_, _ = w.Write([]byte("\r\n"))
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
