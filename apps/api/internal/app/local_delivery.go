package app

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type localDeliveryPayload struct {
	Message     storedMessage
	Attachments []AttachmentInput
}

func baseRecipient(address string) string {
	address = normalizeEmail(address)
	at := strings.LastIndex(address, "@")
	if at < 0 {
		return address
	}
	if plus := strings.Index(address[:at], "+"); plus >= 0 {
		return address[:plus] + address[at:]
	}
	return address
}

func (a *App) collectionForRecipient(ctx context.Context, address string) (string, error) {
	at := strings.LastIndex(address, "@")
	if at < 0 {
		return "", nil
	}
	var id string
	err := a.db.QueryRowContext(ctx, "SELECT m.id FROM domain_collections c JOIN domains s ON s.id=c.domain_id JOIN mailboxes m ON m.id=c.target_mailbox_id JOIN domains d ON d.id=m.domain_id JOIN users u ON u.id=m.user_id WHERE s.name=? AND s.status='active' AND m.status='active' AND d.status='active' AND u.disabled=0", address[at+1:]).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// Only original envelope recipients generate domain copies. Unknown alias
// destinations may use their fallback; generated destinations never recurse.
func (a *App) localRecipientTargets(ctx context.Context, recipients []string) (map[string]bool, []string, error) {
	targets := map[string]bool{}
	unknown := []string{}
	seenUnknown := map[string]bool{}
	var resolve func(string, map[string]bool, int) (bool, error)
	resolve = func(address string, seen map[string]bool, depth int) (bool, error) {
		address = normalizeEmail(address)
		if depth > 20 || seen[address] {
			return false, nil
		}
		seen[address] = true
		base := baseRecipient(address)
		var id string
		err := a.db.QueryRowContext(ctx, "SELECT m.id FROM mailboxes m JOIN domains d ON d.id=m.domain_id JOIN users u ON u.id=m.user_id WHERE m.address=? AND m.status='active' AND d.status='active' AND u.disabled=0", base).Scan(&id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		matched := err == nil
		if matched {
			targets[id] = true
		}
		var destination string
		err = a.db.QueryRowContext(ctx, "SELECT destination FROM aliases WHERE source=? AND enabled=1", address).Scan(&destination)
		if errors.Is(err, sql.ErrNoRows) && base != address {
			err = a.db.QueryRowContext(ctx, "SELECT destination FROM aliases WHERE source=? AND enabled=1", base).Scan(&destination)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		if err == nil {
			matched = true // An explicit alias is not an unknown-address catch-all.
			for _, dest := range strings.Split(destination, ",") {
				if _, err := resolve(dest, seen, depth+1); err != nil {
					return false, err
				}
			}
		}
		if !matched {
			// Alias destinations can themselves be unknown local addresses. Resolve
			// their fallback, but never generate a domain BCC for an existing alias target.
			target, err := a.collectionForRecipient(ctx, address)
			if err != nil {
				return false, err
			}
			if target != "" {
				targets[target] = true
				matched = true
			} else if a.cfg.CatchAllEnabled && a.isLocalDomainAddress(ctx, address) && !seenUnknown[address] {
				unknown = append(unknown, address)
				seenUnknown[address] = true
				matched = true
			}
		}
		return matched, nil
	}
	for _, address := range recipients {
		address = normalizeEmail(address)
		matched, err := resolve(address, map[string]bool{}, 0)
		if err != nil {
			return nil, nil, err
		}
		target, err := a.collectionForRecipient(ctx, address)
		if err != nil {
			return nil, nil, err
		}
		if target != "" {
			targets[target] = true
		} else if !matched && a.cfg.CatchAllEnabled && a.isLocalDomainAddress(ctx, address) && !seenUnknown[address] {
			unknown = append(unknown, address)
			seenUnknown[address] = true
		}
	}
	return targets, unknown, nil
}

func (a *App) deliverLocalRecipients(ctx context.Context, sourceID string, recipients []string, base storedMessage, attachments []AttachmentInput) error {
	targets, unknown, err := a.localRecipientTargets(ctx, recipients)
	if err != nil {
		return err
	}
	// Snapshot attachment bytes because the sender can delete the sent message.
	inputs := make([]AttachmentInput, len(attachments))
	for i, att := range attachments {
		data, err := att.contentBytes()
		if err != nil {
			return err
		}
		inputs[i] = AttachmentInput{Filename: att.Filename, ContentType: att.ContentType, ContentBase64: base64.StdEncoding.EncodeToString(data)}
	}
	base.BCC = nil // Bcc is envelope-only on recipient copies, including local delivery.
	base.IsRead = false
	base.IsStarred = false
	base.RawPath = ""
	base.ThreadID = ""
	payload, err := json.Marshal(localDeliveryPayload{Message: base, Attachments: inputs})
	if err != nil {
		return err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := a.now().UTC().Format(time.RFC3339Nano)
	jobs := []string{}
	for target := range targets {
		id := newID("ldj")
		query := upsertSQL(a.cfg.DBDriver, "INSERT INTO local_delivery_jobs(id,source_id,target_mailbox_id,payload,status,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,'queued',?,?,?)", "(source_id,target_mailbox_id)", "source_id=excluded.source_id", "source_id=VALUES(source_id)")
		if _, err = tx.ExecContext(ctx, query, id, sourceID, target, string(payload), now, now, now); err != nil {
			return err
		}
		if err = tx.QueryRowContext(ctx, "SELECT id FROM local_delivery_jobs WHERE source_id=? AND target_mailbox_id=?", sourceID, target).Scan(&id); err != nil {
			return err
		}
		jobs = append(jobs, id)
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	// The ordinary local development path remains immediately visible; any failed
	// recipient remains independently queued, even when another recipient succeeds.
	for _, id := range jobs {
		a.processLocalDelivery(ctx, id)
	}
	for _, address := range unknown {
		msg := base
		msg.MailboxID = ""
		msg.FolderID = ""
		msg.RecipientAddr = address
		msg.MessageUID = newID("uid")
		if _, err := a.insertMessage(ctx, msg, inputs); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) localDeliveryWorker(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := a.now().UTC().Format(time.RFC3339Nano)
			rows, err := a.db.QueryContext(ctx, "SELECT id FROM local_delivery_jobs WHERE (status='queued' AND next_attempt_at<=?) OR (status='sending' AND lease_until<=?) ORDER BY created_at LIMIT 50", now, now)
			if err != nil {
				continue
			}
			ids := []string{}
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					ids = append(ids, id)
				}
			}
			rows.Close()
			for _, id := range ids {
				if ctx.Err() != nil {
					return
				}
				a.processLocalDelivery(ctx, id)
			}
			// Terminal jobs keep metadata for diagnostics, never indefinite message bodies.
			cutoff := a.now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339Nano)
			_, _ = a.db.ExecContext(ctx, "DELETE FROM local_delivery_jobs WHERE status IN ('delivered','failed') AND updated_at<?", cutoff)
		}
	}
}

func (a *App) processLocalDelivery(ctx context.Context, id string) {
	token := newID("lease")
	now := a.now().UTC()
	stamp := now.Format(time.RFC3339Nano)
	res, err := a.db.ExecContext(ctx, "UPDATE local_delivery_jobs SET status='sending',lease_token=?,lease_until=?,attempts=attempts+1,updated_at=? WHERE id=? AND ((status='queued' AND next_attempt_at<=?) OR (status='sending' AND lease_until<=?))", token, now.Add(5*time.Minute).Format(time.RFC3339Nano), stamp, id, stamp, stamp)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return
	}
	err = a.completeLocalDelivery(ctx, id, token)
	if err == nil {
		return
	}
	// Never persist raw errors containing addresses, message bodies, or file paths.
	code := "local_delivery_failed"
	if errors.Is(err, errMailboxQuotaExceeded) {
		code = "mailbox_quota_exceeded"
	}
	var attempts int
	if e := a.db.QueryRowContext(ctx, "SELECT attempts FROM local_delivery_jobs WHERE id=? AND lease_token=?", id, token).Scan(&attempts); e != nil {
		return
	}
	status := "queued"
	if attempts >= 8 {
		status = "failed"
	}
	delay := time.Duration(30*(1<<min(attempts, 7))) * time.Second
	_, _ = a.db.ExecContext(ctx, "UPDATE local_delivery_jobs SET status=?,next_attempt_at=?,last_error=?,lease_token='',lease_until='',payload=CASE WHEN ?='failed' THEN '' ELSE payload END,updated_at=? WHERE id=? AND lease_token=?", status, now.Add(delay).Format(time.RFC3339Nano), code, status, stamp, id, token)
	a.log.Warn("local delivery deferred", "job_id", id, "status", status, "reason", code)
}

func (a *App) completeLocalDelivery(ctx context.Context, id, token string) error {
	var raw, target, messageID string
	if err := a.db.QueryRowContext(ctx, "SELECT payload,target_mailbox_id,message_id FROM local_delivery_jobs WHERE id=? AND lease_token=?", id, token).Scan(&raw, &target, &messageID); err != nil {
		return err
	}
	var payload localDeliveryPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return err
	}
	var address string
	if err := a.db.QueryRowContext(ctx, "SELECT m.address FROM mailboxes m JOIN domains d ON d.id=m.domain_id JOIN users u ON u.id=m.user_id WHERE m.id=? AND m.status='active' AND d.status='active' AND u.disabled=0", target).Scan(&address); err != nil {
		return err
	}
	folder, err := a.ensureFolder(ctx, target, "Inbox")
	if err != nil {
		return err
	}
	msg := payload.Message
	msg.MailboxID = target
	msg.FolderID = folder
	msg.RecipientAddr = address
	msg.MessageUID = newID("uid")
	msg.ThreadID = a.resolveThreadID(ctx, target, msg.MessageID, msg.InReplyTo, msg.References)
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	createdMessageID := ""
	committed := false
	defer func() {
		if !committed && createdMessageID != "" {
			_ = tx.Rollback()
			// Only this transaction's newly generated attachment directory.
			_ = os.RemoveAll(filepath.Join(a.cfg.DataDir, "attachments", createdMessageID))
		}
	}()
	// Row lock protects insertion and its durable id from concurrent lease recovery.
	res, err := tx.ExecContext(ctx, "UPDATE local_delivery_jobs SET lease_until=? WHERE id=? AND lease_token=?", a.now().UTC().Add(5*time.Minute).Format(time.RFC3339Nano), id, token)
	if err != nil {
		return err
	}
	// Serialize quota consumption for distinct jobs targeting the same mailbox.
	// Acquire this lock before any transactional reads establish a MySQL snapshot.
	if _, err = tx.ExecContext(ctx, "UPDATE mailboxes SET quota_mb=quota_mb WHERE id=?", target); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, "SELECT message_id FROM local_delivery_jobs WHERE id=? AND lease_token=?", id, token).Scan(&messageID); err != nil {
		return err
	}
	if messageID == "" {
		messageID, err = a.insertMessageWithDB(ctx, tx, msg, nil)
		if err != nil {
			return err
		}
		createdMessageID = messageID
		var added int64
		for _, att := range payload.Attachments {
			data, e := att.contentBytes()
			if e != nil {
				return e
			}
			added += int64(len(data))
		}
		if err = a.ensureMailboxQuotaAvailable(ctx, tx, target, added); err != nil {
			return err
		}
		for _, att := range payload.Attachments {
			if err = a.storeAttachmentWithDB(ctx, tx, messageID, att); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, "UPDATE messages SET size_bytes=size_bytes+?,has_attachments=? WHERE id=?", added, boolInt(len(payload.Attachments) > 0), messageID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE local_delivery_jobs SET message_id=? WHERE id=? AND lease_token=?", messageID, id, token); err != nil {
			return err
		}
	}
	// A failed COMMIT acknowledgement can still mean the database committed.
	// Do not remove attachment files in that uncertain state.
	committed = true
	if err = tx.Commit(); err != nil {
		return err
	}
	// The inserted message ID survives a crash or Maildir write failure: retry only
	// finishes filesystem delivery instead of inserting another database message.
	if err = a.writeStoredMessageToMaildir(ctx, messageID, msg, payload.Attachments, "local-"+messageID); err != nil {
		return err
	}
	res, err = a.db.ExecContext(ctx, "UPDATE local_delivery_jobs SET status='delivered',payload='',last_error='',lease_token='',lease_until='',updated_at=? WHERE id=? AND lease_token=?", a.now().UTC().Format(time.RFC3339Nano), id, token)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		a.applyInboundControls(ctx, messageID, target, msg.From, msg.Subject)
	}
	return nil
}

func (a *App) handleLocalDeliveryJobs(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), "SELECT id,target_mailbox_id,status,attempts,last_error,created_at,updated_at FROM local_delivery_jobs ORDER BY created_at DESC LIMIT 100")
	if err != nil {
		respondError(w, 500, "failed to load local deliveries")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, target, status, last, created, updated string
		var attempts int
		if err := rows.Scan(&id, &target, &status, &attempts, &last, &created, &updated); err != nil {
			respondError(w, 500, "failed to load local deliveries")
			return
		}
		items = append(items, map[string]any{"id": id, "targetMailboxId": target, "status": status, "attempts": attempts, "lastError": last, "createdAt": created, "updatedAt": updated})
	}
	if rows.Err() != nil {
		respondError(w, 500, "failed to load local deliveries")
		return
	}
	respondJSON(w, 200, map[string]any{"items": items})
}
