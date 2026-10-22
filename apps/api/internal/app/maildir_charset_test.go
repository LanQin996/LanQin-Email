package app

import (
	"encoding/base64"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

func TestParseMaildirMessageConvertsGB2312BodyToUTF8(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)

	const want = "你好，世界"
	encoded, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(want))
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: sender@example.test\r\n" +
		"To: receiver@example.test\r\n" +
		"Subject: charset test\r\n" +
		"Content-Type: text/plain; charset=gb2312\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString(encoded) + "\r\n")

	message, _, err := a.parseMaildirMessage(raw, "receiver@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if message.BodyText != want {
		t.Fatalf("BodyText = %q, want %q", message.BodyText, want)
	}
}
