package app

import (
	"encoding/base64"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"testing"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

func TestDecodeMIMEHeaderGB2312(t *testing.T) {
	const encoded = "=?gb2312?B?suLK1Nb3zOI=?="
	if got := decodeMIMEHeader(encoded); got != "测试主题" {
		t.Fatalf("decodeMIMEHeader(%q) = %q, want 测试主题", encoded, got)
	}
}

func TestDecodeMIMEHeaderCharsets(t *testing.T) {
	for _, charset := range []struct {
		name string
		enc  encoding.Encoding
	}{
		{"utf-8", encoding.Nop},
		{"gb2312", simplifiedchinese.GBK},
		{"GB2312", simplifiedchinese.GBK},
		{"euc-cn", simplifiedchinese.GBK},
		{"csGB2312", simplifiedchinese.GBK},
		{"gbk", simplifiedchinese.GBK},
		{"gb18030", simplifiedchinese.GB18030},
		{"big5", traditionalchinese.Big5},
		{"shift_jis", japanese.ShiftJIS},
	} {
		t.Run(charset.name, func(t *testing.T) {
			const want = "中文 Test"
			encoded, err := charset.enc.NewEncoder().String(want)
			if err != nil {
				t.Fatal(err)
			}
			for _, transfer := range []mime.WordEncoder{mime.BEncoding, mime.QEncoding} {
				header := transfer.Encode(charset.name, encoded)
				if got := decodeMIMEHeader(header); got != want {
					t.Errorf("decodeMIMEHeader(%q) = %q, want %q", header, got, want)
				}
				if got := decodeMIMEHeader("Re: " + header + "\r\n\t" + header + " suffix"); got != "Re: "+want+want+" suffix" {
					t.Errorf("folded %q header = %q", charset.name, got)
				}
			}
		})
	}
}

func TestDecodeMIMEHeaderHZKeepsItsOwnDecoder(t *testing.T) {
	const want = "中文 Test"
	encoded, err := simplifiedchinese.HZGB2312.NewEncoder().String(want)
	if err != nil {
		t.Fatal(err)
	}
	// HZ uses only ASCII bytes, so WordEncoder would omit the MIME wrapper.
	header := "=?hz-gb-2312?B?" + base64.StdEncoding.EncodeToString([]byte(encoded)) + "?="
	if got := decodeMIMEHeader(header); got != want {
		t.Fatalf("decodeMIMEHeader(%q) = %q, want %q", header, got, want)
	}
}

func TestDecodeMIMEHeaderPreservesUndecodableInput(t *testing.T) {
	for _, value := range []string{
		"", "Plain ASCII", "普通主题", "Re: 普通主题",
		"=?unknown-charset?B?suLK1Nb3zOI=?=",
		"=?gb2312?B?%%%?=",
		"=?gb2312?Q?=ZZ?=",
		"=?gb2312?B?unfinished",
	} {
		if got := decodeMIMEHeader(value); got != value {
			t.Errorf("decodeMIMEHeader(%q) = %q, want unchanged input", value, got)
		}
	}
}

func TestParseMaildirMessageGB2312Subject(t *testing.T) {
	raw := []byte("From: sender@example.test\r\n" +
		"To: receiver@example.test\r\n" +
		"Subject: =?gb2312?B?suLK1Nb3zOI=?=\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\nSynthetic body")
	message, _, err := parseExternalIMAPRawForSnippet(raw)
	if err != nil {
		t.Fatal(err)
	}
	if message.Subject != "测试主题" {
		t.Fatalf("parsed subject = %q, want 测试主题", message.Subject)
	}
}

func TestGB2312HeadersAlsoDecodeNames(t *testing.T) {
	const encoded = "=?gb2312?B?suLK1Nb3zOI=?="
	address, name := firstAddressParts(encoded + " <sender@example.test>")
	if address != "sender@example.test" || name != "测试主题" {
		t.Errorf("From header: address=%q name=%q", address, name)
	}
	header := textproto.MIMEHeader{"Content-Disposition": {`attachment; filename="` + encoded + `"`}}
	if got := partFilename(header); got != "测试主题" {
		t.Errorf("attachment filename=%q, want 测试主题", got)
	}
}

func TestExternalRemoteMessageDecodesSubject(t *testing.T) {
	var account externalIMAPAccountRecord
	account.ID, account.MailboxID = "external-test", "mailbox-test"
	for _, includeBody := range []bool{false, true} {
		msg := externalRemoteMessageToMailMessage(account, externalIMAPRemoteMessage{
			Folder: "INBOX", UID: 1, Subject: "=?gb2312?B?suLK1Nb3zOI=?=",
		}, includeBody)
		if msg.Subject != "测试主题" {
			t.Errorf("includeBody=%v: subject=%q, want 测试主题", includeBody, msg.Subject)
		}
	}
}

func TestStoredEncodedSubjectReadPaths(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)
	server := httptest.NewServer(a.Router())
	defer server.Close()
	admin := &testClient{t: t, server: server}
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("login code=%d", code)
	}
	_, mailbox := defaultAdminUserAndMailbox(t, a)
	folderID, err := a.ensureFolder(t.Context(), mailbox.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	const encoded = "=?gb2312?B?suLK1Nb3zOI=?="
	const body = "Synthetic body: =?gb2312?B?suLK1Nb3zOI=?="
	// Simulate a message stored before the charset fix, without reimporting it.
	id, err := a.insertMessage(t.Context(), storedMessage{
		MailboxID: mailbox.ID, FolderID: folderID, MessageUID: newID("uid"),
		MessageID: "<encoded-subject@example.test>", Subject: encoded,
		From: "sender@example.test", To: []string{mailbox.Address},
		SentAt: a.now(), ReceivedAt: a.now(), BodyText: body, IsStarred: true,
	}, []AttachmentInput{{Filename: "synthetic.txt", ContentType: "text/plain", ContentBase64: base64.StdEncoding.EncodeToString([]byte(body))}})
	if err != nil {
		t.Fatal(err)
	}
	label, err := a.ensureLabel(t.Context(), mailbox.ID, "Synthetic label", "#888888")
	if err != nil {
		t.Fatal(err)
	}
	if code := admin.do("POST", "/api/mail/messages/"+id+"/labels", map[string]string{"name": label.Name}, nil); code != http.StatusOK {
		t.Fatalf("add label code=%d", code)
	}
	checkList := func(client *testClient, path string) {
		t.Helper()
		page := getMailMessagePage(t, client, path)
		for _, item := range page.Items {
			if item.ID == id {
				if item.Subject != "测试主题" {
					t.Errorf("GET %s: subject=%q, want 测试主题", path, item.Subject)
				}
				return
			}
		}
		t.Errorf("GET %s: message missing", path)
	}
	checkDetail := func(client *testClient, path string) {
		t.Helper()
		var msg MailMessage
		if code := client.do("GET", path, nil, &msg); code != http.StatusOK {
			t.Fatalf("GET %s code=%d", path, code)
		}
		if msg.Subject != "测试主题" || msg.BodyText != body {
			t.Errorf("GET %s: subject=%q body=%q", path, msg.Subject, msg.BodyText)
		}
	}
	for _, path := range []string{
		"/api/mail/messages?mailboxId=" + mailbox.ID + "&folder=INBOX",
		"/api/mail/starred?mailboxId=" + mailbox.ID,
		"/api/mail/messages?mailboxId=" + mailbox.ID + "&labelId=" + label.ID,
		"/api/mail/threads?mailboxId=" + mailbox.ID,
		"/api/mail/messages/" + id + "/thread",
		"/api/admin/messages?mailboxId=" + mailbox.ID,
	} {
		checkList(admin, path)
	}
	checkDetail(admin, "/api/mail/messages/"+id+"?markRead=0")
	checkDetail(admin, "/api/admin/messages/"+id)

	viewer := createTestMailbox(t, admin, mustDefaultDomainID(t, a), "subject-viewer", "Subject Viewer", "Password123!", nil)
	client := &testClient{t: t, server: server}
	if code := client.do("POST", "/api/auth/login", map[string]string{"email": viewer.Address, "password": "Password123!"}, nil); code != http.StatusOK {
		t.Fatalf("viewer login code=%d", code)
	}
	listPath := "/api/mail/messages?mailboxId=" + url.QueryEscape(mailbox.ID) + "&folder=INBOX"
	detailPath := "/api/mail/messages/" + id + "?markRead=0"
	for _, path := range []string{listPath, detailPath} {
		if code := client.do("GET", path, nil, nil); code != http.StatusNotFound {
			t.Fatalf("unshared GET %s code=%d, want 404", path, code)
		}
	}
	var share MailboxShare
	if code := admin.do("POST", "/api/me/mailbox-shares", map[string]any{
		"mailboxId": mailbox.ID, "sharedWithUserId": viewer.UserID, "scope": "all", "expiresInDays": 0,
	}, &share); code != http.StatusCreated {
		t.Fatalf("create share code=%d", code)
	}
	checkList(client, listPath)
	checkDetail(client, detailPath)
	if code := admin.do("DELETE", "/api/me/mailbox-shares/"+share.ID, nil, nil); code != http.StatusOK {
		t.Fatalf("revoke share code=%d", code)
	}
	for _, path := range []string{listPath, detailPath} {
		if code := client.do("GET", path, nil, nil); code != http.StatusNotFound {
			t.Fatalf("revoked GET %s code=%d, want 404", path, code)
		}
	}
	var storedSubject, storedBody string
	if err := a.db.QueryRow(`SELECT subject,body_text FROM messages WHERE id=?`, id).Scan(&storedSubject, &storedBody); err != nil {
		t.Fatal(err)
	}
	if storedSubject != encoded || storedBody != body {
		t.Fatalf("read mutated stored content: subject=%q body=%q", storedSubject, storedBody)
	}
}
