package storage

import (
	"context"
	"crypto/md5" // Per-part S3 transport check, always followed by full SHA-256 readback.
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Multipart APIs share the same pinned network policy, adding only the methods
// needed for this selected endpoint. SDK retry is disabled: our durable state
// decides what is safe to repeat. Successful object GET bodies are not truncated.
type multipartS3HTTPClient struct{ client *http.Client }

func (c multipartS3HTTPClient) Do(req *http.Request) (*http.Response, error) {
	r, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	allowed := r.StatusCode == http.StatusOK || req.Method == http.MethodDelete && r.StatusCode == http.StatusNoContent
	if r.StatusCode >= 200 && r.StatusCode < 300 && !allowed {
		r.Body.Close()
		return nil, &uploadError{code: "unexpected_success_status"}
	}
	if req.Method != http.MethodGet || req.URL.Query().Get("uploadId") != "" || r.StatusCode != http.StatusOK {
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.LimitReader(r.Body, 128<<10), r.Body}
	}
	return r, nil
}

func (s *s3Uploader) resume(ctx context.Context, a *artifact, store CheckpointStore) (string, error) {
	state, err := loadTransfer(ctx, store, s.destination, s.binding, a)
	if err != nil {
		return "", err
	}
	if transport, ok := s.client.Transport.(*restrictedTransport); ok {
		transport.methods = map[string]bool{http.MethodGet: true, http.MethodPost: true, http.MethodPut: true, http.MethodDelete: true}
	}
	opts := s.api.Options()
	opts.HTTPClient = multipartS3HTTPClient{s.client}
	api := s3.New(opts)
	key := objectKey(s.destination, state.Name)
	if state.Phase == "completed" || state.Phase == "completing" || state.Phase == "creating" {
		found, err := s.verifyMultipartObject(ctx, api, key, a)
		if err != nil {
			return "", err
		}
		if found {
			return completeTransfer(ctx, store, s.destination.ID, state, key)
		}
		if state.Phase == "completed" {
			return "", &uploadError{code: "remote_verification_failed"}
		}
	}
	if state.Phase == "creating" || state.UploadID == "" && state.Phase != "prepared" {
		return "", &uploadError{code: "upload_session_uncertain"}
	}
	if state.Phase == "prepared" {
		state.Phase = "creating"
		if err := saveTransfer(ctx, store, s.destination.ID, *state); err != nil {
			return "", err
		}
		created, err := api.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
			Bucket: aws.String(s.bucket), Key: aws.String(key), ContentType: aws.String("application/octet-stream"),
			Metadata: map[string]string{"lanqin-sha256": a.sha256, "lanqin-destination": s.destination.ID, "lanqin-transfer": state.Name},
		})
		if err != nil {
			return "", classify(err)
		}
		if created.UploadId == nil || !validTransferToken(*created.UploadId, 2048) {
			return "", &uploadError{code: "invalid_provider_response"}
		}
		state.UploadID, state.Phase = *created.UploadId, "uploading"
		if err := saveTransfer(ctx, store, s.destination.ID, *state); err != nil {
			return "", err
		}
	}
	maxParts := int32((a.size + transferPartBytes - 1) / transferPartBytes)
	remote, err := listTransferParts(ctx, api, s.bucket, key, state.UploadID, maxParts)
	if err != nil {
		if state.Phase == "completing" {
			return "", &uploadError{code: "upload_completion_uncertain", retry: true}
		}
		return "", err
	}
	known := map[int32]CheckpointPart{}
	for _, p := range state.Parts {
		if p.Number < 1 || p.Number > maxParts || known[p.Number].Number != 0 || !validTransferToken(p.ETag, 1024) {
			return "", &uploadError{code: "checkpoint_mismatch"}
		}
		known[p.Number] = p
	}
	completed := []types.CompletedPart{}
	for number := int32(1); number <= maxParts; number++ {
		start := int64(number-1) * transferPartBytes
		size := min(transferPartBytes, a.size-start)
		hash := md5.New()
		if _, err := io.Copy(hash, contextReader{ctx, io.NewSectionReader(a.file, start, size)}); err != nil {
			return "", classify(err)
		}
		checksum := base64.StdEncoding.EncodeToString(hash.Sum(nil))
		part, ok := known[number]
		r, exists := remote[number]
		if !ok || !exists || part.Bytes != size || part.MD5 != checksum || aws.ToString(r.ETag) != part.ETag || aws.ToInt64(r.Size) != size {
			// A lost acknowledgement may leave an unrecorded part. Re-PUT that
			// same part number with the pinned source, rather than guessing bytes.
			uploaded, err := api.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: aws.String(state.UploadID), PartNumber: aws.Int32(number), Body: io.NewSectionReader(a.file, start, size), ContentLength: aws.Int64(size), ContentMD5: aws.String(checksum)})
			if err != nil {
				return "", classify(err)
			}
			if uploaded.ETag == nil || !validTransferToken(*uploaded.ETag, 1024) {
				return "", &uploadError{code: "invalid_provider_response"}
			}
			part = CheckpointPart{Number: number, Bytes: size, MD5: checksum, ETag: *uploaded.ETag}
		}
		known[number] = part
		state.Parts = nil
		for n := int32(1); n <= maxParts; n++ {
			if saved, ok := known[n]; ok {
				state.Parts = append(state.Parts, saved)
			}
		}
		state.Phase = "uploading"
		state.Offset = start + size
		if err := saveTransfer(ctx, store, s.destination.ID, *state); err != nil {
			return "", err
		}
		completed = append(completed, types.CompletedPart{PartNumber: aws.Int32(number), ETag: aws.String(part.ETag)})
	}
	state.Phase = "completing"
	if err := saveTransfer(ctx, store, s.destination.ID, *state); err != nil {
		return "", err
	}
	_, err = api.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: aws.String(state.UploadID), IfNoneMatch: aws.String("*"), MultipartUpload: &types.CompletedMultipartUpload{Parts: completed}})
	if err != nil {
		return "", classify(err)
	}
	found, err := s.verifyMultipartObject(ctx, api, key, a)
	if err != nil {
		return "", err
	}
	if !found {
		return "", &uploadError{code: "upload_completion_uncertain", retry: true}
	}
	return completeTransfer(ctx, store, s.destination.ID, state, key)
}

func validTransferToken(value string, limit int) bool {
	return value != "" && len(value) <= limit && strings.IndexFunc(value, unicode.IsControl) < 0
}

func listTransferParts(ctx context.Context, api *s3.Client, bucket, key, id string, maxParts int32) (map[int32]types.Part, error) {
	parts := map[int32]types.Part{}
	marker := ""
	for page := 0; page < 12; page++ {
		input := &s3.ListPartsInput{Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(id), MaxParts: aws.Int32(100)}
		if marker != "" {
			input.PartNumberMarker = aws.String(marker)
		}
		response, err := api.ListParts(ctx, input)
		if err != nil {
			return nil, classify(err)
		}
		for _, part := range response.Parts {
			n := aws.ToInt32(part.PartNumber)
			if n < 1 || n > maxParts || aws.ToInt64(part.Size) < 1 || parts[n].PartNumber != nil || !validTransferToken(aws.ToString(part.ETag), 1024) {
				return nil, &uploadError{code: "invalid_provider_response"}
			}
			parts[n] = part
		}
		if !aws.ToBool(response.IsTruncated) {
			return parts, nil
		}
		next := aws.ToString(response.NextPartNumberMarker)
		n, err := strconv.ParseInt(next, 10, 32)
		previous, _ := strconv.ParseInt(marker, 10, 32)
		if err != nil || n <= previous || n > int64(maxParts) {
			return nil, &uploadError{code: "invalid_provider_response"}
		}
		marker = next
	}
	return nil, &uploadError{code: "invalid_provider_response"}
}

func (s *s3Uploader) verifyMultipartObject(ctx context.Context, api *s3.Client, key string, a *artifact) (bool, error) {
	result, err := api.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		var classified *uploadError
		err = classify(err)
		if errors.As(err, &classified) && classified.code == "object_missing" {
			return false, nil
		}
		return false, err
	}
	name := key
	if s.prefix != "" {
		name = strings.TrimPrefix(key, s.prefix+"/")
	}
	if result.Metadata["lanqin-sha256"] != a.sha256 || result.Metadata["lanqin-destination"] != s.destination.ID || result.Metadata["lanqin-transfer"] != name || result.ContentLength != nil && *result.ContentLength >= 0 && *result.ContentLength != a.size {
		result.Body.Close()
		return false, &uploadError{code: "remote_verification_failed"}
	}
	if err := verifyTransferredBody(ctx, result.Body, a); err != nil {
		return false, err
	}
	return true, nil
}
