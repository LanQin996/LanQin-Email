package app

import "testing"

func TestParseGoogleTranslateResponse(t *testing.T) {
	raw := []any{
		[]any{
			[]any{"你好", "Hello", nil, nil, float64(3)},
			[]any{"，世界", ", world", nil, nil, float64(3)},
		},
		nil,
		"en",
	}
	translated, source := parseGoogleTranslateResponse(raw)
	if translated != "你好，世界" {
		t.Fatalf("translated = %q", translated)
	}
	if source != "en" {
		t.Fatalf("source = %q", source)
	}
}

func TestTruncateRunes(t *testing.T) {
	got, truncated := truncateRunes("你好world", 4)
	if got != "你好wo" || !truncated {
		t.Fatalf("truncateRunes() = %q, %v", got, truncated)
	}
	got, truncated = truncateRunes("你好", 4)
	if got != "你好" || truncated {
		t.Fatalf("truncateRunes() = %q, %v", got, truncated)
	}
}
