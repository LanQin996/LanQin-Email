package app

import (
	"context"
	"reflect"
	"testing"
	"time"
)

type externalIMAPWorkerTestClient struct {
	externalIMAPSyncTestClient
	listFolders func(context.Context) ([]externalIMAPRemoteFolder, error)
}

func (c *externalIMAPWorkerTestClient) ListFolders(ctx context.Context) ([]externalIMAPRemoteFolder, error) {
	return c.listFolders(ctx)
}

type externalIMAPWorkerTestFactory struct {
	client     externalIMAPClient
	accountIDs []string
}

func (f *externalIMAPWorkerTestFactory) openExternalIMAPClient(_ context.Context, account externalIMAPAccountRecord) (externalIMAPClient, error) {
	f.accountIDs = append(f.accountIDs, account.ID)
	return f.client, nil
}

func TestSyncDueExternalIMAPAccountsReleasesConnectionBeforeSync(t *testing.T) {
	for _, cancelSync := range []bool{false, true} {
		name := "complete"
		if cancelSync {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			a := newTestApp(t)
			stopTestWorkers(a)
			a.cfg.ExternalIMAPEnabled = true
			if got := a.db.Stats().MaxOpenConnections; got != 1 {
				t.Fatalf("SQLite max open connections = %d, want 1", got)
			}
			_, mailbox := defaultAdminUserAndMailbox(t, a)
			accounts := []struct {
				id      string
				mode    string
				enabled int
			}{
				{"ximap_worker_first", externalIMAPStorageLocal, 1},
				{"ximap_worker_second", externalIMAPStorageLocal, 1},
				{"ximap_worker_disabled", externalIMAPStorageLocal, 0},
				{"ximap_worker_remote", externalIMAPStorageRemote, 1},
			}
			for i, account := range accounts {
				now := a.now().UTC().Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
				if _, err := a.db.ExecContext(t.Context(), `INSERT INTO external_imap_accounts(id,user_id,mailbox_id,name,host,port,tls_mode,username,password_ciphertext,storage_mode,sync_read_state,enabled,last_status,created_at,updated_at)
					VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, account.id, mailbox.UserID, mailbox.ID, "Worker test", "imap.example.test", 993, externalIMAPTLS, "remote@example.test", "unused", account.mode, 1, account.enabled, "idle", now, now); err != nil {
					t.Fatal(err)
				}
			}

			started := make(chan struct{}, 1)
			resume := make(chan struct{})
			factory := &externalIMAPWorkerTestFactory{client: &externalIMAPWorkerTestClient{
				listFolders: func(ctx context.Context) ([]externalIMAPRemoteFolder, error) {
					select {
					case started <- struct{}{}:
					default:
					}
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-resume:
						return nil, nil
					}
				},
			}}
			a.externalIMAP = factory
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			done := make(chan struct{})
			go func() {
				defer close(done)
				a.syncDueExternalIMAPAccounts(ctx)
			}()
			t.Cleanup(func() {
				cancel()
				<-done
			})
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("sync could not acquire the SQLite connection to reach the IMAP client")
			}

			// Network synchronization must not retain the only SQLite connection.
			probeCtx, probeCancel := context.WithTimeout(t.Context(), time.Second)
			defer probeCancel()
			if err := a.checkDatabaseReady(probeCtx); err != nil {
				t.Fatalf("database unavailable during IMAP network wait: %v", err)
			}
			if err := a.checkSchemaReady(probeCtx); err != nil {
				t.Fatalf("schema unavailable during IMAP network wait: %v", err)
			}
			var messageCount int
			if err := a.db.QueryRowContext(probeCtx, `SELECT COUNT(*) FROM messages`).Scan(&messageCount); err != nil {
				t.Fatalf("local mail query blocked during IMAP network wait: %v", err)
			}

			if cancelSync {
				cancel()
			} else {
				close(resume)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("sync did not exit after completion or cancellation")
			}
			wantIDs := []string{"ximap_worker_first"}
			if !cancelSync {
				wantIDs = append(wantIDs, "ximap_worker_second")
			}
			if !reflect.DeepEqual(factory.accountIDs, wantIDs) {
				t.Fatalf("synced accounts = %v, want %v", factory.accountIDs, wantIDs)
			}
			verifyCtx, verifyCancel := context.WithTimeout(t.Context(), time.Second)
			defer verifyCancel()
			if err := a.checkDatabaseReady(verifyCtx); err != nil {
				t.Fatalf("database unavailable after sync exits: %v", err)
			}
			if !cancelSync {
				var successfulRuns int
				if err := a.db.QueryRowContext(verifyCtx, `SELECT COUNT(*) FROM external_imap_sync_runs WHERE status='ok'`).Scan(&successfulRuns); err != nil || successfulRuns != len(wantIDs) {
					t.Fatalf("successful sync runs = %d, want %d; error = %v", successfulRuns, len(wantIDs), err)
				}
			}
		})
	}
}
