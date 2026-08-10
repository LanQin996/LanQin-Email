package app

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAttachmentInputContentBytesCachesDecodedData(t *testing.T) {
	input := AttachmentInput{ContentBase64: "aGVsbG8="}

	first, err := input.contentBytes()
	if err != nil {
		t.Fatalf("decode attachment: %v", err)
	}
	input.ContentBase64 = "invalid"
	second, err := input.contentBytes()
	if err != nil {
		t.Fatalf("reuse decoded attachment: %v", err)
	}
	if string(first) != "hello" || string(second) != "hello" {
		t.Fatalf("unexpected decoded content: first=%q second=%q", first, second)
	}
}

func TestDecodeJSONWithLimitRejectsOversizedBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"value":"body exceeds limit"}`))
	var payload struct {
		Value string `json:"value"`
	}

	err := decodeJSONWithLimit(req, &payload, 12)
	var tooLarge *http.MaxBytesError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("expected MaxBytesError, got %v", err)
	}
}
