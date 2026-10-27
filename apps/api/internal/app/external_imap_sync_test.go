package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

type externalIMAPSyncTestClient struct {
	items       []externalIMAPRemoteMessage
	fetchLimit  int
	fetchCalls  int
	rawFetches  int
	rawFetchErr error
}

func (c *externalIMAPSyncTestClient) Close() error { return nil }

func (c *externalIMAPSyncTestClient) ListFolders(context.Context) ([]externalIMAPRemoteFolder, error) {
	return nil, nil
}

func (c *externalIMAPSyncTestClient) FetchSummaries(context.Context, string, string, string, int) ([]externalIMAPRemoteMessage, string, int, error) {
	return nil, "", 0, nil
}

func (c *externalIMAPSyncTestClient) FetchNew(_ context.Context, _ string, afterUID uint32, limit int) ([]externalIMAPRemoteMessage, error) {
	c.fetchCalls++
	c.fetchLimit = limit
	items := make([]externalIMAPRemoteMessage, 0, len(c.items))
	for _, item := range c.items {
		if item.UID > afterUID {
			items = append(items, item)
		}
	}
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (c *externalIMAPSyncTestClient) FetchRaw(context.Context, string, uint32) ([]byte, externalIMAPRemoteMessage, error) {
	c.rawFetches++
	return nil, externalIMAPRemoteMessage{}, c.rawFetchErr
}

func (c *externalIMAPSyncTestClient) FetchAttachments(context.Context, string, uint32) ([]Attachment, error) {
	return nil, nil
}

func (c *externalIMAPSyncTestClient) FetchPart(context.Context, string, uint32, string) ([]byte, Attachment, error) {
	return nil, Attachment{}, nil
}

func (c *externalIMAPSyncTestClient) SetRead(context.Context, string, uint32, bool) error {
	return nil
}

func TestExternalIMAPSyncFolderUsesBatchBodiesAndRetriesFailedUID(t *testing.T) {
	a := newTestApp(t)
	_, mailbox := defaultAdminUserAndMailbox(t, a)
	accountID := "ximap_sync_batch_test"
	now := a.now().UTC().Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`INSERT INTO external_imap_accounts(id,user_id,mailbox_id,name,host,port,tls_mode,username,password_ciphertext,storage_mode,sync_read_state,enabled,last_status,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, accountID, mailbox.UserID, mailbox.ID, "IMAP batch test", "imap.example.test", 993, externalIMAPTLS, "remote@example.test", "ciphertext", externalIMAPStorageLocal, 1, 1, "idle", now, now); err != nil {
		t.Fatal(err)
	}

	client := &externalIMAPSyncTestClient{
		items: []externalIMAPRemoteMessage{
			{
				UID: 1, UIDValidity: 7, Folder: "INBOX", IsRead: true,
				Raw: []byte("From: sender@example.test\r\nTo: remote@example.test\r\nSubject: synthetic import\r\nMessage-ID: <batch-one@example.test>\r\n\r\nbody"),
			},
			{UID: 2, UIDValidity: 7, Folder: "INBOX", IsRead: false},
		},
		rawFetchErr: errors.New("synthetic fetch failure"),
	}
	account := externalIMAPAccountRecord{ExternalIMAPAccount: ExternalIMAPAccount{
		ID: accountID, UserID: mailbox.UserID, MailboxID: mailbox.ID, Username: "remote@example.test", StorageMode: externalIMAPStorageLocal,
	}}

	imported, skipped, failed, err := a.syncExternalIMAPFolder(t.Context(), account, client, externalIMAPRemoteFolder{Name: "INBOX", UIDValidity: 7})
	if err != nil {
		t.Fatalf("sync folder: %v", err)
	}
	if imported != 1 || skipped != 0 || failed != 1 {
		t.Fatalf("sync counts = imported %d, skipped %d, failed %d", imported, skipped, failed)
	}
	if client.fetchLimit != externalIMAPSyncBatchSize || client.rawFetches != 1 {
		t.Fatalf("fetch limit=%d raw fetches=%d, want batch limit %d and one fallback", client.fetchLimit, client.rawFetches, externalIMAPSyncBatchSize)
	}
	var lastUID uint32
	if err := a.db.QueryRow(`SELECT last_uid FROM external_imap_folder_states WHERE account_id=? AND remote_folder=?`, accountID, "INBOX").Scan(&lastUID); err != nil {
		t.Fatal(err)
	}
	if lastUID != 1 {
		t.Fatalf("last UID checkpoint=%d, want successful UID 1", lastUID)
	}
}
