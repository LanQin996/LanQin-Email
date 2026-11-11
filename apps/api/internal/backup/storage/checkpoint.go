package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"time"
)

// CheckpointStore MUST authenticate/encrypt stored state and serialize concurrent
// invocations of the same upload. Save must reject a lost lease. Session URLs and
// upload IDs are sensitive, and must never appear in user reports or logs.
type CheckpointStore interface {
	Load(context.Context, string) (*Checkpoint, error)
	Save(context.Context, string, Checkpoint) error
}

const transferPartBytes int64 = 16 << 20

type Checkpoint struct {
	Version      int              `json:"version"`
	Type         string           `json:"type"`
	Binding      string           `json:"binding"`
	SHA256       string           `json:"sha256"`
	Bytes        int64            `json:"bytes"`
	Name         string           `json:"name"`
	Phase        string           `json:"phase"`
	UploadID     string           `json:"uploadId,omitempty"`
	Session      string           `json:"session,omitempty"`
	FileID       string           `json:"fileId,omitempty"`
	Offset       int64            `json:"offset,omitempty"`
	Parts        []CheckpointPart `json:"parts,omitempty"`
	ObjectKey    string           `json:"objectKey,omitempty"`
	CompletedAt  *time.Time       `json:"completedAt,omitempty"`
	CleanupError string           `json:"cleanupError,omitempty"`
}

type CheckpointPart struct {
	Number int32  `json:"number"`
	Bytes  int64  `json:"bytes"`
	MD5    string `json:"md5"`
	ETag   string `json:"etag"`
}

type resumableUploader interface {
	resume(context.Context, *artifact, CheckpointStore) (string, error)
}

func transferBinding(d Destination, c Credentials) string {
	d.Name, d.Disabled = "", false
	// Short-lived Drive tokens rotate. The app additionally binds its store to
	// the immutable authorization generation; CLI binds the explicit folder/ID.
	if d.Type == "drive" {
		c.DriveAccessToken = ""
	}
	data, _ := json.Marshal(struct {
		Destination Destination
		Credentials Credentials
	}{d, c})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func loadTransfer(ctx context.Context, store CheckpointStore, d Destination, binding string, a *artifact) (*Checkpoint, error) {
	state, err := store.Load(ctx, d.ID)
	if err != nil {
		return nil, &uploadError{code: "checkpoint_unavailable"}
	}
	if state == nil {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, &uploadError{code: "random_source_failed"}
		}
		state = &Checkpoint{Version: 1, Type: d.Type, Binding: binding, SHA256: a.sha256, Bytes: a.size, Name: "lanqin-" + hex.EncodeToString(nonce[:]) + ".age", Phase: "prepared"}
		if err := saveTransfer(ctx, store, d.ID, *state); err != nil {
			return nil, err
		}
	}
	if state.Version != 1 || state.Type != d.Type || state.Binding != binding || state.SHA256 != a.sha256 || state.Bytes != a.size || !backupObjectName.MatchString(state.Name) || len(state.UploadID) > 2048 || len(state.Session) > 8192 || len(state.Parts) > 1024 || state.Offset < 0 || state.Offset > a.size {
		return nil, &uploadError{code: "checkpoint_mismatch"}
	}
	switch state.Phase {
	case "prepared", "creating", "uploading", "completing", "completed", "aborted":
	default:
		return nil, &uploadError{code: "checkpoint_mismatch"}
	}
	if state.Phase == "aborted" {
		return nil, &uploadError{code: "upload_aborted"}
	}
	if state.ObjectKey != "" && (d.Type == "drive" && !ValidDriveID(state.ObjectKey) || d.Type != "drive" && state.ObjectKey != objectKey(d, state.Name)) {
		return nil, &uploadError{code: "checkpoint_mismatch"}
	}
	return state, nil
}

func saveTransfer(ctx context.Context, store CheckpointStore, id string, state Checkpoint) error {
	if ctx.Err() != nil {
		return classify(ctx.Err())
	}
	if err := store.Save(ctx, id, state); err != nil {
		return &uploadError{code: "checkpoint_unavailable"}
	}
	return nil
}

func completeTransfer(ctx context.Context, store CheckpointStore, id string, state *Checkpoint, object string) (string, error) {
	now := time.Now().UTC()
	state.Phase, state.ObjectKey, state.CompletedAt = "completed", object, &now
	if err := saveTransfer(ctx, store, id, *state); err != nil {
		return "", err
	}
	return object, nil
}

func verifyTransferredBody(ctx context.Context, body io.ReadCloser, a *artifact) error {
	defer body.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(contextReader{ctx, body}, a.size+1))
	if err != nil {
		return classify(err)
	}
	if n != a.size || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), a.sha256) {
		return &uploadError{code: "remote_verification_failed"}
	}
	return nil
}
