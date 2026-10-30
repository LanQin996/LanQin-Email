package app

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	netmail "net/mail"
	"path/filepath"
	"testing"
	"time"
)

func TestBuildMIMEAttachmentFilenameQuotedPairsRoundTrip(t *testing.T) {
	for _, filename := range []string{`report "final" \2026.txt`, `report "final" \\2026.txt`, `report "final".txt`, `notes\sample.txt`, `附件「最终版」.txt`, "payload.txt"} {
		t.Run(filename, func(t *testing.T) {
			content := []byte{0, 1, 2, 0xff, 's', 'y', 'n', 't', 'h', 'e', 't', 'i', 'c'}
			raw, err := BuildMIME(MIMEMessage{From: "sender@example.test", To: []string{"recipient@example.test"}, Subject: "Synthetic quoted filename",
				Text: "Synthetic body", Date: time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC), MessageID: "<filename@example.test>",
				Attachments: []AttachmentInput{{Filename: filename, ContentType: "application/octet-stream", decodedContent: content, contentDecoded: true}}})
			if err != nil {
				t.Fatal(err)
			}
			assertBackupMIMEFilename(t, raw, filename)
			parser := App{policy: NewHTMLPolicy(), now: time.Now}
			message, attachments, err := parser.parseMaildirMessage(raw, "unused@example.test")
			if err != nil || message.Subject != "Synthetic quoted filename" || message.BodyText != "Synthetic body" || len(attachments) != 1 {
				t.Fatal("MIME parser failed after quoted-pair encoding", err)
			}
			// Incoming MIME parsing intentionally applies the platform's basename
			// rule. Do not weaken that path-sanitization rule for Windows tests.
			if attachments[0].Filename != filepath.Base(filename) {
				t.Fatalf("attachment filename changed: got %q want %q", attachments[0].Filename, filepath.Base(filename))
			}
			actual, err := attachments[0].contentBytes()
			if err != nil || !bytes.Equal(actual, content) {
				t.Fatal("quoted filename changed attachment bytes", err)
			}
		})
	}
}

// Inspect both raw MIME parameters before the application's intentional basename
// sanitization. This proves full metadata survives even when Windows treats a
// backslash in an incoming filename as a path separator.
func assertBackupMIMEFilename(t *testing.T, raw []byte, expected string) {
	t.Helper()
	message, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
	if err != nil || params["boundary"] == "" {
		t.Fatal("top-level MIME boundary missing", err)
	}
	reader := multipart.NewReader(message.Body, params["boundary"])
	count := 0
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if part.Header.Get("Content-Disposition") != "" {
			count++
			for _, field := range []struct{ header, parameter string }{{"Content-Disposition", "filename"}, {"Content-Type", "name"}} {
				_, values, err := mime.ParseMediaType(part.Header.Get(field.header))
				if err != nil || decodeMIMEHeader(values[field.parameter]) != expected {
					t.Fatalf("%s lost original filename: got %q want %q error=%v", field.header, values[field.parameter], expected, err)
				}
			}
		}
		part.Close()
	}
	if count != 1 {
		t.Fatalf("expected one attachment, got %d", count)
	}
}
