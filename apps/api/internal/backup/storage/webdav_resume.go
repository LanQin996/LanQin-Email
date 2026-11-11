package storage

import (
	"context"
	"io"
	"net/http"
	"strings"
)

// Standard WebDAV has no interoperable byte-range append upload. Keep a stable
// exclusive object name and verify a potentially lost full PUT acknowledgement.
// A partial/conflicting existing object is NEVER unconditionally overwritten.
func (w *webdavUploader) resume(ctx context.Context, a *artifact, store CheckpointStore) (string, error) {
	state, err := loadTransfer(ctx, store, w.destination, w.binding, a)
	if err != nil {
		return "", err
	}
	if transport, ok := w.client.Transport.(*restrictedTransport); ok {
		transport.methods = map[string]bool{http.MethodGet: true, http.MethodPut: true}
	}
	u := *w.base
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + state.Name
	if state.Phase != "prepared" {
		found, err := w.verifyTransferObject(ctx, u.String(), a)
		if err != nil {
			return "", err
		}
		if found {
			return completeTransfer(ctx, store, w.destination.ID, state, state.Name)
		}
		if state.Phase == "completed" {
			return "", &uploadError{code: "remote_verification_failed"}
		}
	}
	state.Phase = "uploading"
	if err := saveTransfer(ctx, store, w.destination.ID, *state); err != nil {
		return "", err
	}
	if err := w.put(ctx, state.Name, io.NewSectionReader(a.file, 0, a.size), a); err != nil {
		return "", err
	}
	// Even a reported successful PUT needs a full readback before this durable
	// checkpoint is marked complete. Failure retains the stable name for retry.
	found, err := w.verifyTransferObject(ctx, u.String(), a)
	if err != nil {
		return "", err
	}
	if !found {
		return "", &uploadError{code: "upload_completion_uncertain", retry: true}
	}
	return completeTransfer(ctx, store, w.destination.ID, state, state.Name)
}

func (w *webdavUploader) verifyTransferObject(ctx context.Context, endpoint string, a *artifact) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, &uploadError{code: "invalid_request"}
	}
	req.SetBasicAuth(w.username, w.password)
	r, err := w.client.Do(req)
	if err != nil {
		return false, classify(err)
	}
	if r.StatusCode == http.StatusOK {
		if err := verifyTransferredBody(ctx, r.Body, a); err != nil {
			return false, err
		}
		return true, nil
	}
	r.Body.Close()
	if r.StatusCode == http.StatusNotFound {
		return false, nil
	}
	return false, statusError(r.StatusCode)
}
