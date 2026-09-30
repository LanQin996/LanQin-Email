package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// Serialize configuration and resource lifecycle writes. External deployments
// use a database advisory lock so multiple API replicas share the same boundary.
func (a *App) collectionMutationLock(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.collectionMu.Lock()
		defer a.collectionMu.Unlock()
		if a.cfg.DBDriver != databaseDriverSQLite {
			// A dedicated connection avoids holding the final pooled connection
			// while the handler needs that pool to perform the protected write.
			lockDB, err := openDatabase(r.Context(), a.cfg)
			if err != nil {
				respondError(w, 503, "collection configuration busy")
				return
			}
			defer lockDB.Close()
			conn, err := lockDB.Conn(r.Context())
			if err != nil {
				respondError(w, 503, "collection configuration busy")
				return
			}
			defer conn.Close()
			unlock, err := lockExternalSchema(r.Context(), conn, a.cfg.DBDriver)
			if err != nil {
				respondError(w, 503, "collection configuration busy")
				return
			}
			defer unlock()
		}
		next.ServeHTTP(w, r)
	})
}

// Shared by SQLite and external schema migrations. IDs, not arbitrary addresses,
// keep the collection destination inside this installation.
func domainCollectionSchema() []string {
	return []string{
		`CREATE TABLE domain_collections (
   domain_id VARCHAR(64) PRIMARY KEY REFERENCES domains(id) ON DELETE CASCADE,
   target_mailbox_id VARCHAR(64) NOT NULL REFERENCES mailboxes(id) ON DELETE RESTRICT,
   updated_at VARCHAR(35) NOT NULL
  )`,
		`CREATE TABLE domain_collection_audit (
   id VARCHAR(64) PRIMARY KEY, domain_id VARCHAR(64) NOT NULL,
   actor_id VARCHAR(64) NOT NULL, old_target_id VARCHAR(64) NOT NULL,
   new_target_id VARCHAR(64) NOT NULL, created_at VARCHAR(35) NOT NULL
  )`,
		`CREATE TABLE local_delivery_jobs (
   id VARCHAR(64) PRIMARY KEY, source_id VARCHAR(64) NOT NULL,
   target_mailbox_id VARCHAR(64) NOT NULL,
   payload TEXT NOT NULL, status VARCHAR(16) NOT NULL,
   attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at VARCHAR(35) NOT NULL,
   lease_token VARCHAR(64) NOT NULL DEFAULT '', lease_until VARCHAR(35) NOT NULL DEFAULT '',
   message_id VARCHAR(64) NOT NULL DEFAULT '', last_error VARCHAR(255) NOT NULL DEFAULT '',
   created_at VARCHAR(35) NOT NULL, updated_at VARCHAR(35) NOT NULL,
   UNIQUE(source_id,target_mailbox_id)
  )`,
		`CREATE INDEX idx_local_delivery_ready ON local_delivery_jobs(status,next_attempt_at)`,
	}
}

type domainCollection struct {
	Enabled         bool   `json:"enabled"`
	TargetMailboxID string `json:"targetMailboxId"`
}

func (a *App) handleGetDomainCollection(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := a.domainByID(r.Context(), id); err != nil {
		respondError(w, 404, "domain not found")
		return
	}
	result := domainCollection{}
	err := a.db.QueryRowContext(r.Context(), "SELECT target_mailbox_id FROM domain_collections WHERE domain_id=?", id).Scan(&result.TargetMailboxID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		respondError(w, 500, "failed to load collection")
		return
	}
	result.Enabled = err == nil
	respondJSON(w, 200, result)
}

func (a *App) handleCollectionTargets(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), `SELECT m.id,m.address FROM mailboxes m
 JOIN domains d ON d.id=m.domain_id JOIN users u ON u.id=m.user_id
 WHERE m.status='active' AND d.status='active' AND u.disabled=0
 AND NOT EXISTS (SELECT 1 FROM aliases al WHERE LOWER(al.source)=LOWER(m.address) AND al.enabled=1 AND LOWER(al.destination)<>LOWER(m.address)) ORDER BY m.address`)
	if err != nil {
		respondError(w, 500, "failed to load collection targets")
		return
	}
	defer rows.Close()
	type target struct {
		ID      string `json:"id"`
		Address string `json:"address"`
	}
	items := []target{}
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.ID, &t.Address); err != nil {
			respondError(w, 500, "failed to load collection targets")
			return
		}
		items = append(items, t)
	}
	if rows.Err() != nil {
		respondError(w, 500, "failed to load collection targets")
		return
	}
	respondJSON(w, 200, map[string]any{"items": items})
}

func (a *App) handleSetDomainCollection(w http.ResponseWriter, r *http.Request) {
	var req domainCollection
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	id := chi.URLParam(r, "id")
	req.TargetMailboxID = strings.TrimSpace(req.TargetMailboxID)
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		respondError(w, 500, "failed to start transaction")
		return
	}
	defer tx.Rollback()
	// Acquire the domain write lock before checking the destination or current rule.
	_, err = tx.ExecContext(r.Context(), "UPDATE domains SET status=status WHERE id=?", id)
	if err != nil {
		respondError(w, 500, "failed to lock domain")
		return
	}
	var exists int
	if err := tx.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM domains WHERE id=?", id).Scan(&exists); err != nil {
		respondError(w, 500, "failed to load domain")
		return
	}
	if exists == 0 {
		respondError(w, 404, "domain not found")
		return
	}
	if req.Enabled {
		var count int
		if err := tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM domains s,mailboxes m
    JOIN domains d ON d.id=m.domain_id JOIN users u ON u.id=m.user_id
    WHERE s.id=? AND s.status='active' AND m.id=? AND m.status='active' AND d.status='active' AND u.disabled=0
    AND NOT EXISTS (SELECT 1 FROM aliases al WHERE LOWER(al.source)=LOWER(m.address) AND al.enabled=1 AND LOWER(al.destination)<>LOWER(m.address))`, id, req.TargetMailboxID).Scan(&count); err != nil {
			respondError(w, 500, "failed to validate collection target")
			return
		}
		if count != 1 {
			respondError(w, 400, "collection target must be an active local mailbox without a forwarding alias")
			return
		}
	}
	old := ""
	err = tx.QueryRowContext(r.Context(), "SELECT target_mailbox_id FROM domain_collections WHERE domain_id=?", id).Scan(&old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		respondError(w, 500, "failed to load collection")
		return
	}
	next := ""
	if req.Enabled {
		next = req.TargetMailboxID
	}
	if old == next {
		respondJSON(w, 200, domainCollection{Enabled: next != "", TargetMailboxID: next})
		return
	}
	if _, err = tx.ExecContext(r.Context(), "DELETE FROM domain_collections WHERE domain_id=?", id); err != nil {
		respondError(w, 500, "failed to update collection")
		return
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	if req.Enabled {
		if _, err = tx.ExecContext(r.Context(), "INSERT INTO domain_collections(domain_id,target_mailbox_id,updated_at) VALUES(?,?,?)", id, next, now); err != nil {
			respondError(w, 500, "failed to update collection")
			return
		}
	}
	if _, err = tx.ExecContext(r.Context(), "INSERT INTO domain_collection_audit(id,domain_id,actor_id,old_target_id,new_target_id,created_at) VALUES(?,?,?,?,?,?)", newID("dca"), id, currentUser(r).ID, old, next, now); err != nil {
		respondError(w, 500, "failed to audit collection")
		return
	}
	if err = tx.Commit(); err != nil {
		respondError(w, 500, "failed to save collection")
		return
	}
	respondJSON(w, 200, domainCollection{Enabled: next != "", TargetMailboxID: next})
}

// Guards are applied before destructive handlers remove associated mail or files.
func (a *App) collectionTargetInUse(ctx context.Context, kind, id string) error {
	clauses := map[string]string{"mailbox": "m.id=?", "user": "m.user_id=?", "domain": "m.domain_id=?"}
	clause, ok := clauses[kind]
	if !ok {
		return errors.New("invalid collection resource")
	}
	var n int
	if err := a.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM domain_collections c JOIN mailboxes m ON m.id=c.target_mailbox_id WHERE "+clause, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errCollectionTargetInUse
	}
	return nil
}

func (a *App) collectionAliasAllowed(ctx context.Context, source, destination string, enabled bool) error {
	if !enabled || strings.EqualFold(source, destination) {
		return nil
	}
	var n int
	if err := a.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM domain_collections c JOIN mailboxes m ON m.id=c.target_mailbox_id WHERE LOWER(m.address)=?", normalizeEmail(source)).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errCollectionAlias
	}
	return nil
}

var errCollectionTargetInUse = errors.New("resource is a unified collection target; change or disable collection first")
var errCollectionAlias = errors.New("collection target cannot be a forwarding alias")

func respondCollectionGuardError(w http.ResponseWriter, err error) {
	if errors.Is(err, errCollectionTargetInUse) || errors.Is(err, errCollectionAlias) {
		respondError(w, http.StatusConflict, err.Error())
		return
	}
	respondError(w, http.StatusInternalServerError, "failed to validate collection target")
}
