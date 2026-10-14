package app

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

func TestBuildMIMEWritesNestedAlternativeAndAttachment(t *testing.T) {
	const textBody = "synthetic text body"
	const htmlBody = "<p>synthetic html body</p>"
	const attachmentBody = "synthetic attachment bytes"
	raw, err := BuildMIME(MIMEMessage{
		From:    "sender@example.test",
		To:      []string{"recipient@example.test"},
		Subject: "synthetic message",
		Text:    textBody,
		HTML:    htmlBody,
		Date:    time.Unix(0, 0).UTC(),
		Attachments: []AttachmentInput{{
			Filename:      "synthetic.txt",
			ContentType:   "text/plain",
			ContentBase64: base64.StdEncoding.EncodeToString([]byte(attachmentBody)),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/mixed" {
		t.Fatalf("outer content type = %q, err=%v", mediaType, err)
	}
	mixed := multipart.NewReader(msg.Body, params["boundary"])
	alternative, err := mixed.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	mediaType, params, err = mime.ParseMediaType(alternative.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/alternative" {
		t.Fatalf("alternative content type = %q, err=%v", mediaType, err)
	}
	alt := multipart.NewReader(alternative, params["boundary"])
	for _, want := range []string{textBody, htmlBody} {
		part, err := alt.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, part))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("alternative body = %q, want %q", got, want)
		}
	}

	attachment, err := mixed.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	gotAttachment, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, attachment))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotAttachment) != attachmentBody {
		t.Fatalf("attachment body = %q, want %q", gotAttachment, attachmentBody)
	}
}

func TestParseMailPartKeepsDecodedAttachmentBytes(t *testing.T) {
	const payload = "synthetic attachment bytes"
	encoded := base64.StdEncoding.EncodeToString([]byte(payload))
	var parsed parsedMail
	header := textproto.MIMEHeader{
		"Content-Type":              {"application/octet-stream"},
		"Content-Disposition":       {`attachment; filename="synthetic.bin"`},
		"Content-Transfer-Encoding": {"base64"},
	}
	if err := parseMailPart(header, strings.NewReader(encoded), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(parsed.Attachments))
	}
	if parsed.Attachments[0].ContentBase64 != "" {
		t.Fatal("decoded MIME attachment was redundantly stored as base64")
	}
	got, err := parsed.Attachments[0].contentBytes()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Fatalf("attachment bytes = %q, want %q", got, payload)
	}
}

func TestWriteBase64PreservesMIMELineWrapping(t *testing.T) {
	for _, size := range []int{0, 1, 56, 57, 58, 114, 115} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			data := bytes.Repeat([]byte("x"), size)
			var got bytes.Buffer
			if err := writeBase64(&got, bytes.NewReader(data)); err != nil {
				t.Fatal(err)
			}

			encoded := base64.StdEncoding.EncodeToString(data)
			var want strings.Builder
			for len(encoded) > 76 {
				want.WriteString(encoded[:76])
				want.WriteString("\r\n")
				encoded = encoded[76:]
			}
			want.WriteString(encoded)
			want.WriteString("\r\n")
			if got.String() != want.String() {
				t.Fatalf("encoded MIME body mismatch for %d input bytes", size)
			}
		})
	}
}
