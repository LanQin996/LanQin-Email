package app

import (
	"context"
	"database/sql"
	"strings"
)

// normalizeMessageID accepts a single RFC 5322 msg-id and removes harmless
// formatting differences while rejecting malformed values.
func normalizeMessageID(value string) string {
	if strings.ContainsAny(value, "\r\n") {
		return ""
	}
	v := strings.TrimSpace(value)
	v = strings.Trim(v, "<>")
	v = strings.Join(strings.Fields(v), "")
	if v == "" || !strings.Contains(v, "@") || strings.ContainsAny(v, "<>\r\n,") {
		return ""
	}
	return strings.ToLower(v)
}

func messageIDList(value string) []string {
	seen := map[string]bool{}
	var out []string
	for _, token := range strings.Fields(value) {
		if id := normalizeMessageID(strings.Trim(token, ",;")); id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func (a *App) resolveThreadID(ctx context.Context, mailboxID, messageID, inReplyTo, references string) string {
	return a.resolveThreadIDWithDB(ctx, a.db, mailboxID, messageID, inReplyTo, references)
}

func (a *App) resolveThreadIDWithDB(ctx context.Context, db dbExecutor, mailboxID, messageID, inReplyTo, references string) string {
	mailboxID = strings.TrimSpace(mailboxID)
	if mailboxID == "" {
		return ""
	}
	ids := append(messageIDList(references), messageIDList(inReplyTo)...)
	if q, ok := db.(interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}); ok {
		for _, ref := range ids {
			var existing string
			if err := q.QueryRowContext(ctx, `SELECT COALESCE(thread_id,'') FROM messages WHERE mailbox_id=? AND lower(trim(message_id,'<>'))=? LIMIT 1`, mailboxID, ref).Scan(&existing); err == nil && strings.TrimSpace(existing) != "" {
				return existing
			}
		}
	}
	if len(ids) > 0 {
		return ids[0]
	}
	return normalizeMessageID(messageID)
}
