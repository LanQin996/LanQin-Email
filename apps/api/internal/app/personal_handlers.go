package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

type MailboxApplyOptions struct {
	Enabled          bool     `json:"enabled"`
	Domains          []Domain `json:"domains"`
	ReservedPrefixes []string `json:"reservedPrefixes,omitempty"`
}

func (a *App) handleMailboxApplyOptions(w http.ResponseWriter, r *http.Request) {
	domains, err := a.mailboxApplyDomains(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load domains")
		return
	}
	respondJSON(w, http.StatusOK, MailboxApplyOptions{
		Enabled:          a.cfg.UserMailboxApplyEnabled,
		Domains:          domains,
		ReservedPrefixes: parseReservedPrefixes(a.cfg.ReservedMailboxPrefixes),
	})
}

func (a *App) handleApplyMailbox(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.UserMailboxApplyEnabled {
		respondError(w, http.StatusForbidden, "mailbox application is disabled")
		return
	}
	user := currentUser(r)
	var req struct {
		DomainID    string `json:"domainId"`
		LocalPart   string `json:"localPart"`
		DisplayName string `json:"displayName"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	domainID := strings.TrimSpace(req.DomainID)
	if domainID == "" {
		badRequest(w, errors.New("domainId is required"))
		return
	}
	allowed, err := a.mailboxApplyDomainAllowed(r.Context(), domainID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to check domain")
		return
	}
	if !allowed {
		respondError(w, http.StatusForbidden, "domain is not available")
		return
	}

	localPart := normalizeLocalPart(req.LocalPart)
	if localPart == "" {
		badRequest(w, errors.New("localPart is required"))
		return
	}
	if len(localPart) > 64 {
		badRequest(w, errors.New("localPart is too long"))
		return
	}
	reserved := map[string]bool{}
	for _, item := range parseReservedPrefixes(a.cfg.ReservedMailboxPrefixes) {
		reserved[item] = true
	}
	if reserved[localPart] {
		respondError(w, http.StatusForbidden, "localPart is reserved")
		return
	}
	var exists int
	if err := a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM mailboxes WHERE domain_id=? AND local_part=?`, domainID, localPart).Scan(&exists); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to check mailbox")
		return
	}
	if exists > 0 {
		respondError(w, http.StatusConflict, "mailbox already exists")
		return
	}

	var passwordHash string
	if err := a.db.QueryRowContext(r.Context(), `SELECT password_hash FROM users WHERE id=? AND disabled=0`, user.ID).Scan(&passwordHash); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load user")
		return
	}
	displayName := strings.TrimSpace(req.DisplayName)
	if len([]rune(displayName)) > 80 {
		badRequest(w, errors.New("displayName must be at most 80 characters"))
		return
	}
	mailboxID, err := a.createMailboxWithPasswordHash(r.Context(), user.ID, domainID, localPart, displayName, passwordHash, 1024, "active")
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			respondError(w, http.StatusConflict, "mailbox already exists")
			return
		}
		badRequest(w, err)
		return
	}
	mailbox, err := a.mailboxByID(r.Context(), mailboxID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load mailbox")
		return
	}
	respondJSON(w, http.StatusCreated, mailbox)
}

func (a *App) mailboxApplyDomains(ctx context.Context) ([]Domain, error) {
	if !a.cfg.UserMailboxApplyEnabled {
		return []Domain{}, nil
	}
	ids := cleanIDList(strings.Split(a.cfg.UserMailboxDomainIDs, ","))
	if len(ids) == 0 {
		return []Domain{}, nil
	}
	items := make([]Domain, 0, len(ids))
	for _, id := range ids {
		domain, err := a.domainByID(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if domain.Status == "active" {
			items = append(items, *domain)
		}
	}
	return items, nil
}

func (a *App) mailboxApplyDomainAllowed(ctx context.Context, domainID string) (bool, error) {
	domains, err := a.mailboxApplyDomains(ctx)
	if err != nil {
		return false, err
	}
	for _, domain := range domains {
		if domain.ID == domainID {
			return true, nil
		}
	}
	return false, nil
}

func (a *App) handleListContacts(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	rows, err := a.db.QueryContext(r.Context(), `SELECT id,user_id,name,email,note,created_at FROM contacts WHERE user_id=? ORDER BY name,email`, user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load contacts")
		return
	}
	defer rows.Close()
	items := []Contact{}
	for rows.Next() {
		item, err := scanContact(rows)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan contacts")
			return
		}
		items = append(items, item)
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleCreateContact(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	var req struct {
		Name  string `json:"name"`
		Email string `json:"email"`
		Note  string `json:"note"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	email := normalizeEmail(req.Email)
	if email == "" || !strings.Contains(email, "@") {
		badRequest(w, errors.New("invalid email"))
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = email
	}
	id := newID("ctc")
	now := a.now().UTC().Format(time.RFC3339Nano)
	_, err := a.db.ExecContext(r.Context(), `INSERT INTO contacts(id,user_id,name,email,note,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(user_id,email) DO UPDATE SET name=excluded.name,note=excluded.note,updated_at=excluded.updated_at`,
		id, user.ID, name, email, strings.TrimSpace(req.Note), now, now)
	if err != nil {
		badRequest(w, err)
		return
	}
	row := a.db.QueryRowContext(r.Context(), `SELECT id,user_id,name,email,note,created_at FROM contacts WHERE user_id=? AND email=?`, user.ID, email)
	item, err := scanContact(row)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load contact")
		return
	}
	respondJSON(w, http.StatusCreated, item)
}

func (a *App) handleDeleteContact(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	res, err := a.db.ExecContext(r.Context(), `DELETE FROM contacts WHERE id=? AND user_id=?`, chi.URLParam(r, "id"), user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete contact")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		respondError(w, http.StatusNotFound, "contact not found")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleListRules(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	rows, err := a.db.QueryContext(r.Context(), `SELECT id,user_id,mailbox_id,name,match_mode,conditions_json,actions_json,from_contains,subject_contains,action,apply_to_existing,stop_processing,enabled,created_at FROM mail_rules WHERE user_id=? ORDER BY created_at DESC`, user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load rules")
		return
	}
	defer rows.Close()
	items := []MailRule{}
	for rows.Next() {
		item, err := scanRule(rows)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan rules")
			return
		}
		items = append(items, item)
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	var req struct {
		MailboxID       string              `json:"mailboxId"`
		Name            string              `json:"name"`
		MatchMode       string              `json:"matchMode"`
		Conditions      []MailRuleCondition `json:"conditions"`
		Actions         []MailRuleAction    `json:"actions"`
		FromContains    string              `json:"fromContains"`
		SubjectContains string              `json:"subjectContains"`
		Action          string              `json:"action"`
		ApplyToExisting bool                `json:"applyToExisting"`
		StopProcessing  bool                `json:"stopProcessing"`
		Enabled         *bool               `json:"enabled"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	mailboxID, ok := a.optionalMailboxIDForUser(r, req.MailboxID)
	if !ok {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	matchMode := strings.TrimSpace(req.MatchMode)
	if matchMode == "" {
		matchMode = "all"
	}
	if matchMode != "all" && matchMode != "any" {
		badRequest(w, errors.New("invalid match mode"))
		return
	}
	conditions := normalizeRuleConditions(req.Conditions, req.FromContains, req.SubjectContains)
	if len(conditions) == 0 {
		badRequest(w, errors.New("rule condition is required"))
		return
	}
	actions := normalizeRuleActions(req.Actions, req.Action)
	if len(actions) == 0 {
		badRequest(w, errors.New("rule action is required"))
		return
	}
	conditionsJSON, err := json.Marshal(conditions)
	if err != nil {
		badRequest(w, err)
		return
	}
	actionsJSON, err := json.Marshal(actions)
	if err != nil {
		badRequest(w, err)
		return
	}
	fromContains := legacyConditionValue(conditions, "from")
	subjectContains := legacyConditionValue(conditions, "subject")
	action := actions[0].Type
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "收件规则"
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	id := newID("rule")
	now := a.now().UTC().Format(time.RFC3339Nano)
	_, err = a.db.ExecContext(r.Context(), `INSERT INTO mail_rules(id,user_id,mailbox_id,name,match_mode,conditions_json,actions_json,from_contains,subject_contains,action,apply_to_existing,stop_processing,enabled,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, user.ID, mailboxID, name, matchMode, string(conditionsJSON), string(actionsJSON), fromContains, subjectContains, action, boolInt(req.ApplyToExisting), boolInt(req.StopProcessing), boolInt(enabled), now, now)
	if err != nil {
		badRequest(w, err)
		return
	}
	appliedCount := int64(0)
	if req.ApplyToExisting && enabled {
		appliedCount, _ = a.applyRuleToExistingMessages(r.Context(), user.ID, mailboxID, MailRule{
			ID: id, UserID: user.ID, MailboxID: mailboxID, Name: name, MatchMode: matchMode,
			Conditions: conditions, Actions: actions, ApplyToExisting: req.ApplyToExisting, StopProcessing: req.StopProcessing,
			FromContains: fromContains, SubjectContains: subjectContains, Action: action, Enabled: enabled,
		})
	}
	row := a.db.QueryRowContext(r.Context(), `SELECT id,user_id,mailbox_id,name,match_mode,conditions_json,actions_json,from_contains,subject_contains,action,apply_to_existing,stop_processing,enabled,created_at FROM mail_rules WHERE id=?`, id)
	item, err := scanRule(row)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load rule")
		return
	}
	item.AppliedExistingCount = appliedCount
	respondJSON(w, http.StatusCreated, item)
}

func (a *App) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	res, err := a.db.ExecContext(r.Context(), `DELETE FROM mail_rules WHERE id=? AND user_id=?`, chi.URLParam(r, "id"), user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete rule")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		respondError(w, http.StatusNotFound, "rule not found")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleListBlockedSenders(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	rows, err := a.db.QueryContext(r.Context(), `SELECT id,user_id,mailbox_id,email,reason,created_at FROM blocked_senders WHERE user_id=? ORDER BY created_at DESC`, user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load blocked senders")
		return
	}
	defer rows.Close()
	items := []BlockedSender{}
	for rows.Next() {
		item, err := scanBlockedSender(rows)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan blocked senders")
			return
		}
		items = append(items, item)
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleCreateBlockedSender(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	var req struct {
		MailboxID string `json:"mailboxId"`
		Email     string `json:"email"`
		Reason    string `json:"reason"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	mailboxID, ok := a.optionalMailboxIDForUser(r, req.MailboxID)
	if !ok {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	email := normalizeEmail(req.Email)
	if email == "" || !strings.Contains(email, "@") {
		badRequest(w, errors.New("invalid email"))
		return
	}
	id := newID("blk")
	now := a.now().UTC().Format(time.RFC3339Nano)
	_, err := a.db.ExecContext(r.Context(), `INSERT INTO blocked_senders(id,user_id,mailbox_id,email,reason,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(user_id,mailbox_id,email) DO UPDATE SET reason=excluded.reason,updated_at=excluded.updated_at`,
		id, user.ID, mailboxID, email, strings.TrimSpace(req.Reason), now, now)
	if err != nil {
		badRequest(w, err)
		return
	}
	row := a.db.QueryRowContext(r.Context(), `SELECT id,user_id,mailbox_id,email,reason,created_at FROM blocked_senders WHERE user_id=? AND mailbox_id=? AND email=?`, user.ID, mailboxID, email)
	item, err := scanBlockedSender(row)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load blocked sender")
		return
	}
	respondJSON(w, http.StatusCreated, item)
}

func (a *App) handleDeleteBlockedSender(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	res, err := a.db.ExecContext(r.Context(), `DELETE FROM blocked_senders WHERE id=? AND user_id=?`, chi.URLParam(r, "id"), user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete blocked sender")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		respondError(w, http.StatusNotFound, "blocked sender not found")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleMailStats(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	mailboxID := strings.TrimSpace(r.URL.Query().Get("mailboxId"))
	args := []any{user.ID}
	where := `mb.user_id=?`
	if mailboxID != "" {
		if _, err := a.mailboxForCurrentUserWithID(r, mailboxID); err != nil {
			respondError(w, http.StatusNotFound, "mailbox not found")
			return
		}
		where += ` AND mb.id=?`
		args = append(args, mailboxID)
	}
	stats := MailStats{ByFolder: []MailStatsFolderCount{}}
	row := a.db.QueryRowContext(r.Context(), `SELECT COUNT(m.id),COALESCE(SUM(CASE WHEN m.is_read=0 THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN m.is_starred=1 THEN 1 ELSE 0 END),0),COALESCE(SUM(m.size_bytes),0)
		FROM mailboxes mb LEFT JOIN messages m ON m.mailbox_id=mb.id WHERE `+where, args...)
	if err := row.Scan(&stats.TotalMessages, &stats.UnreadMessages, &stats.StarredMessages, &stats.StorageBytes); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load stats")
		return
	}
	if err := a.db.QueryRowContext(r.Context(), `SELECT COUNT(a.id) FROM attachments a JOIN messages m ON m.id=a.message_id JOIN mailboxes mb ON mb.id=m.mailbox_id WHERE `+where, args...).Scan(&stats.AttachmentCount); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load attachment stats")
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT f.name,f.role,COUNT(m.id),COALESCE(SUM(CASE WHEN m.is_read=0 THEN 1 ELSE 0 END),0),COALESCE(SUM(m.size_bytes),0)
		FROM mailboxes mb JOIN folders f ON f.mailbox_id=mb.id LEFT JOIN messages m ON m.folder_id=f.id
		WHERE `+where+` GROUP BY f.id,f.name,f.role ORDER BY CASE f.role WHEN 'inbox' THEN 1 WHEN 'sent' THEN 2 WHEN 'drafts' THEN 3 WHEN 'archive' THEN 4 WHEN 'spam' THEN 5 WHEN 'trash' THEN 6 ELSE 99 END`, args...)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load folder stats")
		return
	}
	defer rows.Close()
	for rows.Next() {
		var item MailStatsFolderCount
		if err := rows.Scan(&item.Folder, &item.Role, &item.Count, &item.Unread, &item.Bytes); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan folder stats")
			return
		}
		stats.ByFolder = append(stats.ByFolder, item)
	}
	respondJSON(w, http.StatusOK, stats)
}

func (a *App) handleMailCleanup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MailboxID string `json:"mailboxId"`
		Target    string `json:"target"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	mb, err := a.mailboxForCurrentUserWithID(r, req.MailboxID)
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	target := strings.TrimSpace(req.Target)
	affected := int64(0)
	switch target {
	case "empty-trash":
		affected, err = a.deleteMessagesInFolder(r.Context(), mb.ID, "Trash")
	case "empty-spam":
		affected, err = a.deleteMessagesInFolder(r.Context(), mb.ID, "Spam")
	case "archive-read-inbox":
		affected, err = a.archiveReadInbox(r.Context(), mb.ID)
	default:
		badRequest(w, errors.New("invalid cleanup target"))
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to cleanup messages")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected})
}

func (a *App) optionalMailboxIDForUser(r *http.Request, mailboxID string) (string, bool) {
	mailboxID = strings.TrimSpace(mailboxID)
	if mailboxID == "" || mailboxID == "all" {
		return "", true
	}
	_, err := a.mailboxForCurrentUserWithID(r, mailboxID)
	return mailboxID, err == nil
}

func (a *App) deleteMessagesInFolder(ctx context.Context, mailboxID, folder string) (int64, error) {
	folderID, err := a.ensureFolder(ctx, mailboxID, folder)
	if err != nil {
		return 0, err
	}
	rows, err := a.db.QueryContext(ctx, `SELECT id FROM messages WHERE mailbox_id=? AND folder_id=?`, mailboxID, folderID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		a.deleteMessageFiles(ctx, id)
		if _, err := a.db.ExecContext(ctx, `DELETE FROM messages WHERE id=?`, id); err != nil {
			return 0, err
		}
	}
	return int64(len(ids)), nil
}

func (a *App) archiveReadInbox(ctx context.Context, mailboxID string) (int64, error) {
	inboxID, err := a.ensureFolder(ctx, mailboxID, "Inbox")
	if err != nil {
		return 0, err
	}
	archiveID, err := a.ensureFolder(ctx, mailboxID, "Archive")
	if err != nil {
		return 0, err
	}
	res, err := a.db.ExecContext(ctx, `UPDATE messages SET folder_id=?, updated_at=? WHERE mailbox_id=? AND folder_id=? AND is_read=1`,
		archiveID, a.now().UTC().Format(time.RFC3339Nano), mailboxID, inboxID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func scanContact(row messageSummaryScanner) (Contact, error) {
	var item Contact
	var created string
	err := row.Scan(&item.ID, &item.UserID, &item.Name, &item.Email, &item.Note, &created)
	item.CreatedAt = parseTime(created)
	return item, err
}

func scanRule(row messageSummaryScanner) (MailRule, error) {
	var item MailRule
	var enabled, applyToExisting, stopProcessing int
	var created string
	var conditionsJSON, actionsJSON string
	err := row.Scan(&item.ID, &item.UserID, &item.MailboxID, &item.Name, &item.MatchMode, &conditionsJSON, &actionsJSON, &item.FromContains, &item.SubjectContains, &item.Action, &applyToExisting, &stopProcessing, &enabled, &created)
	if err == nil {
		item.Conditions = decodeRuleConditions(conditionsJSON, item.FromContains, item.SubjectContains)
		item.Actions = decodeRuleActions(actionsJSON, item.Action)
		if item.MatchMode == "" {
			item.MatchMode = "all"
		}
	}
	item.ApplyToExisting = intBool(applyToExisting)
	item.StopProcessing = intBool(stopProcessing)
	item.Enabled = intBool(enabled)
	item.CreatedAt = parseTime(created)
	return item, err
}

func scanBlockedSender(row messageSummaryScanner) (BlockedSender, error) {
	var item BlockedSender
	var created string
	err := row.Scan(&item.ID, &item.UserID, &item.MailboxID, &item.Email, &item.Reason, &created)
	item.CreatedAt = parseTime(created)
	return item, err
}

func (a *App) applyInboundControls(ctx context.Context, messageID, mailboxID, from, subject string) {
	var userID string
	if err := a.db.QueryRowContext(ctx, `SELECT user_id FROM mailboxes WHERE id=?`, mailboxID).Scan(&userID); err != nil {
		return
	}
	from = normalizeEmail(from)
	var blocked int
	_ = a.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM blocked_senders WHERE user_id=? AND (mailbox_id='' OR mailbox_id=?) AND email=?`, userID, mailboxID, from).Scan(&blocked)
	if blocked > 0 {
		if spamID, err := a.ensureFolder(ctx, mailboxID, "Spam"); err == nil {
			_, _ = a.db.ExecContext(ctx, `UPDATE messages SET folder_id=?, updated_at=? WHERE id=?`, spamID, a.now().UTC().Format(time.RFC3339Nano), messageID)
		}
		return
	}
	rows, err := a.db.QueryContext(ctx, `SELECT id,user_id,mailbox_id,name,match_mode,conditions_json,actions_json,from_contains,subject_contains,action,apply_to_existing,stop_processing,enabled,created_at FROM mail_rules WHERE user_id=? AND (mailbox_id='' OR mailbox_id=?) AND enabled=1 ORDER BY created_at`, userID, mailboxID)
	if err != nil {
		return
	}
	rules := []MailRule{}
	for rows.Next() {
		rule, err := scanRule(rows)
		if err == nil {
			rules = append(rules, rule)
		}
	}
	rows.Close()
	msg := ruleMessage{ID: messageID, MailboxID: mailboxID, From: from, Subject: subject}
	_ = a.db.QueryRowContext(ctx, `SELECT from_addr,to_addrs,subject,snippet,body_text FROM messages WHERE id=?`, messageID).Scan(&msg.From, &msg.To, &msg.Subject, &msg.Snippet, &msg.BodyText)
	for _, rule := range rules {
		if !ruleMatches(rule, msg) {
			continue
		}
		_ = a.applyRuleActions(ctx, mailboxID, messageID, rule.Actions)
		if rule.StopProcessing {
			return
		}
	}
}

type ruleMessage struct {
	ID        string
	MailboxID string
	From      string
	To        string
	Subject   string
	Snippet   string
	BodyText  string
}

func normalizeRuleConditions(items []MailRuleCondition, legacyFrom, legacySubject string) []MailRuleCondition {
	if len(items) == 0 {
		if strings.TrimSpace(legacyFrom) != "" {
			items = append(items, MailRuleCondition{Field: "from", Operator: "contains", Value: legacyFrom})
		}
		if strings.TrimSpace(legacySubject) != "" {
			items = append(items, MailRuleCondition{Field: "subject", Operator: "contains", Value: legacySubject})
		}
	}
	out := []MailRuleCondition{}
	for _, item := range items {
		field := strings.TrimSpace(item.Field)
		operator := strings.TrimSpace(item.Operator)
		value := strings.TrimSpace(item.Value)
		if value == "" {
			continue
		}
		if field != "from" && field != "to" && field != "subject" && field != "body" {
			continue
		}
		if operator == "" {
			operator = "contains"
		}
		if operator != "contains" && operator != "not-contains" && operator != "equals" && operator != "not-equals" && operator != "starts-with" && operator != "ends-with" {
			continue
		}
		out = append(out, MailRuleCondition{Field: field, Operator: operator, Value: value})
	}
	return out
}

func normalizeRuleActions(items []MailRuleAction, legacyAction string) []MailRuleAction {
	if len(items) == 0 && strings.TrimSpace(legacyAction) != "" {
		items = append(items, MailRuleAction{Type: strings.TrimSpace(legacyAction)})
	}
	out := []MailRuleAction{}
	for _, item := range items {
		typ := strings.TrimSpace(item.Type)
		value := strings.TrimSpace(item.Value)
		labelID := strings.TrimSpace(item.LabelID)
		if typ != "archive" && typ != "trash" && typ != "star" && typ != "mark-read" && typ != "label" && typ != "move" {
			continue
		}
		if typ == "label" && value == "" && labelID == "" {
			continue
		}
		if typ == "move" && value == "" {
			continue
		}
		out = append(out, MailRuleAction{Type: typ, Value: value, LabelID: labelID})
	}
	return out
}

func legacyConditionValue(items []MailRuleCondition, field string) string {
	for _, item := range items {
		if item.Field == field && item.Operator == "contains" {
			return item.Value
		}
	}
	return ""
}

func decodeRuleConditions(raw, legacyFrom, legacySubject string) []MailRuleCondition {
	var items []MailRuleCondition
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &items)
	}
	return normalizeRuleConditions(items, legacyFrom, legacySubject)
}

func decodeRuleActions(raw, legacyAction string) []MailRuleAction {
	var items []MailRuleAction
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &items)
	}
	return normalizeRuleActions(items, legacyAction)
}

func ruleMatches(rule MailRule, msg ruleMessage) bool {
	conditions := rule.Conditions
	if len(conditions) == 0 {
		conditions = normalizeRuleConditions(nil, rule.FromContains, rule.SubjectContains)
	}
	if len(conditions) == 0 {
		return false
	}
	matchMode := rule.MatchMode
	if matchMode == "" {
		matchMode = "all"
	}
	matched := 0
	for _, condition := range conditions {
		if ruleConditionMatches(condition, msg) {
			matched++
			if matchMode == "any" {
				return true
			}
		} else if matchMode == "all" {
			return false
		}
	}
	return matched == len(conditions)
}

func ruleConditionMatches(condition MailRuleCondition, msg ruleMessage) bool {
	var source string
	switch condition.Field {
	case "from":
		source = msg.From
	case "to":
		source = msg.To
	case "subject":
		source = msg.Subject
	case "body":
		source = msg.BodyText
		if source == "" {
			source = msg.Snippet
		}
	default:
		return false
	}
	source = strings.ToLower(source)
	value := strings.ToLower(condition.Value)
	switch condition.Operator {
	case "contains":
		return strings.Contains(source, value)
	case "not-contains":
		return !strings.Contains(source, value)
	case "equals":
		return source == value
	case "not-equals":
		return source != value
	case "starts-with":
		return strings.HasPrefix(source, value)
	case "ends-with":
		return strings.HasSuffix(source, value)
	default:
		return strings.Contains(source, value)
	}
}

func (a *App) applyRuleActions(ctx context.Context, mailboxID, messageID string, actions []MailRuleAction) error {
	now := a.now().UTC().Format(time.RFC3339Nano)
	for _, action := range normalizeRuleActions(actions, "") {
		switch action.Type {
		case "archive":
			if folderID, err := a.ensureFolder(ctx, mailboxID, "Archive"); err == nil {
				if _, err := a.db.ExecContext(ctx, `UPDATE messages SET folder_id=?, updated_at=? WHERE id=?`, folderID, now, messageID); err != nil {
					return err
				}
			}
		case "trash":
			if folderID, err := a.ensureFolder(ctx, mailboxID, "Trash"); err == nil {
				if _, err := a.db.ExecContext(ctx, `UPDATE messages SET folder_id=?, updated_at=? WHERE id=?`, folderID, now, messageID); err != nil {
					return err
				}
			}
		case "move":
			target := ruleTargetFolder(action.Value)
			if folderID, err := a.ensureFolder(ctx, mailboxID, target); err == nil {
				if _, err := a.db.ExecContext(ctx, `UPDATE messages SET folder_id=?, updated_at=? WHERE id=?`, folderID, now, messageID); err != nil {
					return err
				}
			}
		case "star":
			if _, err := a.db.ExecContext(ctx, `UPDATE messages SET is_starred=1, updated_at=? WHERE id=?`, now, messageID); err != nil {
				return err
			}
		case "mark-read":
			if _, err := a.db.ExecContext(ctx, `UPDATE messages SET is_read=1, updated_at=? WHERE id=?`, now, messageID); err != nil {
				return err
			}
		case "label":
			if err := a.applyRuleLabel(ctx, mailboxID, messageID, action); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *App) applyRuleLabel(ctx context.Context, mailboxID, messageID string, action MailRuleAction) error {
	var label MailLabel
	var err error
	if action.LabelID != "" {
		var count int
		_ = a.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM mail_labels WHERE id=? AND mailbox_id=?`, action.LabelID, mailboxID).Scan(&count)
		if count > 0 {
			label.ID = action.LabelID
		}
	}
	if label.ID == "" {
		name := strings.TrimSpace(action.Value)
		if name == "" {
			name = "规则标签"
		}
		label, err = a.ensureLabel(ctx, mailboxID, name, "")
		if err != nil {
			return err
		}
	}
	_, err = a.db.ExecContext(ctx, `INSERT OR IGNORE INTO message_labels(message_id,label_id,created_at) VALUES(?,?,?)`, messageID, label.ID, a.now().UTC().Format(time.RFC3339Nano))
	return err
}

func ruleTargetFolder(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "inbox":
		return "Inbox"
	case "archive":
		return "Archive"
	case "spam":
		return "Spam"
	case "trash":
		return "Trash"
	default:
		return "Archive"
	}
}

func (a *App) applyRuleToExistingMessages(ctx context.Context, userID, mailboxID string, rule MailRule) (int64, error) {
	args := []any{userID}
	where := `mb.user_id=?`
	if mailboxID != "" {
		where += ` AND m.mailbox_id=?`
		args = append(args, mailboxID)
	}
	rows, err := a.db.QueryContext(ctx, `SELECT m.id,m.mailbox_id,m.from_addr,m.to_addrs,m.subject,m.snippet,m.body_text FROM messages m JOIN mailboxes mb ON mb.id=m.mailbox_id WHERE `+where, args...)
	if err != nil {
		return 0, err
	}
	messages := []ruleMessage{}
	var count int64
	for rows.Next() {
		var msg ruleMessage
		var toAddrs sql.NullString
		if err := rows.Scan(&msg.ID, &msg.MailboxID, &msg.From, &toAddrs, &msg.Subject, &msg.Snippet, &msg.BodyText); err != nil {
			return count, err
		}
		msg.To = toAddrs.String
		if !ruleMatches(rule, msg) {
			continue
		}
		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		return count, err
	}
	rows.Close()
	for _, msg := range messages {
		if err := a.applyRuleActions(ctx, msg.MailboxID, msg.ID, rule.Actions); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}
