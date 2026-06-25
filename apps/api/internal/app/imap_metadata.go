package app

import (
	"context"
	"database/sql"
	"math"
	"time"
)

type imapMetadata struct {
	UID    int64
	ModSeq int64
}

func (a *App) migrateIMAPMetadata(ctx context.Context) error {
	if err := a.ensureTableColumn(ctx, "folders", "uid_validity", `ALTER TABLE folders ADD COLUMN uid_validity INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := a.ensureTableColumn(ctx, "folders", "uid_next", `ALTER TABLE folders ADD COLUMN uid_next INTEGER NOT NULL DEFAULT 1`); err != nil {
		return err
	}
	if err := a.ensureTableColumn(ctx, "folders", "highest_modseq", `ALTER TABLE folders ADD COLUMN highest_modseq INTEGER NOT NULL DEFAULT 1`); err != nil {
		return err
	}
	if err := a.ensureTableColumn(ctx, "messages", "imap_uid", `ALTER TABLE messages ADD COLUMN imap_uid INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := a.ensureTableColumn(ctx, "messages", "imap_modseq", `ALTER TABLE messages ADD COLUMN imap_modseq INTEGER NOT NULL DEFAULT 1`); err != nil {
		return err
	}
	if _, err := a.db.ExecContext(ctx, `UPDATE folders SET uid_validity=? WHERE uid_validity=0`, a.newUIDValidity()); err != nil {
		return err
	}
	if _, err := a.db.ExecContext(ctx, `UPDATE folders SET uid_next=1 WHERE uid_next<1`); err != nil {
		return err
	}
	if _, err := a.db.ExecContext(ctx, `UPDATE folders SET highest_modseq=1 WHERE highest_modseq<1`); err != nil {
		return err
	}
	if err := a.backfillIMAPUIDs(ctx); err != nil {
		return err
	}
	_, err := a.db.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_folder_imap_uid ON messages(folder_id, imap_uid) WHERE folder_id IS NOT NULL AND imap_uid > 0`)
	return err
}

func (a *App) migrateFolderSortOrder(ctx context.Context) error {
	if err := a.ensureTableColumn(ctx, "folders", "sort_order", `ALTER TABLE folders ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	rows, err := a.db.QueryContext(ctx, `SELECT id FROM folders WHERE lower(name) NOT IN ('inbox','sent','drafts','archive','spam','trash') ORDER BY mailbox_id, created_at, name, id`)
	if err != nil {
		return err
	}
	var folderIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		folderIDs = append(folderIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	order := customFolderDefaultSortOrderBase + 1
	for _, id := range folderIDs {
		if _, err := a.db.ExecContext(ctx, `UPDATE folders SET sort_order=? WHERE id=? AND sort_order=0`, order, id); err != nil {
			return err
		}
		order++
	}
	return nil
}

func (a *App) ensureTableColumn(ctx context.Context, table, column, alterSQL string) error {
	rows, err := a.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = a.db.ExecContext(ctx, alterSQL)
	return err
}

func (a *App) backfillIMAPUIDs(ctx context.Context) error {
	rows, err := a.db.QueryContext(ctx, `SELECT id FROM folders ORDER BY created_at,id`)
	if err != nil {
		return err
	}
	var folderIDs []string
	for rows.Next() {
		var folderID string
		if err := rows.Scan(&folderID); err != nil {
			rows.Close()
			return err
		}
		folderIDs = append(folderIDs, folderID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, folderID := range folderIDs {
		if err := a.backfillFolderIMAPUIDs(ctx, folderID); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) backfillFolderIMAPUIDs(ctx context.Context, folderID string) error {
	rows, err := a.db.QueryContext(ctx, `SELECT id FROM messages WHERE folder_id=? AND imap_uid=0 ORDER BY created_at,id`, folderID)
	if err != nil {
		return err
	}
	var messageIDs []string
	for rows.Next() {
		var messageID string
		if err := rows.Scan(&messageID); err != nil {
			rows.Close()
			return err
		}
		messageIDs = append(messageIDs, messageID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, messageID := range messageIDs {
		meta, err := a.nextIMAPMetadata(ctx, a.db, folderID)
		if err != nil {
			return err
		}
		if _, err := a.db.ExecContext(ctx, `UPDATE messages SET imap_uid=?,imap_modseq=? WHERE id=?`, meta.UID, meta.ModSeq, messageID); err != nil {
			return err
		}
	}
	var maxUID, maxModSeq int64
	if err := a.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(imap_uid),0),COALESCE(MAX(imap_modseq),1) FROM messages WHERE folder_id=?`, folderID).Scan(&maxUID, &maxModSeq); err != nil {
		return err
	}
	_, err = a.db.ExecContext(ctx, `UPDATE folders SET uid_next=MAX(uid_next,?),highest_modseq=MAX(highest_modseq,?) WHERE id=?`, maxUID+1, maxModSeq, folderID)
	return err
}

func (a *App) newUIDValidity() int64 {
	value := a.now().UTC().Unix()
	if value <= 0 {
		return time.Now().UTC().Unix()
	}
	return value
}

func (a *App) nextIMAPMetadata(ctx context.Context, db dbExecutor, folderID string) (imapMetadata, error) {
	if folderID == "" {
		return imapMetadata{}, nil
	}
	rowDB, ok := db.(dbQueryer)
	if !ok {
		return imapMetadata{}, nil
	}
	var nextUID, highestModSeq int64
	err := rowDB.QueryRowContext(ctx, `SELECT uid_next,highest_modseq FROM folders WHERE id=?`, folderID).Scan(&nextUID, &highestModSeq)
	if err != nil {
		return imapMetadata{}, err
	}
	if nextUID < 1 {
		nextUID = 1
	}
	nextModSeq := highestModSeq + 1
	if nextModSeq < 1 {
		nextModSeq = 1
	}
	if _, err := db.ExecContext(ctx, `UPDATE folders SET uid_next=?,highest_modseq=MAX(highest_modseq,?) WHERE id=?`, nextUID+1, nextModSeq, folderID); err != nil {
		return imapMetadata{}, err
	}
	return imapMetadata{UID: nextUID, ModSeq: nextModSeq}, nil
}

func (a *App) bumpFolderModSeq(ctx context.Context, folderID string) (int64, error) {
	return a.bumpFolderModSeqWithDB(ctx, a.db, folderID)
}

func (a *App) bumpFolderModSeqWithDB(ctx context.Context, db dbExecutor, folderID string) (int64, error) {
	if folderID == "" {
		return 0, nil
	}
	rowDB, ok := db.(dbQueryer)
	if !ok {
		return 0, nil
	}
	var current int64
	if err := rowDB.QueryRowContext(ctx, `SELECT highest_modseq FROM folders WHERE id=?`, folderID).Scan(&current); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}
	next := current + 1
	if next < 1 || next == math.MaxInt64 {
		next = current
	}
	if _, err := db.ExecContext(ctx, `UPDATE folders SET highest_modseq=MAX(highest_modseq,?) WHERE id=?`, next, folderID); err != nil {
		return 0, err
	}
	return next, nil
}

func (a *App) touchMessageIMAPModSeq(ctx context.Context, messageID string) error {
	var folderID sql.NullString
	if err := a.db.QueryRowContext(ctx, `SELECT folder_id FROM messages WHERE id=?`, messageID).Scan(&folderID); err != nil {
		return err
	}
	if !folderID.Valid || folderID.String == "" {
		return nil
	}
	modSeq, err := a.bumpFolderModSeq(ctx, folderID.String)
	if err != nil {
		return err
	}
	if modSeq == 0 {
		return nil
	}
	_, err = a.db.ExecContext(ctx, `UPDATE messages SET imap_modseq=? WHERE id=?`, modSeq, messageID)
	return err
}

func (a *App) updateMessageModSeq(ctx context.Context, messageID string, folderID string) (int64, error) {
	if folderID == "" {
		var dbFolderID sql.NullString
		if err := a.db.QueryRowContext(ctx, `SELECT folder_id FROM messages WHERE id=?`, messageID).Scan(&dbFolderID); err != nil {
			return 0, err
		}
		if !dbFolderID.Valid || dbFolderID.String == "" {
			return 0, nil
		}
		folderID = dbFolderID.String
	}
	modSeq, err := a.bumpFolderModSeq(ctx, folderID)
	if err != nil {
		return 0, err
	}
	if modSeq == 0 {
		return 0, nil
	}
	_, err = a.db.ExecContext(ctx, `UPDATE messages SET imap_modseq=? WHERE id=?`, modSeq, messageID)
	return modSeq, err
}
