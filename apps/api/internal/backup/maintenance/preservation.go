package maintenance

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
	"lanqin-email-api/internal/backup/preservation"
)

var ErrPreservation = errors.New("maintenance preservation could not be verified; retain original containers and all preservation artifacts")

type preservationReceipt struct {
	Version        int    `json:"version"`
	ConfigSHA256   string `json:"configSHA256"`
	OperationID    string `json:"operationId"`
	ManifestSHA256 string `json:"manifestSHA256"`
	CipherSHA256   string `json:"cipherSHA256"`
	Bytes          int64  `json:"bytes"`
}

func preservationIdentity(cfg Config) (string, error) {
	if cfg.Preservation == nil {
		return "", ErrPreservation
	}
	value := os.Getenv(cfg.Preservation.IdentityEnv)
	if len(value) > 4096 {
		return "", ErrPreservation
	}
	value = strings.TrimSpace(value)
	identity, err := age.ParseX25519Identity(value)
	if err != nil || identity.Recipient().String() != cfg.Preservation.Recipient {
		return "", ErrPreservation
	}
	return value, nil
}

// EnsureSafePostfixQueues keeps the original empty-queue rule unless the plan
// explicitly opts into verified encrypted preservation. No queue is replayed.
func (c *ComposeController) EnsureSafePostfixQueues(ctx context.Context, cfg Config) error {
	if cfg.Preservation == nil {
		return c.EnsureEmptyPostfixQueues(ctx, cfg)
	}
	identity, err := preservationIdentity(cfg)
	if err != nil {
		return err
	}
	state, err := ReadState(cfg)
	if err != nil || !validOperationID(state.OperationID) {
		return ErrPreservation
	}
	items, want, err := c.preservationSnapshot(ctx, cfg, state)
	if err != nil {
		return ErrPreservation
	}
	defer clearPreservationItems(items)
	if err := savePreservation(ctx, cfg, identity, items, want); err != nil {
		return ErrPreservation
	}
	// Re-read the stopped sources after verifying the actual encrypted file.
	// A receipt for a previous queue, container set or environment is insufficient.
	current, observed, err := c.preservationSnapshot(ctx, cfg, state)
	if err != nil {
		return ErrPreservation
	}
	defer clearPreservationItems(current)
	expectedSHA, _ := preservationManifestSHA(want)
	actualSHA, _ := preservationManifestSHA(observed)
	if actualSHA != expectedSHA || c.VerifyStopped(ctx, cfg) != nil || VerifyPinnedFiles(cfg) != nil {
		return ErrPreservation
	}
	return nil
}

func clearPreservationItems(items []preservation.Item) {
	for _, item := range items {
		clear(item.Data)
	}
}

func (c *ComposeController) preservationSnapshot(ctx context.Context, cfg Config, state State) ([]preservation.Item, preservation.Manifest, error) {
	var empty preservation.Manifest
	archives, runtimeBinding, err := c.inspectPostfixArchives(ctx, cfg, false)
	if err != nil {
		return nil, empty, err
	}
	if !cfg.Preservation.QuarantineQueues {
		for _, archive := range archives {
			if validateEmptyPostfixArchive(ctx, archive.Data) != nil {
				clearPostfixArchives(archives)
				return nil, empty, ErrPreservation
			}
		}
	}
	items := []preservation.Item{{Name: "assets/0000.bin", Source: "maintenance-runtime-binding", Data: runtimeBinding}}
	success := false
	defer func() {
		if !success {
			clearPostfixArchives(archives)
			clearPreservationItems(items)
		}
	}()
	config, err := json.Marshal(cfg)
	if err != nil {
		return nil, empty, ErrPreservation
	}
	items = append(items, preservation.Item{Name: "assets/0001.bin", Source: "maintenance-plan", Data: config})
	for _, archive := range archives {
		items = append(items, preservation.Item{Name: "spools/" + archive.ID + ".tar", Source: "docker:" + archive.ID + ":/var/spool/postfix", Data: archive.Data})
	}
	environment, _, err := composeEnvironmentVersion(cfg)
	if err != nil {
		return nil, empty, ErrPreservation
	}
	selected := append(append([]PathDigest{}, cfg.ComposeFiles...), PathDigest{Path: cfg.EnvironmentFile.Path, SHA256: environment})
	if cfg.NextEnvironmentFile != nil {
		selected = append(selected, *cfg.NextEnvironmentFile)
	}
	selected = append(selected, cfg.Preservation.Assets...)
	remaining := preservation.DefaultLimits().MaxBytes
	for _, item := range items {
		remaining -= int64(len(item.Data))
	}
	for i, source := range selected {
		if remaining < 0 {
			return nil, empty, ErrPreservation
		}
		data, err := readPreservationAsset(ctx, source)
		if err != nil {
			return nil, empty, err
		}
		if int64(len(data)) > remaining {
			clear(data)
			return nil, empty, ErrPreservation
		}
		remaining -= int64(len(data))
		items = append(items, preservation.Item{Name: fmt.Sprintf("assets/%04d.bin", i+2), Source: source.Path, Data: data})
	}
	limits := preservation.DefaultLimits()
	if len(items) > limits.MaxEntries {
		return nil, empty, ErrPreservation
	}
	manifest := preservation.Manifest{Version: 1, Scope: "maintenance-preservation", Binding: preservation.Binding{ConfigSHA256: state.ConfigSHA256, OperationID: state.OperationID}, Entries: make([]preservation.Entry, 0, len(items))}
	remaining = limits.MaxBytes
	for _, item := range items {
		if int64(len(item.Data)) > remaining {
			return nil, empty, ErrPreservation
		}
		remaining -= int64(len(item.Data))
		digest := sha256.Sum256(item.Data)
		manifest.Entries = append(manifest.Entries, preservation.Entry{Name: item.Name, Source: item.Source, Bytes: int64(len(item.Data)), SHA256: hex.EncodeToString(digest[:])})
	}
	success = true
	return items, manifest, nil
}

func readPreservationAsset(ctx context.Context, source PathDigest) ([]byte, error) {
	info, err := checkedPath(source.Path, false)
	if err != nil || info.Size() > 8<<20 || !trustedMaintenanceInfo(info) || !trustedParents(source.Path) {
		return nil, ErrPreservation
	}
	file, err := os.Open(source.Path)
	if err != nil {
		return nil, ErrPreservation
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, ErrPreservation
	}
	data, err := io.ReadAll(&postfixArchiveReader{ctx: ctx, reader: io.LimitReader(file, (8<<20)+1)})
	after, statErr := file.Stat()
	digest := sha256.Sum256(data)
	if err != nil || statErr != nil || int64(len(data)) != info.Size() || !os.SameFile(opened, after) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) || !strings.EqualFold(hex.EncodeToString(digest[:]), source.SHA256) || bytes.Contains(data, []byte("AGE-SECRET-KEY-")) {
		clear(data)
		return nil, ErrPreservation
	}
	return data, nil
}

func preservationManifestSHA(manifest preservation.Manifest) (string, error) {
	data, err := json.Marshal(manifest)
	if err != nil {
		return "", ErrPreservation
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func savePreservation(ctx context.Context, cfg Config, identity string, items []preservation.Item, want preservation.Manifest) error {
	digest, err := preservationManifestSHA(want)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(cfg.StateDir)
	if err != nil {
		return ErrPreservation
	}
	defer root.Close()
	name := "preservation-" + digest
	var receipt preservationReceipt
	data, err := readPreservationPrivate(root, name+".json", 4096)
	if err == nil {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&receipt) != nil || decoder.Decode(new(any)) != io.EOF || receipt.Version != 1 || receipt.ConfigSHA256 != want.Binding.ConfigSHA256 || receipt.OperationID != want.Binding.OperationID || receipt.ManifestSHA256 != digest || !validDigest(receipt.CipherSHA256) || receipt.Bytes < 1 {
			return ErrPreservation
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrPreservation
	}
	if _, err := root.Lstat(name + ".age"); errors.Is(err, os.ErrNotExist) {
		if receipt.Version != 0 || preservationCapacity(root) != nil {
			return ErrPreservation
		}
		if err := publishPreservation(root, cfg.StateDir, name+".age", func(file *os.File) error {
			written, err := preservation.Write(ctx, file, cfg.Preservation.Recipient, want.Binding, items, preservation.DefaultLimits())
			writtenSHA, hashErr := preservationManifestSHA(written)
			if err != nil || hashErr != nil || writtenSHA != digest {
				return ErrPreservation
			}
			return nil
		}); err != nil {
			return err
		}
	} else if err != nil {
		return ErrPreservation
	}
	actualSHA, size, err := verifyPreservationFile(ctx, root, name+".age", identity, receipt.CipherSHA256, want)
	if err != nil || receipt.Version != 0 && receipt.Bytes != size {
		return ErrPreservation
	}
	if receipt.Version != 0 {
		return syncStateDirectory(cfg.StateDir)
	}
	// Adopting an interrupted publication still requires a complete decryption
	// against the current source manifest, never just a filename or READY marker.
	receipt = preservationReceipt{1, want.Binding.ConfigSHA256, want.Binding.OperationID, digest, actualSHA, size}
	data, err = json.Marshal(receipt)
	if err != nil {
		return ErrPreservation
	}
	return publishPreservation(root, cfg.StateDir, name+".json", func(file *os.File) error { _, err := file.Write(append(data, '\n')); return err })
}

func readPreservationPrivate(root *os.Root, name string, limit int64) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !privateInfo(info) || info.Size() < 1 || info.Size() > limit {
		return nil, ErrPreservation
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, ErrPreservation
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, ErrPreservation
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	after, statErr := file.Stat()
	if err != nil || statErr != nil || int64(len(data)) != info.Size() || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return nil, ErrPreservation
	}
	return data, nil
}

func verifyPreservationFile(ctx context.Context, root *os.Root, name, identity, expected string, want preservation.Manifest) (string, int64, error) {
	info, err := root.Lstat(name)
	maximum := preservation.DefaultLimits().MaxBytes + (2 << 20)
	if err != nil || !info.Mode().IsRegular() || !privateInfo(info) || info.Size() < 1 || info.Size() > maximum {
		return "", 0, ErrPreservation
	}
	file, err := root.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return "", 0, ErrPreservation
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", 0, ErrPreservation
	}
	hash := sha256.New()
	n, err := io.Copy(hash, &postfixArchiveReader{ctx: ctx, reader: io.LimitReader(file, maximum+1)})
	digest := hex.EncodeToString(hash.Sum(nil))
	if err != nil || n != info.Size() || expected != "" && digest != expected {
		return "", 0, ErrPreservation
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil || preservation.Verify(ctx, file, identity, digest, want, preservation.DefaultLimits()) != nil {
		return "", 0, ErrPreservation
	}
	after, err := file.Stat()
	if err != nil || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) || file.Sync() != nil {
		return "", 0, ErrPreservation
	}
	return digest, n, nil
}

func preservationCapacity(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return ErrPreservation
	}
	defer directory.Close()
	count := 0
	for scanned := 0; scanned <= 256; scanned++ {
		entries, err := directory.ReadDir(1)
		if err == io.EOF {
			if count < 16 {
				return nil
			}
			return ErrPreservation
		}
		if err != nil || len(entries) != 1 || scanned == 256 {
			return ErrPreservation
		}
		if strings.HasPrefix(entries[0].Name(), "preservation-") && strings.HasSuffix(entries[0].Name(), ".age") {
			count++
		}
	}
	return ErrPreservation
}

func publishPreservation(root *os.Root, directory, name string, write func(*os.File) error) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ErrPreservation
	}
	temporary := ".preservation-" + hex.EncodeToString(nonce[:]) + ".tmp"
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return ErrPreservation
	}
	defer root.Remove(temporary)
	defer file.Close()
	if write(file) != nil || file.Sync() != nil || file.Close() != nil || root.Link(temporary, name) != nil || syncStateDirectory(directory) != nil {
		return ErrPreservation
	}
	return nil
}
