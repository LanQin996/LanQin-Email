package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const driveUploadEndpoint = "https://www.googleapis.com/upload/drive/v3/files"

type driveTransferResult struct {
	ID            string            `json:"id"`
	Size          string            `json:"size"`
	MD5           string            `json:"md5Checksum"`
	Parents       []string          `json:"parents"`
	AppProperties map[string]string `json:"appProperties"`
}

func validDriveSession(endpoint string) bool {
	if len(endpoint) > 8192 {
		return false
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host != "www.googleapis.com" || u.User != nil || u.Fragment != "" || u.Path != "/upload/drive/v3/files" || u.RawPath != "" {
		return false
	}
	query, err := url.ParseQuery(u.RawQuery)
	return err == nil && len(query["upload_id"]) == 1 && validTransferToken(query.Get("upload_id"), 4096)
}

func driveAcknowledgedOffset(r *http.Response, total int64) (int64, error) {
	value := r.Header.Get("Range")
	if value == "" {
		return 0, nil
	}
	if !strings.HasPrefix(value, "bytes=0-") {
		return 0, &uploadError{code: "invalid_upload_progress"}
	}
	last, err := strconv.ParseInt(strings.TrimPrefix(value, "bytes=0-"), 10, 64)
	if err != nil || last < 0 || last >= total {
		return 0, &uploadError{code: "invalid_upload_progress"}
	}
	return last + 1, nil
}

func (d *driveUploader) resume(ctx context.Context, a *artifact, store CheckpointStore) (string, error) {
	state, err := loadTransfer(ctx, store, d.config, d.binding, a)
	if err != nil {
		return "", err
	}
	if state.FileID != "" && !ValidDriveID(state.FileID) {
		return "", &uploadError{code: "checkpoint_mismatch"}
	}
	if state.Phase == "completed" || state.Phase == "creating" || state.Phase == "completing" {
		found, err := d.verifyTransferObject(ctx, state, a)
		if err != nil {
			return "", err
		}
		if found {
			return completeTransfer(ctx, store, d.destination, state, state.FileID)
		}
		if state.Phase == "completed" {
			return "", &uploadError{code: "remote_verification_failed"}
		}
	}
	if state.FileID == "" {
		if state.Phase != "prepared" {
			return "", &uploadError{code: "checkpoint_mismatch"}
		}
		// A preallocated provider ID makes session-creation retries address the
		// SAME logical file even if the initial Location response was lost.
		r, err := driveRequest(ctx, d.client, http.MethodGet, driveFiles+"/generateIds?count=1&space=drive&type=files", d.token, nil)
		if err != nil {
			return "", err
		}
		if r.StatusCode != http.StatusOK {
			r.Body.Close()
			return "", statusError(r.StatusCode)
		}
		var ids struct {
			IDs []string `json:"ids"`
		}
		if err := driveJSON(r, &ids); err != nil || len(ids.IDs) != 1 || !ValidDriveID(ids.IDs[0]) {
			return "", &uploadError{code: "invalid_provider_response"}
		}
		state.FileID = ids.IDs[0]
		if err := saveTransfer(ctx, store, d.destination, *state); err != nil {
			return "", err
		}
	}
	if state.Session == "" {
		state.Phase = "creating"
		if err := saveTransfer(ctx, store, d.destination, *state); err != nil {
			return "", err
		}
		metadata, _ := json.Marshal(map[string]any{"id": state.FileID, "name": state.Name, "parents": []string{d.folder}, "appProperties": map[string]string{"lanqinDestination": d.destination, "lanqinSHA256": a.sha256, "lanqinTransfer": state.Name}})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, driveUploadEndpoint+"?uploadType=resumable&fields=id,size,md5Checksum,appProperties", bytes.NewReader(metadata))
		if err != nil {
			return "", &uploadError{code: "invalid_request"}
		}
		req.Header.Set("Authorization", "Bearer "+d.token)
		req.Header.Set("Content-Type", "application/json; charset=UTF-8")
		req.Header.Set("X-Upload-Content-Type", "application/octet-stream")
		req.Header.Set("X-Upload-Content-Length", strconv.FormatInt(a.size, 10))
		r, err := d.client.Do(req)
		if err != nil {
			return "", classify(err)
		}
		location := r.Header.Get("Location")
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			return "", statusError(r.StatusCode)
		}
		if !validDriveSession(location) {
			return "", &uploadError{code: "invalid_upload_session"}
		}
		state.Session, state.Phase, state.Offset = location, "uploading", 0
		if err := saveTransfer(ctx, store, d.destination, *state); err != nil {
			return "", err
		}
	}
	if !validDriveSession(state.Session) {
		return "", &uploadError{code: "invalid_upload_session"}
	}
	// The server, not a saved local offset, decides what actually arrived.
	status, err := d.transferPut(ctx, state.Session, "bytes */"+strconv.FormatInt(a.size, 10), http.NoBody, 0)
	if err != nil {
		return "", err
	}
	if status.StatusCode == http.StatusOK || status.StatusCode == http.StatusCreated {
		return d.finishResumable(ctx, status, state, a, store)
	}
	if status.StatusCode == http.StatusNotFound || status.StatusCode == http.StatusGone {
		status.Body.Close()
		// Expired sessions can be recreated only with the SAME saved file ID.
		state.Session, state.Phase, state.Offset = "", "creating", 0
		if err := saveTransfer(ctx, store, d.destination, *state); err != nil {
			return "", err
		}
		return "", &uploadError{code: "upload_session_expired", retry: true}
	}
	if status.StatusCode != 308 {
		status.Body.Close()
		return "", statusError(status.StatusCode)
	}
	offset, err := driveAcknowledgedOffset(status, a.size)
	status.Body.Close()
	if err != nil {
		return "", err
	}
	if offset < state.Offset {
		return "", &uploadError{code: "invalid_upload_progress"}
	}
	state.Offset = offset
	if err := saveTransfer(ctx, store, d.destination, *state); err != nil {
		return "", err
	}
	maxRequests := (a.size+transferPartBytes-1)/transferPartBytes + 8
	for requests := int64(0); offset < a.size && requests < maxRequests; requests++ {
		size := min(transferPartBytes, a.size-offset)
		if offset+size == a.size {
			state.Phase = "completing"
			if err := saveTransfer(ctx, store, d.destination, *state); err != nil {
				return "", err
			}
		}
		r, err := d.transferPut(ctx, state.Session, fmt.Sprintf("bytes %d-%d/%d", offset, offset+size-1, a.size), io.NewSectionReader(a.file, offset, size), size)
		if err != nil {
			return "", err
		}
		if r.StatusCode == http.StatusOK || r.StatusCode == http.StatusCreated {
			return d.finishResumable(ctx, r, state, a, store)
		}
		if r.StatusCode != 308 {
			r.Body.Close()
			return "", statusError(r.StatusCode)
		}
		next, err := driveAcknowledgedOffset(r, a.size)
		r.Body.Close()
		if err != nil {
			return "", err
		}
		if next <= offset || next > offset+size {
			return "", &uploadError{code: "invalid_upload_progress"}
		}
		offset, state.Offset = next, next
		state.Phase = "uploading"
		if err := saveTransfer(ctx, store, d.destination, *state); err != nil {
			return "", err
		}
	}
	return "", &uploadError{code: "upload_completion_uncertain", retry: true}
}

func (d *driveUploader) transferPut(ctx context.Context, endpoint, contentRange string, body io.Reader, size int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, body)
	if err != nil {
		return nil, &uploadError{code: "invalid_request"}
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Range", contentRange)
	req.ContentLength = size
	r, err := d.client.Do(req)
	if err != nil {
		return nil, classify(err)
	}
	return r, nil
}

func (d *driveUploader) validTransferResult(result driveTransferResult, state *Checkpoint, a *artifact) bool {
	md, err := base64.StdEncoding.DecodeString(a.md5)
	return err == nil && result.ID == state.FileID && result.Size == strconv.FormatInt(a.size, 10) && strings.EqualFold(result.MD5, hex.EncodeToString(md)) && result.AppProperties["lanqinSHA256"] == a.sha256 && result.AppProperties["lanqinDestination"] == d.destination && result.AppProperties["lanqinTransfer"] == state.Name
}

func (d *driveUploader) finishResumable(ctx context.Context, r *http.Response, state *Checkpoint, a *artifact, store CheckpointStore) (string, error) {
	var result driveTransferResult
	if err := driveJSON(r, &result); err != nil {
		return "", err
	}
	if !d.validTransferResult(result, state, a) {
		return "", &uploadError{code: "invalid_provider_response"}
	}
	state.Phase = "completing"
	if err := saveTransfer(ctx, store, d.destination, *state); err != nil {
		return "", err
	}
	found, err := d.verifyTransferObject(ctx, state, a)
	if err != nil {
		return "", err
	}
	if !found {
		return "", &uploadError{code: "upload_completion_uncertain", retry: true}
	}
	return completeTransfer(ctx, store, d.destination, state, state.FileID)
}

func (d *driveUploader) verifyTransferObject(ctx context.Context, state *Checkpoint, a *artifact) (bool, error) {
	if !ValidDriveID(state.FileID) {
		return false, &uploadError{code: "checkpoint_mismatch"}
	}
	endpoint := driveFiles + "/" + url.PathEscape(state.FileID)
	r, err := driveRequest(ctx, d.client, http.MethodGet, endpoint+"?fields=id,size,md5Checksum,parents,appProperties", d.token, nil)
	if err != nil {
		return false, err
	}
	if r.StatusCode == http.StatusNotFound {
		r.Body.Close()
		return false, nil
	}
	if r.StatusCode != http.StatusOK {
		r.Body.Close()
		return false, statusError(r.StatusCode)
	}
	var result driveTransferResult
	if err := driveJSON(r, &result); err != nil {
		return false, err
	}
	parent := false
	for _, id := range result.Parents {
		parent = parent || id == d.folder
	}
	owned := parent && result.ID == state.FileID && result.AppProperties["lanqinSHA256"] == a.sha256 && result.AppProperties["lanqinDestination"] == d.destination && result.AppProperties["lanqinTransfer"] == state.Name
	if !owned {
		return false, &uploadError{code: "remote_verification_failed"}
	}
	if !d.validTransferResult(result, state, a) {
		// Some providers expose the new file's owned metadata before the final
		// chunk is committed. Its placeholder is not a completed backup; resume
		// through the saved session, never overwrite via a separate file PATCH.
		size, err := strconv.ParseInt(result.Size, 10, 64)
		if state.Phase != "completed" && validDriveSession(state.Session) && err == nil && size >= 0 && size < a.size && result.MD5 == "" {
			return false, nil
		}
		return false, &uploadError{code: "remote_verification_failed"}
	}
	r, err = driveRequest(ctx, d.client, http.MethodGet, endpoint+"?alt=media", d.token, nil)
	if err != nil {
		return false, err
	}
	if r.StatusCode != http.StatusOK {
		r.Body.Close()
		return false, statusError(r.StatusCode)
	}
	if err := verifyTransferredBody(ctx, r.Body, a); err != nil {
		return false, err
	}
	return true, nil
}
