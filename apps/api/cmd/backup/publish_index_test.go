package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"lanqin-email-api/internal/backup/storage"
)

func TestPublishIndexCLIRequiresTrustedReceiptAndExplicitKeyBeforeRemoteWork(t *testing.T) {
	f := newFetchFixture(t)
	t.Setenv(discoveryTestKeyEnv, base64.StdEncoding.EncodeToString(discoveryTestKey()))
	base := []string{"--config=" + f.config, "--destination=s3_a", "--object-key=first/" + f.object, "--expected-sha256=" + f.receipt, "--bytes=" + fmt.Sprint(len(f.data)), "--index-key-env=" + discoveryTestKeyEnv}
	for i := range base {
		var output bytes.Buffer
		args := append(append([]string{}, base[:i]...), base[i+1:]...)
		err := runPublishIndexWithPublisher(t.Context(), args, &output, func(context.Context, storage.Destination, string, string, int64, time.Time, []byte, storage.Credentials) error {
			t.Fatal("incomplete index request reached remote work")
			return nil
		})
		if err == nil || output.Len() != 0 {
			t.Fatal("incomplete trusted publication accepted", i)
		}
	}
	for _, invalid := range []string{"--index-key-env=raw-secret-do-not-echo", "--bytes=0", "--bytes=5368709121", "--expected-sha256=untrusted-secret-do-not-echo", "--secret=do-not-echo", "--destination=unknown"} {
		var output bytes.Buffer
		err := runPublishIndexWithPublisher(t.Context(), append(append([]string{}, base...), invalid), &output, func(context.Context, storage.Destination, string, string, int64, time.Time, []byte, storage.Credentials) error {
			t.Fatal("invalid request reached remote work")
			return nil
		})
		if err == nil || output.Len() != 0 || strings.Contains(err.Error(), "do-not-echo") {
			t.Fatal("invalid publication accepted or leaked")
		}
	}
	for _, fail := range []bool{false, true} {
		var output bytes.Buffer
		called := false
		err := runPublishIndexWithPublisher(t.Context(), base, &output, func(ctx context.Context, d storage.Destination, object, sha string, size int64, at time.Time, key []byte, c storage.Credentials) error {
			called = true
			if d.ID != "s3_a" || object != "first/"+f.object || sha != f.receipt || size != int64(len(f.data)) || at.IsZero() || !bytes.Equal(key, discoveryTestKey()) || c.SecretAccessKey != "a-synthetic-secret" || c.Password != "" {
				t.Fatal("index publication lost independent authority/credential binding")
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("publication unbounded")
			}
			if fail {
				return errors.New("provider-do-not-echo")
			}
			return nil
		})
		if !called || (err != nil) != fail {
			t.Fatal("publication outcome wrong", err)
		}
		if fail {
			if output.Len() != 0 || strings.Contains(err.Error(), "do-not-echo") {
				t.Fatal("publication failure leaked or emitted success")
			}
		} else {
			var receipt struct{ DestinationID, ObjectKey, IndexState string }
			if json.Unmarshal(output.Bytes(), &receipt) != nil || receipt.DestinationID != "s3_a" || receipt.ObjectKey != "first/"+f.object || receipt.IndexState != "succeeded" {
				t.Fatal("publication receipt wrong")
			}
		}
	}
}
