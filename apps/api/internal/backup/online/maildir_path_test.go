package online

import "testing"

func TestValidMaildirPathUsesCollectorFlagGrammar(t *testing.T) {
	for _, name := range []string{"example.test/user/Maildir/cur/message:2,S", "example.test/user/Maildir/.Sent/cur/message:2,RS", "example.test/user/Maildir/cur/message:2,", "example.test/user/Maildir/new/message"} {
		if !ValidMaildirPath(name) {
			t.Fatal("valid Maildir path rejected", name)
		}
	}
	for _, name := range []string{"example.test/user/Maildir/cur/message:stream", "example.test/user/Maildir/cur/message:2,S:stream", "example.test/user/Maildir/cur/message:2,../x", "example.test/user/Maildir/cur/../message:2,S", "example.test/user/Maildir/.Sent:stream/cur/message:2,S", "example.test/user/Maildir/tmp/message:2,S", "example.test/user/Maildir/cur/invalid\xff:2,S", "example.test/user/Maildir/cur/message:2,S\n"} {
		if ValidMaildirPath(name) {
			t.Fatal("unsafe Maildir path accepted", name)
		}
	}
}
