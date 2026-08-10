package app

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	webmailSearchColumns = "subject from_addr from_name snippet body_text"
	openAPISearchColumns = "subject from_addr from_name to_addrs snippet body_text"
	adminSearchColumns   = "subject from_addr from_name to_addrs recipient_addr snippet body_text"
)

func (a *App) ensureMessageSearchFTS(ctx context.Context) error {
	if a.cfg.DBDriver != databaseDriverSQLite {
		a.messageSearchFTS = false
		return nil
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='messages_fts'`).Scan(&exists); err != nil {
		return err
	}
	created := exists == 0
	if created {
		_, err := tx.ExecContext(ctx, `CREATE VIRTUAL TABLE messages_fts USING fts5(
			subject, from_addr, from_name, to_addrs, recipient_addr, snippet, body_text,
			content='messages', content_rowid='rowid', tokenize='trigram'
		)`)
		if err != nil {
			if isFTSUnavailableError(err) {
				a.log.Warn("SQLite FTS5 unavailable; falling back to LIKE message search", "error", err)
				return nil
			}
			return fmt.Errorf("create message search index: %w", err)
		}
	}

	triggers := []string{
		`CREATE TRIGGER IF NOT EXISTS messages_fts_ai AFTER INSERT ON messages BEGIN
			INSERT INTO messages_fts(rowid,subject,from_addr,from_name,to_addrs,recipient_addr,snippet,body_text)
			VALUES(new.rowid,new.subject,new.from_addr,new.from_name,new.to_addrs,new.recipient_addr,new.snippet,new.body_text);
		END`,
		`CREATE TRIGGER IF NOT EXISTS messages_fts_ad AFTER DELETE ON messages BEGIN
			INSERT INTO messages_fts(messages_fts,rowid,subject,from_addr,from_name,to_addrs,recipient_addr,snippet,body_text)
			VALUES('delete',old.rowid,old.subject,old.from_addr,old.from_name,old.to_addrs,old.recipient_addr,old.snippet,old.body_text);
		END`,
		`CREATE TRIGGER IF NOT EXISTS messages_fts_au AFTER UPDATE OF subject,from_addr,from_name,to_addrs,recipient_addr,snippet,body_text ON messages BEGIN
			INSERT INTO messages_fts(messages_fts,rowid,subject,from_addr,from_name,to_addrs,recipient_addr,snippet,body_text)
			VALUES('delete',old.rowid,old.subject,old.from_addr,old.from_name,old.to_addrs,old.recipient_addr,old.snippet,old.body_text);
			INSERT INTO messages_fts(rowid,subject,from_addr,from_name,to_addrs,recipient_addr,snippet,body_text)
			VALUES(new.rowid,new.subject,new.from_addr,new.from_name,new.to_addrs,new.recipient_addr,new.snippet,new.body_text);
		END`,
	}
	for _, stmt := range triggers {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("create message search trigger: %w", err)
		}
	}
	if created {
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages_fts(messages_fts) VALUES('rebuild')`); err != nil {
			return fmt.Errorf("backfill message search index: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS idx_messages_search`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	a.messageSearchFTS = true
	return nil
}

func isFTSUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such module: fts5") ||
		strings.Contains(message, "no such tokenizer: trigram") ||
		strings.Contains(message, "error in tokenizer constructor")
}

func (a *App) canUseMessageFTS(query string) bool {
	return a.messageSearchFTS && utf8.RuneCountInString(query) >= 3
}

func messageFTSLiteralQuery(query, columns string) string {
	literal := strings.ReplaceAll(query, `"`, `""`)
	return "{" + columns + `} : "` + literal + `"`
}
