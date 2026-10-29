package storage

import (
	"bytes"
	"context"
	"crypto/md5" // S3 Content-MD5 transport check only; receipts use SHA-256.
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"sync"
	"time"
)

const MaxUploadBytes int64 = 5 << 30 // Single PUT only; multipart is not implemented.

var ErrPartialFailure = errors.New("one or more backup destinations did not succeed; inspect the JSON report")

type Options struct {
	Concurrency    int
	Attempts       int
	AttemptTimeout time.Duration
	MaxBytes       int64
	SpoolDir       string
}

func DefaultOptions() Options {
	return Options{Concurrency: 2, Attempts: 3, AttemptTimeout: 20 * time.Minute, MaxBytes: 1 << 30}
}

type Report struct {
	SHA256       string   `json:"sha256"`
	Bytes        int64    `json:"bytes"`
	Destinations []Result `json:"destinations"`
}

type Result struct {
	DestinationID string     `json:"destination_id"`
	State         string     `json:"state"`
	Attempts      int        `json:"attempts"`
	ObjectKey     string     `json:"object_key,omitempty"`
	ErrorCode     string     `json:"error_code,omitempty"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
}

type artifact struct {
	file   *os.File
	size   int64
	sha256 string
	md5    string
}

type uploader interface {
	put(context.Context, string, *io.SectionReader, *artifact) error
	close()
}

type uploadError struct {
	code  string
	retry bool
}

func (e *uploadError) Error() string { return e.code }

type uploaderFactory func(Destination) (uploader, error)

// Upload stages and hashes the source once, then gives each upload an independent
// reader over the private immutable copy. Credentials and provider errors are
// never included in the report. A successful result means remote PUT acceptance,
// not an independently downloaded or decrypted recovery verification.
func Upload(ctx context.Context, source, expectedSHA256 string, cfg Config, opts Options) (Report, error) {
	return upload(ctx, source, expectedSHA256, cfg, opts, newUploader)
}

func upload(ctx context.Context, source, expectedSHA256 string, cfg Config, opts Options, factory uploaderFactory) (Report, error) {
	var report Report
	if err := cfg.Validate(); err != nil {
		return report, err
	}
	if opts.Concurrency < 1 || opts.Concurrency > 8 || opts.Attempts < 1 || opts.Attempts > 3 || opts.AttemptTimeout <= 0 || opts.AttemptTimeout > time.Hour || opts.MaxBytes < 1 || opts.MaxBytes > MaxUploadBytes {
		return report, errors.New("invalid upload limits (concurrency 1..8, attempts 1..3, timeout up to 1h, size up to 5 GiB)")
	}
	active := 0
	for _, d := range cfg.Destinations {
		if !d.Disabled {
			active++
		}
	}
	if active == 0 {
		return report, errors.New("no enabled backup destinations")
	}
	data, err := stage(ctx, source, expectedSHA256, opts)
	if err != nil {
		return report, err
	}
	defer os.Remove(data.file.Name()) // Only our own ciphertext spool file.
	defer data.file.Close()
	report.SHA256, report.Bytes = data.sha256, data.size
	report.Destinations = make([]Result, len(cfg.Destinations))
	jobs := make(chan int, len(cfg.Destinations))
	for i, d := range cfg.Destinations {
		report.Destinations[i] = Result{DestinationID: d.ID, State: "skipped"}
		if !d.Disabled {
			jobs <- i
		}
	}
	close(jobs)
	var workers sync.WaitGroup
	for range min(opts.Concurrency, active) {
		workers.Go(func() {
			for i := range jobs {
				report.Destinations[i] = uploadDestination(ctx, cfg.Destinations[i], data, opts, factory)
			}
		})
	}
	workers.Wait()
	for _, result := range report.Destinations {
		if result.State != "succeeded" && result.State != "skipped" {
			return report, ErrPartialFailure
		}
	}
	return report, nil
}

func uploadDestination(ctx context.Context, d Destination, data *artifact, opts Options, factory uploaderFactory) Result {
	result := Result{DestinationID: d.ID, State: "failed"}
	if ctx.Err() != nil {
		result.State, result.ErrorCode = "cancelled", "cancelled"
		return result
	}
	client, err := factory(d)
	if err != nil {
		result.ErrorCode = "destination_unavailable"
		return result
	}
	defer client.close()
	for attempt := 1; attempt <= opts.Attempts; attempt++ {
		if ctx.Err() != nil {
			result.State, result.ErrorCode = "cancelled", "cancelled"
			return result
		}
		// A fresh unpredictable name per attempt avoids overwriting either a
		// previous backup or an object accepted before a lost success response.
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			result.ErrorCode = "random_source_failed"
			return result
		}
		key := "lanqin-" + hex.EncodeToString(nonce[:]) + ".age"
		attemptCtx, cancel := context.WithTimeout(ctx, opts.AttemptTimeout)
		result.Attempts = attempt
		err := client.put(attemptCtx, key, io.NewSectionReader(data.file, 0, data.size), data)
		cancel()
		if err == nil {
			now := time.Now().UTC()
			result.State, result.ObjectKey, result.CompletedAt = "succeeded", objectKey(d, key), &now
			result.ErrorCode = ""
			return result
		}
		if ctx.Err() != nil {
			result.State, result.ErrorCode = "cancelled", "cancelled"
			return result
		}
		var classified *uploadError
		if !errors.As(err, &classified) {
			classified = &uploadError{code: "upload_failed"}
		}
		result.ErrorCode = classified.code
		if !classified.retry || attempt == opts.Attempts {
			return result
		}
		// Bounded exponential backoff with jitter, cancellable during shutdown.
		wait := time.Duration(1<<(attempt-1))*time.Second + time.Duration(nonce[0])*time.Millisecond
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			result.State, result.ErrorCode = "cancelled", "cancelled"
			return result
		case <-timer.C:
		}
	}
	return result
}

func objectKey(d Destination, name string) string {
	if d.S3 != nil && d.S3.Prefix != "" {
		return d.S3.Prefix + "/" + name
	}
	return name
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func stage(ctx context.Context, source, expected string, opts Options) (*artifact, error) {
	want, err := hex.DecodeString(expected)
	if err != nil || len(want) != sha256.Size {
		return nil, errors.New("upload requires a trusted ciphertext SHA-256 receipt")
	}
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("upload input must be a regular ciphertext file")
	}
	in, err := os.Open(source)
	if err != nil {
		return nil, errors.New("cannot open encrypted backup")
	}
	defer in.Close()
	opened, err := in.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Size() > opts.MaxBytes {
		return nil, errors.New("encrypted backup changed or exceeds upload size limit")
	}
	file, err := os.CreateTemp(opts.SpoolDir, ".lanqin-upload-*")
	if err != nil {
		return nil, errors.New("cannot create private ciphertext spool")
	}
	ok := false
	defer func() {
		if !ok {
			file.Close()
			os.Remove(file.Name())
		}
	}()
	sha, md := sha256.New(), md5.New()
	n, err := io.Copy(io.MultiWriter(file, sha, md), io.LimitReader(contextReader{ctx, in}, opts.MaxBytes+1))
	if err != nil || n > opts.MaxBytes || n != opened.Size() || ctx.Err() != nil {
		return nil, errors.New("cannot stage encrypted backup within limits")
	}
	if !bytes.Equal(sha.Sum(nil), want) {
		return nil, errors.New("ciphertext does not match trusted SHA-256 receipt; no upload attempted")
	}
	const ageHeader = "age-encryption.org/v1\n"
	header := make([]byte, len(ageHeader))
	if _, err := file.ReadAt(header, 0); err != nil || string(header) != ageHeader {
		return nil, errors.New("upload accepts binary age backups only, not plaintext")
	}
	// This detects an accidental plaintext input, not ciphertext authentication.
	// Full age authentication still requires the private recovery key at restore.
	ok = true
	return &artifact{file: file, size: n, sha256: hex.EncodeToString(want), md5: base64.StdEncoding.EncodeToString(md.Sum(nil))}, nil
}
