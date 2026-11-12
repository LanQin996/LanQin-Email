package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// AbortCheckpoint closes a known S3 multipart upload, NEVER a completed object.
// It requires the authenticated checkpoint and separately pinned source receipt.
// A lost creation response without an upload ID cannot be guessed or enumerated.
func AbortCheckpoint(ctx context.Context, d Destination, state Checkpoint, expected string, size int64, c Credentials) error {
	want, err := hex.DecodeString(expected)
	if err != nil || len(want) != sha256.Size || size < 1 || size > MaxUploadBytes || (Config{Version: 1, Destinations: []Destination{d}}).Validate() != nil || d.Type != "s3" || state.Version != 1 || state.Type != "s3" || !strings.EqualFold(state.SHA256, expected) || state.Bytes != size || state.Binding != transferBinding(d, c) || !backupObjectName.MatchString(state.Name) {
		return &uploadError{code: "checkpoint_mismatch"}
	}
	switch state.Phase {
	case "prepared", "creating", "uploading", "completing", "completed", "aborted":
	default:
		return &uploadError{code: "checkpoint_mismatch"}
	}
	if state.Phase == "completed" || state.Phase == "aborted" || state.Phase == "prepared" {
		return nil
	}
	if !validTransferToken(state.UploadID, 2048) {
		return &uploadError{code: "upload_session_uncertain"}
	}
	uploader, err := credentialsUploader(d, c)
	if err != nil {
		return &uploadError{code: "destination_unavailable"}
	}
	defer uploader.close()
	s := uploader.(*s3Uploader)
	return s.abort(ctx, state)
}

func (s *s3Uploader) abort(ctx context.Context, state Checkpoint) error {
	if transport, ok := s.client.Transport.(*restrictedTransport); ok {
		transport.methods = map[string]bool{http.MethodDelete: true}
	}
	opts := s.api.Options()
	opts.HTTPClient = multipartS3HTTPClient{s.client}
	api := s3.New(opts)
	_, err := api.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(s.bucket), Key: aws.String(objectKey(s.destination, state.Name)), UploadId: aws.String(state.UploadID)})
	if err != nil {
		var classified *uploadError
		err = classify(err)
		if errors.As(err, &classified) && classified.code == "object_missing" {
			return nil
		}
		return &uploadError{code: "multipart_cleanup_failed"}
	}
	return nil
}
