package storage

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"golang.org/x/crypto/hkdf"
)

var errCheckpointFile = errors.New("private upload checkpoint unavailable, invalid or already in use")

// FileCheckpointStore keeps CLI upload sessions encrypted, with an OS process
// lock for the whole invocation. A dead process releases the lock automatically;
// there is no unsafe age-based lock stealing or plaintext session file.
type FileCheckpointStore struct {
	mu        sync.Mutex
	root      *os.Root
	lock      *os.File
	cipher    cipher.AEAD
	namespace string
	known     map[string]bool
}

func OpenFileCheckpointStore(directory, namespace string, key []byte) (*FileCheckpointStore, error) {
	if !identifierPattern.MatchString(namespace) || len(key) != 32 {
		return nil, errCheckpointFile
	}
	directory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, errCheckpointFile
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		return nil, errCheckpointFile
	}
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() || !checkpointPrivateDirectory(info) {
		return nil, errCheckpointFile
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errCheckpointFile
	}
	fail := func() (*FileCheckpointStore, error) { root.Close(); return nil, errCheckpointFile }
	name := ".lanqin-" + namespace + ".lock"
	lockInfo, lockErr := root.Lstat(name)
	if lockErr == nil && !lockInfo.Mode().IsRegular() || lockErr != nil && !errors.Is(lockErr, os.ErrNotExist) {
		return fail()
	}
	lock, err := root.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fail()
	}
	opened, err := lock.Stat()
	current, pathErr := root.Lstat(name)
	if err != nil || pathErr != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) || checkpointLock(lock) != nil {
		lock.Close()
		return fail()
	}
	derived := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, key, nil, []byte("lanqin/backup/upload-checkpoints/v1/"+namespace)), derived); err != nil {
		lock.Close()
		return fail()
	}
	block, err := aes.NewCipher(derived)
	clear(derived)
	if err != nil {
		lock.Close()
		return fail()
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		lock.Close()
		return fail()
	}
	store := &FileCheckpointStore{root: root, lock: lock, cipher: aead, namespace: namespace, known: map[string]bool{}}
	if err := store.loadIndex(errors.Is(lockErr, os.ErrNotExist)); err != nil {
		store.Close()
		return nil, errCheckpointFile
	}
	return store, nil
}

func (s *FileCheckpointStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root == nil {
		return nil
	}
	err := s.lock.Close()
	closeErr := s.root.Close()
	s.root, s.lock, s.cipher = nil, nil, nil
	if err != nil || closeErr != nil {
		return errCheckpointFile
	}
	return nil
}

func (s *FileCheckpointStore) name(id string) (string, []byte, error) {
	if s.root == nil || !identifierPattern.MatchString(id) {
		return "", nil, errCheckpointFile
	}
	aad := []byte("lanqin-upload-checkpoint-v1\x00" + s.namespace + "\x00" + id)
	hash := sha256.Sum256(aad)
	return "checkpoint-" + hex.EncodeToString(hash[:]) + ".bin", aad, nil
}

func (s *FileCheckpointStore) Load(ctx context.Context, id string) (*Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	name, aad, err := s.name(id)
	if err != nil {
		return nil, err
	}
	plain, err := s.readPlain(ctx, name, aad)
	if err != nil {
		return nil, err
	}
	if plain == nil {
		if s.known[id] {
			return nil, errCheckpointFile
		}
		return nil, nil
	}
	defer clear(plain)
	if !s.known[id] {
		return nil, errCheckpointFile
	}
	var value Checkpoint
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, errCheckpointFile
	}
	return &value, nil
}

func (s *FileCheckpointStore) readPlain(ctx context.Context, name string, aad []byte) ([]byte, error) {
	info, err := s.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, errCheckpointFile
	}
	file, err := s.root.Open(name)
	if err != nil {
		return nil, errCheckpointFile
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(opened, info) {
		return nil, errCheckpointFile
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx, file}, (1<<20)+1))
	n := s.cipher.NonceSize()
	if err != nil || len(data) > 1<<20 || len(data) < n+s.cipher.Overhead() {
		return nil, errCheckpointFile
	}
	plain, err := s.cipher.Open(nil, data[:n], data[n:], aad)
	if err != nil {
		return nil, errCheckpointFile
	}
	return plain, nil
}

func (s *FileCheckpointStore) Save(ctx context.Context, id string, value Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	name, aad, err := s.name(id)
	if err != nil {
		return err
	}
	plain, err := json.Marshal(value)
	if err != nil || len(plain) > 900<<10 {
		return errCheckpointFile
	}
	defer clear(plain)
	if !s.known[id] {
		if len(s.known) >= 64 {
			return errCheckpointFile
		}
		s.known[id] = true
		if err := s.saveIndex(ctx); err != nil {
			return err
		}
	} else if _, err := s.root.Lstat(name); err != nil {
		return errCheckpointFile // A recorded checkpoint must not silently restart.
	}
	return s.writePlain(ctx, name, aad, plain)
}

func (s *FileCheckpointStore) writePlain(ctx context.Context, name string, aad, plain []byte) error {
	nonce := make([]byte, s.cipher.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return errCheckpointFile
	}
	data := s.cipher.Seal(nonce, nonce, plain, aad)
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return errCheckpointFile
	}
	temporary := ".checkpoint-" + hex.EncodeToString(random[:]) + ".partial"
	file, err := s.root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errCheckpointFile
	}
	defer s.root.Remove(temporary)
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return errCheckpointFile
	}
	if err := file.Sync(); err != nil {
		return errCheckpointFile
	}
	if err := file.Close(); err != nil {
		return errCheckpointFile
	}
	if info, err := s.root.Lstat(name); err == nil && !info.Mode().IsRegular() || err != nil && !errors.Is(err, os.ErrNotExist) {
		return errCheckpointFile
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := checkpointReplace(s.root, temporary, name); err != nil {
		return errCheckpointFile
	}
	return nil
}

type checkpointIndex struct {
	Version int      `json:"version"`
	IDs     []string `json:"ids"`
}

func (s *FileCheckpointStore) indexName() (string, []byte) {
	aad := []byte("lanqin-upload-checkpoint-index-v1\x00" + s.namespace)
	hash := sha256.Sum256(aad)
	return "checkpoint-" + hex.EncodeToString(hash[:]) + ".index", aad
}

func (s *FileCheckpointStore) loadIndex(mayInitialize bool) error {
	name, aad := s.indexName()
	plain, err := s.readPlain(context.Background(), name, aad)
	if err != nil {
		return err
	}
	if plain == nil {
		if !mayInitialize {
			return errCheckpointFile
		}
		return s.saveIndex(context.Background())
	}
	defer clear(plain)
	var index checkpointIndex
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&index) != nil || decoder.Decode(new(any)) != io.EOF || index.Version != 1 || len(index.IDs) > 64 {
		return errCheckpointFile
	}
	for _, id := range index.IDs {
		if !identifierPattern.MatchString(id) || s.known[id] {
			return errCheckpointFile
		}
		s.known[id] = true
	}
	return nil
}

func (s *FileCheckpointStore) saveIndex(ctx context.Context) error {
	ids := []string{}
	for id := range s.known {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	plain, err := json.Marshal(checkpointIndex{Version: 1, IDs: ids})
	if err != nil {
		return errCheckpointFile
	}
	defer clear(plain)
	name, aad := s.indexName()
	return s.writePlain(ctx, name, aad, plain)
}
