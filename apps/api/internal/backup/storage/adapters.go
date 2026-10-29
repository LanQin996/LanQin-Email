package storage

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"unicode"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/logging"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

func newUploader(d Destination) (uploader, error) {
	return buildUploader(d, os.LookupEnv, nil)
}

// An injected client is only used by in-package protocol tests. Production
// always constructs a TLS-verifying, public-IP-only, no-redirect client.
func buildUploader(d Destination, env func(string) (string, bool), client *http.Client) (uploader, error) {
	secret := func(ref string) (string, error) {
		value, ok := env(ref)
		if !ok || value == "" || len(value) > 65536 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return "", errors.New("credential environment variable unavailable or invalid")
		}
		return value, nil
	}
	cleanup := func() {}
	switch d.Type {
	case "s3":
		cfg := d.S3
		key, err := secret(cfg.AccessKeyIDEnv)
		if err != nil {
			return nil, err
		}
		password, err := secret(cfg.SecretAccessKeyEnv)
		if err != nil {
			return nil, err
		}
		token := ""
		if cfg.SessionTokenEnv != "" {
			token, err = secret(cfg.SessionTokenEnv)
			if err != nil {
				return nil, err
			}
		}
		if client == nil {
			u, _ := endpointURL(cfg.Endpoint) // Config.Validate ran before construction.
			virtualHost := cfg.Bucket + "." + u.Hostname()
			if port := u.Port(); port != "" {
				virtualHost = net.JoinHostPort(virtualHost, port)
			}
			client, cleanup = secureClient(u.Host, virtualHost)
		}
		credentials := aws.Credentials{AccessKeyID: key, SecretAccessKey: password, SessionToken: token, Source: "LanQinBackupDestination"}
		api := s3.New(s3.Options{
			BaseEndpoint: aws.String(cfg.Endpoint), Region: cfg.Region,
			UsePathStyle: cfg.PathStyle, HTTPClient: s3HTTPClient{client},
			Credentials:      aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) { return credentials, nil }),
			RetryMaxAttempts: 1, Logger: logging.Nop{},
			RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		})
		return &s3Uploader{api: api, bucket: cfg.Bucket, prefix: cfg.Prefix, cleanup: cleanup}, nil
	case "webdav":
		cfg := d.WebDAV
		username, err := secret(cfg.UsernameEnv)
		if err != nil || strings.Contains(username, ":") {
			return nil, errors.New("webdav username environment variable unavailable or invalid")
		}
		password, err := secret(cfg.PasswordEnv)
		if err != nil {
			return nil, err
		}
		u, _ := endpointURL(cfg.URL)
		if client == nil {
			client, cleanup = secureClient(u.Host)
		}
		return &webdavUploader{client: client, base: u, username: username, password: password, cleanup: cleanup}, nil
	default:
		return nil, errors.New("unsupported destination type")
	}
}

type s3Uploader struct {
	api            *s3.Client
	bucket, prefix string
	cleanup        func()
}

// The SDK normally accepts any 2xx status and reads error XML into memory.
// Require PutObject's final 200 response (not an asynchronous 202) and bound
// response bodies before handing them to the deserializer.
type s3HTTPClient struct{ client *http.Client }

func (c s3HTTPClient) Do(req *http.Request) (*http.Response, error) {
	response, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 && response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, &uploadError{code: "unexpected_success_status"}
	}
	response.Body = struct {
		io.Reader
		io.Closer
	}{io.LimitReader(response.Body, 64<<10), response.Body}
	return response, nil
}

func (s *s3Uploader) close() { s.cleanup() }

func (s *s3Uploader) put(ctx context.Context, name string, body *io.SectionReader, data *artifact) error {
	key := name
	if s.prefix != "" {
		key = s.prefix + "/" + name
	}
	_, err := s.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), Body: body,
		ContentLength: aws.Int64(data.size), ContentType: aws.String("application/octet-stream"),
		ContentMD5: aws.String(data.md5), IfNoneMatch: aws.String("*"),
	})
	return classify(err)
}

type webdavUploader struct {
	client             *http.Client
	base               *url.URL
	username, password string
	cleanup            func()
}

func (w *webdavUploader) close() { w.cleanup() }

func (w *webdavUploader) put(ctx context.Context, name string, body *io.SectionReader, data *artifact) error {
	u := *w.base
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), body)
	if err != nil {
		return &uploadError{code: "invalid_request"}
	}
	req.ContentLength = data.size
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("If-None-Match", "*")
	req.SetBasicAuth(w.username, w.password)
	response, err := w.client.Do(req)
	if err != nil {
		return classify(err)
	}
	defer response.Body.Close()
	// Do not log or retain remote bodies, even when they claim to be errors.
	// A server may echo the Authorization header, query, or credential material.
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusOK {
		return statusError(response.StatusCode)
	}
	return nil
}

func classify(err error) error {
	if err == nil {
		return nil
	}
	var classified *uploadError
	if errors.As(err, &classified) {
		return classified
	}
	if errors.Is(err, errEndpointBlocked) {
		return &uploadError{code: "endpoint_blocked"}
	}
	if errors.Is(err, context.Canceled) {
		return &uploadError{code: "cancelled"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &uploadError{code: "timeout", retry: true}
	}
	// x509 errors can be wrapped by either the SDK or net/http.
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &unknown) || errors.As(err, &hostname) || errors.As(err, &invalid) {
		return &uploadError{code: "tls_error"}
	}
	var response *smithyhttp.ResponseError
	if errors.As(err, &response) {
		return statusError(response.HTTPStatusCode())
	}
	return &uploadError{code: "network_error", retry: true}
}

func statusError(status int) error {
	switch {
	case status == 401 || status == 403:
		return &uploadError{code: "access_denied"}
	case status == 409:
		return &uploadError{code: "conflict", retry: true}
	case status == 412:
		return &uploadError{code: "object_exists"}
	case status == 408 || status == 429 || status >= 500:
		return &uploadError{code: "remote_temporary_error", retry: true}
	case status >= 300 && status < 400:
		return &uploadError{code: "redirect_refused"}
	default:
		return &uploadError{code: "remote_rejected"}
	}
}
