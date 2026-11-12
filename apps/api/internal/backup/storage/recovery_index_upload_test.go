package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type recoveryIndexStub struct {
	putFn   func(context.Context, string, *io.SectionReader, *artifact) error
	indexFn func(context.Context, string, string, int64, time.Time, []byte, func(context.Context) error) error
}

func (u recoveryIndexStub) put(ctx context.Context, object string, reader *io.SectionReader, a *artifact) error {
	return u.putFn(ctx, object, reader, a)
}
func (u recoveryIndexStub) close() {}
func (u recoveryIndexStub) publishRecoveryIndex(ctx context.Context, object, sha string, size int64, at time.Time, key []byte, guard func(context.Context) error) error {
	return u.indexFn(ctx, object, sha, size, at, key, guard)
}

func TestUploadPublishesIndependentIndexWithoutLosingSuccessfulCiphertext(t *testing.T) {
	for _, scenario := range []string{"legacy", "success", "publish-failed", "canceled-during-index", "guard-rejected", "index-unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			file, sha, ciphertext := fixture(t)
			d := s3Destination("index")
			opts := DefaultOptions()
			callerKey := bytes.Repeat([]byte{0x42}, 32)
			if scenario != "legacy" {
				opts.RecoveryIndexKey = callerKey
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			puts, indexes, guards := 0, 0, 0
			var privateKey []byte
			opts.RecoveryIndexGuard = func(context.Context, string) error {
				guards++
				if scenario == "guard-rejected" {
					return errors.New("synthetic private guard error")
				}
				return nil
			}
			put := func(ctx context.Context, object string, reader *io.SectionReader, a *artifact) error {
				puts++
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("ciphertext upload had no attempt deadline")
				}
				data, err := io.ReadAll(reader)
				if err != nil || !bytes.Equal(data, ciphertext) {
					t.Fatal("ciphertext upload changed payload", err)
				}
				return nil
			}
			publisher := recoveryIndexStub{putFn: put, indexFn: func(ctx context.Context, object, digest string, size int64, at time.Time, key []byte, guard func(context.Context) error) error {
				indexes++
				privateKey = key
				if puts != 1 || digest != sha || size != int64(len(ciphertext)) || at.IsZero() || !strings.HasPrefix(object, d.S3.Prefix+"/lanqin-") || !bytes.Equal(key, callerKey) {
					t.Fatal("index was not bound to successful ciphertext receipt")
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("index publishing escaped upload attempt deadline")
				}
				if scenario == "canceled-during-index" {
					cancel()
				}
				if err := guard(ctx); err != nil {
					return err
				}
				if scenario == "publish-failed" {
					return errors.New("private-provider-response-do-not-echo")
				}
				return nil
			}}
			report, err := upload(ctx, file, sha, Config{Version: 1, Destinations: []Destination{d}}, opts, func(Destination) (uploader, error) {
				if scenario == "index-unavailable" {
					return stubUploader{put}, nil
				}
				return publisher, nil
			})
			failed := scenario != "legacy" && scenario != "success"
			if (err != nil) != failed || failed && (!errors.Is(err, ErrRecoveryIndexFailure) || !errors.Is(err, ErrPartialFailure)) {
				t.Fatal("index failure not reported independently", err)
			}
			if len(report.Destinations) != 1 || puts != 1 {
				t.Fatal("index failure retried or omitted successful ciphertext")
			}
			result := report.Destinations[0]
			if result.State != "succeeded" || result.ObjectKey == "" || result.CompletedAt == nil || result.ErrorCode != "" {
				t.Fatal("index failure erased durable upload receipt", result)
			}
			if scenario == "legacy" {
				if result.RecoveryIndexState != "" || indexes != 0 || guards != 0 {
					t.Fatal("legacy upload silently published an index")
				}
			} else if failed {
				if result.RecoveryIndexState != "failed" || result.RecoveryIndexErrorCode == "" {
					t.Fatal("failed index advertised independently recoverable backup")
				}
			} else if result.RecoveryIndexState != "succeeded" || result.RecoveryIndexErrorCode != "" || indexes != 1 || guards != 2 {
				t.Fatal("confirmed index result missing", result)
			}
			if len(privateKey) > 0 && !bytes.Equal(privateKey, make([]byte, len(privateKey))) {
				t.Fatal("upload retained private index-key copy after return")
			}
			if !bytes.Equal(callerKey, bytes.Repeat([]byte{0x42}, 32)) {
				t.Fatal("upload cleared caller-owned recovery index key")
			}
			encoded, _ := json.Marshal(report)
			if strings.Contains(string(encoded), "do-not-echo") || bytes.Contains(encoded, callerKey) {
				t.Fatal("index report leaked provider or key material")
			}
		})
	}
}

func TestUploadRecoveryIndexKeyPreflightAndSerialization(t *testing.T) {
	file, sha, _ := fixture(t)
	d := s3Destination("index")
	for _, length := range []int{1, 31, 33, 64} {
		opts := DefaultOptions()
		opts.RecoveryIndexKey = bytes.Repeat([]byte{0x42}, length)
		called := false
		if _, err := upload(t.Context(), file, sha, Config{Version: 1, Destinations: []Destination{d}}, opts, func(Destination) (uploader, error) { called = true; return nil, nil }); err == nil || called {
			t.Fatal("bad index key reached credentials/transport")
		}
	}
	opts := DefaultOptions()
	opts.RecoveryIndexKey = bytes.Repeat([]byte{0x42}, 32)
	opts.RecoveryIndexGuard = func(context.Context, string) error { return nil }
	encoded, err := json.Marshal(opts)
	if err != nil || bytes.Contains(encoded, []byte("RecoveryIndexKey")) || bytes.Contains(encoded, []byte("RecoveryIndexGuard")) {
		t.Fatal("options serialized secret or authorization callback", err)
	}
}
