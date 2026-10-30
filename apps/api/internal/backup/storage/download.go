package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/logging"
)

type DownloadReceipt struct {
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

var backupObjectName = regexp.MustCompile(`^lanqin-[a-f0-9]{32}\.age$`)

// Download fetches one explicitly selected encrypted backup, validates its
// separately trusted ciphertext receipt and publishes a NEW local file. It does
// not decrypt, overwrite, list unrelated objects, use ambient AWS credentials or
// delete remote data. Only MaxBytes/AttemptTimeout from Options are used; GET
// is not automatically retried. The caller may retry into another absent path.
func Download(ctx context.Context, destination Destination, objectKey, expectedSHA256, output string, opts Options, credentials Credentials) (DownloadReceipt, error) {
	return download(ctx, destination, objectKey, expectedSHA256, output, opts, credentials, nil)
}

func download(ctx context.Context, d Destination, object, expected, output string, opts Options, credentials Credentials, client *http.Client) (DownloadReceipt, error) {
	var empty DownloadReceipt
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if err := (Config{Version: 1, Destinations: []Destination{d}}).Validate(); err != nil {
		return empty, errors.New("invalid download destination")
	}
	want, err := hex.DecodeString(expected)
	if err != nil || len(want) != sha256.Size || !validDownloadObject(d, object) {
		return empty, errors.New("download requires a valid backup object and trusted SHA-256 receipt")
	}
	if opts.MaxBytes < 1 || opts.MaxBytes > MaxUploadBytes || opts.AttemptTimeout < 0 || opts.AttemptTimeout > time.Hour {
		return empty, errors.New("invalid download limits")
	}
	if opts.AttemptTimeout == 0 {
		opts.AttemptTimeout = DefaultOptions().AttemptTimeout
	}
	if !validDownloadCredentials(d, credentials) {
		return empty, errors.New("download credentials unavailable or invalid")
	}
	if output == "" {
		return empty, errors.New("download output path is required")
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		return empty, errors.New("download output must be absent")
	}
	info, err := os.Stat(filepath.Dir(output))
	if err != nil || !info.IsDir() {
		return empty, errors.New("download output parent must exist")
	}
	ctx, cancel := context.WithTimeout(ctx, opts.AttemptTimeout)
	defer cancel()
	cleanup := func() {}
	if client == nil {
		client, cleanup = downloadClient(d)
	}
	defer cleanup()
	body, err := openDownload(ctx, d, object, strings.ToLower(expected), credentials, client)
	if err != nil {
		return empty, err
	}
	defer body.Close()
	file, err := os.CreateTemp(filepath.Dir(output), ".lanqin-download-*")
	if err != nil {
		return empty, errors.New("cannot create private ciphertext download")
	}
	defer func() {
		file.Close()
		os.Remove(file.Name()) // Only our own EXCL temporary file, after close on Windows.
	}()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(contextReader{ctx, body}, opts.MaxBytes+1))
	if err != nil || ctx.Err() != nil || n > opts.MaxBytes || !bytes.Equal(hash.Sum(nil), want) {
		return empty, errors.New("download failed, exceeded limit or did not match trusted ciphertext receipt")
	}
	const ageHeader = "age-encryption.org/v1\n"
	header := make([]byte, len(ageHeader))
	if _, err := file.ReadAt(header, 0); err != nil || string(header) != ageHeader {
		return empty, errors.New("download is not a supported encrypted artifact")
	}
	if err := file.Sync(); err != nil {
		return empty, errors.New("cannot flush ciphertext download")
	}
	if err := file.Close(); err != nil {
		return empty, errors.New("cannot close ciphertext download")
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if err := os.Link(file.Name(), output); err != nil {
		return empty, errors.New("cannot publish download without replacement")
	}
	return DownloadReceipt{SHA256: hex.EncodeToString(want), Bytes: n}, nil
}

func validDownloadObject(d Destination, object string) bool {
	if d.Type == "drive" {
		return ValidDriveID(object)
	}
	name := object
	if d.Type == "s3" && d.S3 != nil && d.S3.Prefix != "" {
		prefix := d.S3.Prefix + "/"
		if !strings.HasPrefix(object, prefix) {
			return false
		}
		name = strings.TrimPrefix(object, prefix)
	}
	return backupObjectName.MatchString(name)
}

func validDownloadCredentials(d Destination, c Credentials) bool {
	valid := func(value string) bool {
		return value != "" && len(value) <= 65536 && strings.IndexFunc(value, unicode.IsControl) < 0
	}
	switch d.Type {
	case "s3":
		return valid(c.AccessKeyID) && valid(c.SecretAccessKey) && (c.SessionToken == "" || valid(c.SessionToken))
	case "webdav":
		return valid(c.Username) && !strings.Contains(c.Username, ":") && valid(c.Password)
	case "drive":
		return valid(c.DriveAccessToken)
	}
	return false
}

// Construct a GET-only client without widening the PUT-only upload default.
func downloadClient(d Destination) (*http.Client, func()) {
	hosts := []string{"www.googleapis.com"}
	if d.S3 != nil {
		u, _ := endpointURL(d.S3.Endpoint)
		virtual := d.S3.Bucket + "." + u.Hostname()
		if port := u.Port(); port != "" {
			virtual = net.JoinHostPort(virtual, port)
		}
		hosts = []string{u.Host, virtual}
	} else if d.WebDAV != nil {
		u, _ := endpointURL(d.WebDAV.URL)
		hosts = []string{u.Host}
	}
	client, closeClient := secureClient(hosts...)
	client.Transport.(*restrictedTransport).methods = map[string]bool{http.MethodGet: true}
	return client, closeClient
}

type downloadS3Client struct{ client *http.Client }

func (c downloadS3Client) Do(req *http.Request) (*http.Response, error) {
	r, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if r.StatusCode >= 200 && r.StatusCode < 300 && r.StatusCode != http.StatusOK {
		r.Body.Close()
		return nil, &uploadError{code: "unexpected_success_status"}
	}
	if r.StatusCode != http.StatusOK {
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.LimitReader(r.Body, 64<<10), r.Body}
	}
	return r, nil
}

func openDownload(ctx context.Context, d Destination, object, expected string, c Credentials, client *http.Client) (io.ReadCloser, error) {
	if d.Type == "s3" {
		api := s3.New(s3.Options{BaseEndpoint: aws.String(d.S3.Endpoint), Region: d.S3.Region, UsePathStyle: d.S3.PathStyle,
			HTTPClient: downloadS3Client{client}, RetryMaxAttempts: 1, Logger: logging.Nop{},
			Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				return aws.Credentials{AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken, Source: "LanQinBackupDownload"}, nil
			})})
		result, err := api.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(d.S3.Bucket), Key: aws.String(object)})
		if err != nil {
			return nil, classify(err)
		}
		return result.Body, nil
	}
	endpoint := ""
	if d.Type == "webdav" {
		u, _ := endpointURL(d.WebDAV.URL)
		u.Path = strings.TrimSuffix(u.Path, "/") + "/" + object
		endpoint = u.String()
	} else {
		endpoint = driveFiles + "/" + url.PathEscape(object)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?fields=parents,appProperties", nil)
		if err != nil {
			return nil, &uploadError{code: "invalid_request"}
		}
		request.Header.Set("Authorization", "Bearer "+c.DriveAccessToken)
		r, err := client.Do(request)
		if err != nil {
			return nil, classify(err)
		}
		if r.StatusCode != http.StatusOK {
			r.Body.Close()
			return nil, statusError(r.StatusCode)
		}
		var metadata struct {
			Parents       []string          `json:"parents"`
			AppProperties map[string]string `json:"appProperties"`
		}
		if err := driveJSON(r, &metadata); err != nil {
			return nil, err
		}
		parent := false
		for _, id := range metadata.Parents {
			if id == d.Drive.FolderID {
				parent = true
			}
		}
		if !parent || metadata.AppProperties["lanqinDestination"] != d.ID || metadata.AppProperties["lanqinSHA256"] != expected {
			return nil, errors.New("Drive object is outside the selected backup destination")
		}
		endpoint += "?alt=media"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, &uploadError{code: "invalid_request"}
	}
	if d.Type == "webdav" {
		request.SetBasicAuth(c.Username, c.Password)
	} else {
		request.Header.Set("Authorization", "Bearer "+c.DriveAccessToken)
	}
	r, err := client.Do(request)
	if err != nil {
		return nil, classify(err)
	}
	if r.StatusCode != http.StatusOK {
		r.Body.Close()
		return nil, statusError(r.StatusCode)
	}
	return r.Body, nil
}
