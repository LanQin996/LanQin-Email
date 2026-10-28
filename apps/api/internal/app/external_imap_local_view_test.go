package app

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalExternalIMAPMessagesAreHiddenFromMailboxAndVisibleInAccount(t *testing.T) {
	dir := newTestDir(t)
	a := newTestAppWithConfig(t, Config{
		Addr:                          ":0",
		DBPath:                        filepath.Join(dir, "lanqin.db"),
		DataDir:                       filepath.Join(dir, "data"),
		CookieName:                    "lanqin_test",
		SessionTTLHours:               24,
		AdminEmail:                    "admin@lanqin.local",
		AdminPassword:                 "ChangeMe123!",
		PublicHostname:                "mail.example.test",
		PublicBaseURL:                 "http://localhost:5173",
		AllowInsecureHTTP:             true,
		ExternalIMAPEnabled:           true,
		ExternalIMAPSecretKey:         "test-secret",
		ExternalIMAPAllowPrivateHosts: true,
	})
	ts := httptest.NewServer(a.Router())
	defer ts.Close()
	admin := &testClient{t: t, server: ts}
	if code := admin.do("POST", "/api/auth/login", map[string]string{"email": "admin@lanqin.local", "password": "ChangeMe123!"}, nil); code != http.StatusOK {
		t.Fatalf("login code=%d", code)
	}
	user, mailbox := defaultAdminUserAndMailbox(t, a)
	now := a.now().UTC().Format(time.RFC3339Nano)
	accountID := "ximap_local_view_test"
	if _, err := a.db.Exec(`INSERT INTO external_imap_accounts(id,user_id,mailbox_id,name,host,port,tls_mode,username,password_ciphertext,storage_mode,sync_read_state,enabled,last_status,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, accountID, user.ID, mailbox.ID, "External test", "imap.example.test", 993, externalIMAPTLS, "remote@example.test", "ciphertext", externalIMAPStorageLocal, 0, 1, "idle", now, now); err != nil {
		t.Fatal(err)
	}
	inboxID, err := a.ensureFolder(t.Context(), mailbox.ID, "Inbox")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO external_imap_folder_states(account_id,remote_folder,local_folder_id,uid_validity,last_uid,last_sync_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?)`, accountID, "INBOX", inboxID, 7, 1, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := a.insertMessage(t.Context(), storedMessage{
		MailboxID: mailbox.ID, FolderID: inboxID, MessageUID: newID("uid"), MessageID: "<native@example.test>",
		Subject: "native message", From: "sender@example.test", To: []string{mailbox.Address},
		SentAt: a.now().UTC(), ReceivedAt: a.now().UTC(), BodyText: "native body", Snippet: "native body",
	}, nil); err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: sender@example.test\r\nTo: remote@example.test\r\nSubject: external message\r\nMessage-ID: <external@example.test>\r\n\r\nexternal body")
	external, attachments, err := a.parseMaildirMessage(raw, "remote@example.test")
	if err != nil {
		t.Fatal(err)
	}
	external.MailboxID = mailbox.ID
	external.FolderID = inboxID
	external.RecipientAddr = "remote@example.test"
	external.IsRead = true
	messageID, err := a.insertMessage(t.Context(), external, attachments)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO external_imap_messages(account_id,remote_folder,uid_validity,uid,message_id,local_message_id,is_read,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, accountID, "INBOX", 7, 1, external.MessageID, messageID, 1, now, now); err != nil {
		t.Fatal(err)
	}
	externalFolderID, err := a.ensureFolder(t.Context(), mailbox.ID, externalIMAPLocalFolderName(accountID, "INBOX"))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.moveExternalIMAPMessagesToFolder(t.Context(), accountID, mailbox.ID, "INBOX", externalFolderID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE external_imap_folder_states SET local_folder_id=? WHERE account_id=? AND remote_folder=?`, externalFolderID, accountID, "INBOX"); err != nil {
		t.Fatal(err)
	}
	if messageID == "" {
		t.Fatal("legacy external message was not inserted")
	}

	var mailboxList struct {
		Items      []MailMessage `json:"items"`
		TotalCount int           `json:"totalCount"`
	}
	if code := admin.do("GET", "/api/mail/messages?folder=Inbox&mailboxId="+mailbox.ID, nil, &mailboxList); code != http.StatusOK {
		t.Fatalf("mailbox messages code=%d", code)
	}
	if mailboxList.TotalCount != len(mailboxList.Items) {
		t.Fatalf("ordinary inbox included external data: %+v", mailboxList)
	}
	foundNative := false
	for _, item := range mailboxList.Items {
		if item.Subject == "external message" {
			t.Fatalf("ordinary inbox exposed external message: %+v", item)
		}
		foundNative = foundNative || item.Subject == "native message"
	}
	if !foundNative {
		t.Fatalf("ordinary inbox lost native message: %+v", mailboxList)
	}
	for _, path := range []string{
		"/api/mail/folders?mailboxId=" + mailbox.ID,
		"/api/me/stats?mailboxId=" + mailbox.ID,
	} {
		var payload map[string]any
		if code := admin.do("GET", path, nil, &payload); code != http.StatusOK {
			t.Fatalf("GET %s code=%d", path, code)
		}
		if path == "/api/mail/folders?mailboxId="+mailbox.ID {
			items, _ := payload["items"].([]any)
			for _, rawItem := range items {
				item, _ := rawItem.(map[string]any)
				if name, _ := item["name"].(string); strings.HasPrefix(name, "External ") {
					t.Fatalf("external-only folder leaked into mailbox folder list: %s", name)
				}
			}
		} else if total, _ := payload["totalMessages"].(float64); int(total) != mailboxList.TotalCount {
			t.Fatalf("mail stats total=%v, inbox total=%d", total, mailboxList.TotalCount)
		}
	}
	var externalList struct {
		Items      []MailMessage `json:"items"`
		TotalCount int           `json:"totalCount"`
	}
	if code := admin.do("GET", "/api/mail/external-accounts/"+accountID+"/messages?folder=INBOX", nil, &externalList); code != http.StatusOK {
		t.Fatalf("external account messages code=%d", code)
	}
	if externalList.TotalCount != 1 || len(externalList.Items) != 1 || externalList.Items[0].ID != messageID {
		t.Fatalf("external account list missing local copy: %+v", externalList)
	}
	var detail MailMessage
	if code := admin.do("GET", "/api/mail/external-accounts/"+accountID+"/messages/"+messageID, nil, &detail); code != http.StatusOK || detail.Subject != "external message" {
		t.Fatalf("external local detail code=%d detail=%+v", code, detail)
	}
	if code := admin.do("GET", "/api/mail/messages/"+messageID, nil, &detail); code != http.StatusNotFound {
		t.Fatalf("ordinary message detail exposed external copy: code=%d", code)
	}
	if code := admin.do("POST", "/api/mail/external-accounts/"+accountID+"/messages/"+messageID+"/mark-read", map[string]bool{"read": false}, nil); code != http.StatusOK {
		t.Fatalf("local external read update code=%d", code)
	}
	var isRead int
	if err := a.db.QueryRow(`SELECT is_read FROM messages WHERE id=?`, messageID).Scan(&isRead); err != nil || isRead != 0 {
		t.Fatalf("local read state=%d err=%v", isRead, err)
	}
}
