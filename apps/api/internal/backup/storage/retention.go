package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/logging"
)

var ErrRemoteObjectMissing = errors.New("receipted remote object is unavailable")

// VerifyRemote reads back the complete ciphertext against a separately trusted
// receipt. This is transport verification, not an age decryption exercise.
func VerifyRemote(ctx context.Context, d Destination, object, expected string, size int64, c Credentials) error {
	return retainRemote(ctx, d, object, expected, size, c, false, nil)
}

// DeleteVerified removes exactly one receipted object, after full readback. A
// strong ETag and conditional DELETE prevent deleting a concurrent replacement.
// No remote listing, recursive deletion, unconditional fallback or retries.
func DeleteVerified(ctx context.Context, d Destination, object, expected string, size int64, c Credentials) error {
	return retainRemote(ctx, d, object, expected, size, c, true, nil)
}

// DeleteVerifiedGuarded also rechecks the caller's live authorization after the
// potentially long readback, immediately before constructing the DELETE request.
func DeleteVerifiedGuarded(ctx context.Context, d Destination, object, expected string, size int64, c Credentials, beforeDelete func(context.Context) error) error {
	if beforeDelete == nil {
		return errors.New("retention authorization guard required")
	}
	return retainRemote(ctx, d, object, expected, size, c, true, nil, beforeDelete)
}

func retainRemote(ctx context.Context, d Destination, object, expected string, size int64, c Credentials, remove bool, client *http.Client, guards ...func(context.Context) error) error {
	if err := (Config{Version: 1, Destinations: []Destination{d}}).Validate(); err != nil || !validDownloadObject(d, object) || !validDownloadCredentials(d, c) || size < 1 || size > MaxUploadBytes {
		return errors.New("invalid retention object or credentials")
	}
	want, err := hex.DecodeString(expected)
	if err != nil || len(want) != sha256.Size {
		return errors.New("invalid retention receipt")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cleanup := func() {}
	if client == nil {
		client, cleanup = downloadClient(d)
		if remove {
			client.Transport.(*restrictedTransport).methods[http.MethodDelete] = true
		}
	}
	defer cleanup()
	body, err := openDownload(ctx, d, object, strings.ToLower(expected), c, client)
	if err != nil {
		var remote *uploadError
		if errors.As(err, &remote) && remote.code == "object_missing" {
			return ErrRemoteObjectMissing
		}
		return err
	}
	defer body.Close()
	hash := sha256.New()
	const ageHeader = "age-encryption.org/v1\n"
	header := make([]byte, len(ageHeader))
	if _, err := io.ReadFull(body, header); err != nil || string(header) != ageHeader {
		return errors.New("retention readback failed")
	}
	hash.Write(header)
	n, err := io.Copy(hash, io.LimitReader(contextReader{ctx, body}, size+1))
	if err != nil || ctx.Err() != nil || n+int64(len(header)) != size || hex.EncodeToString(hash.Sum(nil)) != strings.ToLower(expected) {
		return errors.New("retention readback failed")
	}
	if !remove {
		return nil
	}
	tag := body.(*remoteDownload).etag
	// RFC 9110 strong entity-tag; reject weak, missing, malformed or injected tags.
	if len(tag) < 3 || len(tag) > 1024 || tag[0] != '"' || tag[len(tag)-1] != '"' {
		return errors.New("retention requires a strong entity tag")
	}
	for _, b := range []byte(tag[1 : len(tag)-1]) {
		if b < 0x21 || b == '"' || b == 0x7f {
			return errors.New("retention requires a strong entity tag")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, guard := range guards {
		if err := guard(ctx); err != nil {
			return err
		}
	}
	if d.Type == "s3" {
		api := s3.New(s3.Options{BaseEndpoint: aws.String(d.S3.Endpoint), Region: d.S3.Region, UsePathStyle: d.S3.PathStyle,
			HTTPClient: retentionS3Client{client}, RetryMaxAttempts: 1, Logger: logging.Nop{},
			Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				return aws.Credentials{AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken, Source: "LanQinBackupRetention"}, nil
			})})
		_, err := api.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(d.S3.Bucket), Key: aws.String(object), IfMatch: aws.String(tag)})
		return classify(err)
	}
	endpoint := driveFiles + "/" + url.PathEscape(object)
	if d.Type == "webdav" {
		u, _ := endpointURLWithPolicy(d.WebDAV.URL, d.NetworkPolicy)
		u.Path = strings.TrimSuffix(u.Path, "/") + "/" + object
		endpoint = u.String()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return errors.New("invalid retention request")
	}
	req.Header.Set("If-Match", tag)
	if d.Type == "webdav" {
		req.SetBasicAuth(c.Username, c.Password)
	} else {
		req.Header.Set("Authorization", "Bearer "+c.DriveAccessToken)
	}
	r, err := client.Do(req)
	if err != nil {
		return classify(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent && (d.Type != "webdav" || r.StatusCode != http.StatusOK) {
		return statusError(r.StatusCode)
	}
	return nil
}

type retentionS3Client struct{ client *http.Client }

func (c retentionS3Client) Do(req *http.Request) (*http.Response, error) {
	r, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if r.StatusCode >= 200 && r.StatusCode < 300 && r.StatusCode != http.StatusNoContent {
		r.Body.Close()
		return nil, errors.New("retention deletion not confirmed")
	}
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.LimitReader(r.Body, 64<<10), r.Body}
	return r, nil
}
