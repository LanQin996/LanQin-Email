package app

import "testing"

func TestNormalizeMessageID(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{" <ABC@example.test> ", "abc@example.test"},
		{"bad", ""},
		{"<bad\n@example.test>", ""},
	} {
		if got := normalizeMessageID(tc.in); got != tc.want {
			t.Errorf("normalizeMessageID(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMessageIDList(t *testing.T) {
	got := messageIDList("<Root@example.test>  <Parent@example.test> <root@example.test>")
	if len(got) != 2 || got[0] != "root@example.test" || got[1] != "parent@example.test" {
		t.Fatalf("unexpected ids: %#v", got)
	}
}
