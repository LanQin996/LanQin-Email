package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
)

type driveRecoveryFile struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Size       string            `json:"size"`
	MIME       string            `json:"mimeType"`
	Trashed    bool              `json:"trashed"`
	Parents    []string          `json:"parents"`
	Properties map[string]string `json:"appProperties"`
}

func validDriveRecoveryFile(d Destination, item driveRecoveryFile) bool {
	name := strings.TrimSuffix(strings.TrimPrefix(item.Name, "lanqin-recovery-"), recoveryIndexSuffix)
	size, err := strconv.ParseInt(item.Size, 10, 64)
	parent := false
	for _, id := range item.Parents {
		if id == d.Drive.FolderID {
			parent = true
		}
	}
	return ValidDriveID(item.ID) && strings.HasPrefix(item.Name, "lanqin-recovery-") && strings.HasSuffix(item.Name, recoveryIndexSuffix) && ValidDriveID(name) && err == nil && size > 0 && size <= MaxRecoveryIndexBytes && item.MIME == "application/json" && !item.Trashed && parent && identifierPattern.MatchString(item.Properties["lanqinDestination"]) && item.Properties["lanqinRecoveryIndex"] == "1"
}

func getDriveRecoveryIndex(ctx context.Context, d Destination, id string, c Credentials, client *http.Client) ([]byte, driveRecoveryFile, error) {
	var item driveRecoveryFile
	if !ValidDriveID(id) {
		return nil, item, errRecoveryRemote
	}
	fields := "id,name,size,mimeType,trashed,parents,appProperties"
	response, err := driveRequest(ctx, client, http.MethodGet, driveFiles+"/"+url.PathEscape(id)+"?fields="+url.QueryEscape(fields), c.DriveAccessToken, nil)
	if err != nil {
		return nil, item, errRecoveryRemote
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, item, errRecoveryRemote
	}
	metadata, err := recoveryBody(response, MaxRecoveryIndexBytes)
	if err != nil || recoveryJSON(metadata, &item) != nil || item.ID != id || !validDriveRecoveryFile(d, item) {
		return nil, item, errRecoveryRemote
	}
	response, err = driveRequest(ctx, client, http.MethodGet, driveFiles+"/"+url.PathEscape(id)+"?alt=media", c.DriveAccessToken, nil)
	if err != nil {
		return nil, item, errRecoveryRemote
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, item, errRecoveryRemote
	}
	document, err := recoveryBody(response, MaxRecoveryIndexBytes)
	if err != nil || strconv.Itoa(len(document)) != item.Size {
		return nil, item, errRecoveryRemote
	}
	return document, item, nil
}

func publishDriveRecoveryIndex(ctx context.Context, d Destination, object string, document, key []byte, c Credentials, client *http.Client, guard func() error) ([]byte, error) {
	name := "lanqin-recovery-" + object + recoveryIndexSuffix
	existing, err := findDriveRecoveryIndex(ctx, d, object, name, document, key, c, client)
	if err != nil || existing != nil {
		return existing, err
	}
	if err := guard(); err != nil {
		return nil, err
	}
	response, err := driveRequest(ctx, client, http.MethodGet, driveFiles+"/generateIds?count=1&space=drive&type=files", c.DriveAccessToken, nil)
	if err != nil {
		return nil, errRecoveryRemote
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, errRecoveryRemote
	}
	idsBody, err := recoveryBody(response, MaxRecoveryIndexBytes)
	var ids struct {
		IDs []string `json:"ids"`
	}
	if err != nil || recoveryJSON(idsBody, &ids) != nil || len(ids.IDs) != 1 || !ValidDriveID(ids.IDs[0]) || ids.IDs[0] == object {
		return nil, errRecoveryRemote
	}
	metadata, _ := json.Marshal(map[string]any{"id": ids.IDs[0], "name": name, "mimeType": "application/json", "parents": []string{d.Drive.FolderID}, "appProperties": map[string]string{"lanqinRecoveryIndex": "1", "lanqinDestination": d.ID}})
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, part := range []struct {
		contentType string
		data        []byte
	}{{"application/json; charset=UTF-8", metadata}, {"application/json", document}} {
		header := textproto.MIMEHeader{}
		header.Set("Content-Type", part.contentType)
		output, err := writer.CreatePart(header)
		if err != nil {
			return nil, errRecoveryRemote
		}
		if _, err := output.Write(part.data); err != nil {
			return nil, errRecoveryRemote
		}
	}
	if writer.Close() != nil {
		return nil, errRecoveryRemote
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, driveUploadEndpoint+"?uploadType=multipart&fields=id", bytes.NewReader(body.Bytes()))
	if err != nil {
		return nil, errRecoveryRemote
	}
	request.Header.Set("Content-Type", "multipart/related; boundary="+writer.Boundary())
	request.Header.Set("Authorization", "Bearer "+c.DriveAccessToken)
	request.Header.Set("If-None-Match", "*")
	if err := guard(); err != nil {
		return nil, err
	}
	response, err = client.Do(request)
	if err != nil {
		return nil, errRecoveryRemote
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		response.Body.Close()
		return nil, errRecoveryRemote
	}
	resultBody, err := recoveryBody(response, MaxRecoveryIndexBytes)
	var result struct {
		ID string `json:"id"`
	}
	if err != nil || recoveryJSON(resultBody, &result) != nil || result.ID != ids.IDs[0] {
		return nil, errRecoveryRemote
	}
	actual, item, err := getDriveRecoveryIndex(ctx, d, result.ID, c, client)
	if err != nil || item.Name != name || item.Properties["lanqinDestination"] != d.ID {
		return nil, errRecoveryRemote
	}
	return actual, nil
}

func findDriveRecoveryIndex(ctx context.Context, d Destination, object, name string, document, key []byte, c Credentials, client *http.Client) ([]byte, error) {
	query := "'" + d.Drive.FolderID + "' in parents and trashed = false and name = '" + name + "' and appProperties has { key='lanqinRecoveryIndex' and value='1' }"
	values := url.Values{"q": {query}, "fields": {"nextPageToken,incompleteSearch,files(id,name,size,mimeType,trashed,parents,appProperties)"}, "pageSize": {strconv.Itoa(maxRecoveryIndexes)}, "spaces": {"drive"}}
	response, err := driveRequest(ctx, client, http.MethodGet, driveFiles+"?"+values.Encode(), c.DriveAccessToken, nil)
	if err != nil {
		return nil, errRecoveryRemote
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, errRecoveryRemote
	}
	data, err := recoveryBody(response, maxRecoveryPageBody)
	var result struct {
		Next       string              `json:"nextPageToken"`
		Incomplete bool                `json:"incompleteSearch"`
		Files      []driveRecoveryFile `json:"files"`
	}
	if err != nil || recoveryJSON(data, &result) != nil || result.Next != "" || result.Incomplete || len(result.Files) > maxRecoveryIndexes {
		return nil, errRecoveryRemote
	}
	want, err := OpenRecoveryIndex(d, document, key)
	if err != nil {
		return nil, errRecoveryRemote
	}
	var existing []byte
	seen := map[string]bool{}
	for _, hint := range result.Files {
		if !validDriveRecoveryFile(d, hint) || hint.Name != name || seen[hint.ID] {
			return nil, errRecoveryRemote
		}
		seen[hint.ID] = true
		actual, item, err := getDriveRecoveryIndex(ctx, d, hint.ID, c, client)
		if err != nil || item.Name != name || item.Properties["lanqinDestination"] != d.ID {
			return nil, errRecoveryRemote
		}
		entry, err := OpenRecoveryIndex(d, actual, key)
		if err != nil || entry.ObjectKey != object || entry.DestinationID != want.DestinationID || entry.Location != want.Location || entry.SHA256 != want.SHA256 || entry.Bytes != want.Bytes {
			return nil, errRecoveryRemote
		}
		existing = actual
	}
	return existing, nil
}

func discoverDriveRecoveryIndexes(ctx context.Context, d Destination, key []byte, c Credentials, client *http.Client) ([][]byte, error) {
	query := "'" + d.Drive.FolderID + "' in parents and trashed = false and appProperties has { key='lanqinRecoveryIndex' and value='1' }"
	next := ""
	seenTokens, seenFiles := map[string]bool{}, map[string]bool{}
	items := []driveRecoveryFile{}
	for page := 0; page < maxRecoveryPages; page++ {
		values := url.Values{"q": {query}, "fields": {"nextPageToken,incompleteSearch,files(id,name,size,mimeType,trashed,parents,appProperties)"}, "pageSize": {strconv.Itoa(maxRecoveryIndexes)}, "spaces": {"drive"}}
		if next != "" {
			values.Set("pageToken", next)
		}
		response, err := driveRequest(ctx, client, http.MethodGet, driveFiles+"?"+values.Encode(), c.DriveAccessToken, nil)
		if err != nil {
			return nil, errRecoveryRemote
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return nil, errRecoveryRemote
		}
		data, err := recoveryBody(response, maxRecoveryPageBody)
		var result struct {
			Next       string              `json:"nextPageToken"`
			Incomplete bool                `json:"incompleteSearch"`
			Files      []driveRecoveryFile `json:"files"`
		}
		if err != nil || recoveryJSON(data, &result) != nil || result.Incomplete || len(result.Files) > maxRecoveryIndexes {
			return nil, errRecoveryRemote
		}
		for _, item := range result.Files {
			if !validDriveRecoveryFile(d, item) || seenFiles[item.ID] {
				return nil, errRecoveryRemote
			}
			seenFiles[item.ID] = true
			items = append(items, item)
			if len(items) > maxRecoveryIndexes {
				return nil, errRecoveryRemote
			}
		}
		if result.Next == "" {
			break
		}
		if page == maxRecoveryPages-1 || !validRecoveryPageToken(result.Next) || seenTokens[result.Next] {
			return nil, errRecoveryRemote
		}
		seenTokens[result.Next] = true
		next = result.Next
	}
	documents := make([][]byte, 0, len(items))
	for _, hint := range items {
		document, item, err := getDriveRecoveryIndex(ctx, d, hint.ID, c, client)
		if err != nil || item.Properties["lanqinDestination"] != hint.Properties["lanqinDestination"] {
			return nil, errRecoveryRemote
		}
		entry, err := OpenRecoveryIndex(d, document, key)
		if err != nil || entry.DestinationID != item.Properties["lanqinDestination"] || item.Name != "lanqin-recovery-"+entry.ObjectKey+recoveryIndexSuffix {
			return nil, errRecoveryRemote
		}
		documents = append(documents, document)
	}
	return documents, nil
}
