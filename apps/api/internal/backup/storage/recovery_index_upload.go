package storage

import (
	"context"
	"net/http"
	"time"
)

// Each destination uses the exact credentials and TLS/client constructed for
// its ciphertext upload; publication never re-resolves ambient credentials.
func (s *s3Uploader) publishRecoveryIndex(ctx context.Context, object, sha string, size int64, completed time.Time, key []byte, guard func(context.Context) error) error {
	restore := recoveryIndexMethods(s.client, false)
	defer restore()
	return publishRecoveryIndex(ctx, s.destination, object, sha, size, completed, key, s.credentials, s.client, guard)
}

func (w *webdavUploader) publishRecoveryIndex(ctx context.Context, object, sha string, size int64, completed time.Time, key []byte, guard func(context.Context) error) error {
	restore := recoveryIndexMethods(w.client, false)
	defer restore()
	return publishRecoveryIndex(ctx, w.destination, object, sha, size, completed, key, Credentials{Username: w.username, Password: w.password}, w.client, guard)
}

func (d *driveUploader) publishRecoveryIndex(ctx context.Context, object, sha string, size int64, completed time.Time, key []byte, guard func(context.Context) error) error {
	restore := recoveryIndexMethods(d.client, true)
	defer restore()
	return publishRecoveryIndex(ctx, d.config, object, sha, size, completed, key, Credentials{DriveAccessToken: d.token}, d.client, guard)
}

func recoveryIndexMethods(client *http.Client, drive bool) func() {
	transport, ok := client.Transport.(*restrictedTransport)
	if !ok {
		return func() {}
	}
	previous := transport.methods
	transport.methods = map[string]bool{http.MethodGet: true, http.MethodPut: true}
	if drive {
		transport.methods[http.MethodPost] = true
	}
	return func() { transport.methods = previous }
}
