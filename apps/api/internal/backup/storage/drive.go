package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const driveFiles = "https://www.googleapis.com/drive/v3/files"

type driveUploader struct {
	client                               *http.Client
	folder, token, destination, resultID string
	cleanup                              func()
}

func (d *driveUploader) close()                  { d.cleanup() }
func (d *driveUploader) objectKey(string) string { return d.resultID }

func driveJSON(response *http.Response, dst any) error {
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(data) > 64<<10 || json.Unmarshal(data, dst) != nil {
		return &uploadError{code: "invalid_provider_response"}
	}
	return nil
}

func driveRequest(ctx context.Context, client *http.Client, method, endpoint, token string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, &uploadError{code: "invalid_request"}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	response, err := client.Do(req)
	if err != nil {
		return nil, classify(err)
	}
	return response, nil
}

// CreateDriveBackupFolder always creates a new app-owned folder. Reauthorization
// cannot silently re-use another Google account's old target. Old files remain.
func CreateDriveBackupFolder(ctx context.Context, client *http.Client, token, name, destination string) (string, error) {
	if client == nil || token == "" || !identifierPattern.MatchString(destination) || len(name) > 128 {
		return "", errors.New("invalid Drive folder request")
	}
	data, _ := json.Marshal(map[string]any{"name": "LanQin Backups - " + name, "mimeType": "application/vnd.google-apps.folder", "appProperties": map[string]string{"lanqinDestination": destination}})
	r, err := driveRequest(ctx, client, "POST", driveFiles+"?fields=id", token, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	if r.StatusCode != http.StatusOK && r.StatusCode != http.StatusCreated {
		r.Body.Close()
		return "", statusError(r.StatusCode)
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := driveJSON(r, &result); err != nil || !ValidDriveID(result.ID) {
		return "", errors.New("invalid Drive folder response")
	}
	return result.ID, nil
}

func (d *driveUploader) put(ctx context.Context, name string, body *io.SectionReader, a *artifact) error {
	metadata, _ := json.Marshal(map[string]any{"name": name, "parents": []string{d.folder}, "appProperties": map[string]string{"lanqinDestination": d.destination, "lanqinSHA256": a.sha256}})
	req, err := http.NewRequestWithContext(ctx, "POST", "https://www.googleapis.com/upload/drive/v3/files?uploadType=resumable&fields=id,size,md5Checksum,appProperties", bytes.NewReader(metadata))
	if err != nil {
		return &uploadError{code: "invalid_request"}
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("X-Upload-Content-Type", "application/octet-stream")
	req.Header.Set("X-Upload-Content-Length", strconv.FormatInt(a.size, 10))
	r, err := d.client.Do(req)
	if err != nil {
		return classify(err)
	}
	location := r.Header.Get("Location")
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return statusError(r.StatusCode)
	}
	u, err := url.Parse(location)
	if err != nil || u.Scheme != "https" || u.Host != "www.googleapis.com" || u.User != nil || u.Fragment != "" || u.Path != "/upload/drive/v3/files" || u.Query().Get("upload_id") == "" {
		return &uploadError{code: "invalid_upload_session"}
	}
	// Location contains a sensitive session token. Never return/log it. A failed
	// attempt starts a fresh object/session; no overwrite, no remote delete.
	put, err := http.NewRequestWithContext(ctx, "PUT", u.String(), body)
	if err != nil {
		return &uploadError{code: "invalid_request"}
	}
	put.Header.Set("Authorization", "Bearer "+d.token)
	put.Header.Set("Content-Type", "application/octet-stream")
	put.Header.Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", a.size-1, a.size))
	put.ContentLength = a.size
	response, err := d.client.Do(put)
	if err != nil {
		return classify(err)
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		response.Body.Close()
		return statusError(response.StatusCode)
	}
	var result struct {
		ID            string            `json:"id"`
		Size          string            `json:"size"`
		MD5           string            `json:"md5Checksum"`
		AppProperties map[string]string `json:"appProperties"`
	}
	if err := driveJSON(response, &result); err != nil {
		return err
	}
	md5Bytes, err := base64.StdEncoding.DecodeString(a.md5)
	if err != nil || !strings.EqualFold(result.MD5, hex.EncodeToString(md5Bytes)) || !ValidDriveID(result.ID) || result.Size != strconv.FormatInt(a.size, 10) || result.AppProperties["lanqinSHA256"] != a.sha256 || result.AppProperties["lanqinDestination"] != d.destination {
		return &uploadError{code: "invalid_provider_response"}
	}
	d.resultID = result.ID
	return nil
}
