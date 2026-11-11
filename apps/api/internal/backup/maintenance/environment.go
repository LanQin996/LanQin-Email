package maintenance

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var ErrUnsupported = errors.New("maintenance environment switching requires Linux")
var ErrEnvironment = errors.New("maintenance environment transaction unavailable, changed or incomplete; retain environment journal and sibling files")

type environmentOps struct {
	rename        func(string, string) error
	syncDirectory func(string) error
}

type environmentJournal struct {
	Version        int      `json:"version"`
	ConfigSHA256   string   `json:"configSHA256"`
	OriginalPath   string   `json:"originalPath"`
	OriginalSHA256 string   `json:"originalSHA256"`
	NextSHA256     string   `json:"nextSHA256"`
	Events         []string `json:"-"`
}

type environmentPaths struct{ live, old, staged, nextSaved, journal, parent, configSHA string }

// ApplyEnvironment installs only the explicitly pinned complete next environment
// after the coordinator has stopped every writer. The original file remains in
// a derived sibling in its own filesystem; no cross-device rename is required.
func ApplyEnvironment(ctx context.Context, cfg Config) error {
	if runtime.GOOS != "linux" {
		return ErrUnsupported
	}
	return applyEnvironment(ctx, cfg, productionEnvironmentOps())
}

func RollbackEnvironment(ctx context.Context, cfg Config) error {
	if runtime.GOOS != "linux" {
		return ErrUnsupported
	}
	return rollbackEnvironment(ctx, cfg, productionEnvironmentOps())
}

// EnvironmentStatus never changes a path. A configured native environment is
// considered next only after its journal's durable installation boundary.
func EnvironmentStatus(cfg Config) (string, error) {
	paths, err := checkedEnvironmentPaths(cfg)
	if err != nil {
		return "incomplete", ErrEnvironment
	}
	if cfg.NextEnvironmentFile == nil {
		if _, err := environmentFileHash(paths.live, cfg.EnvironmentFile.SHA256); err != nil {
			return "incomplete", ErrEnvironment
		}
		return "original", nil
	}
	journal, err := readEnvironmentJournal(paths, cfg)
	if errors.Is(err, os.ErrNotExist) {
		if _, err := environmentFileHash(paths.live, cfg.EnvironmentFile.SHA256); err != nil {
			return "incomplete", ErrEnvironment
		}
		if !environmentAbsent(paths.old) || !environmentAbsent(paths.staged) || !environmentAbsent(paths.nextSaved) {
			return "incomplete", ErrEnvironment
		}
		return "original", nil
	}
	if err != nil {
		return "incomplete", ErrEnvironment
	}
	live := environmentKnownFile(paths.live, cfg)
	old := environmentKnownFile(paths.old, cfg)
	next := environmentKnownFile(paths.nextSaved, cfg)
	if live == "original" && old == "absent" && (next == "absent" || next == "next") {
		return "original", nil
	}
	if live == "next" && old == "original" && next == "absent" && environmentAbsent(paths.staged) && len(journal.Events) > 0 && journal.Events[len(journal.Events)-1] == "next-installed" {
		return "next", nil
	}
	return "incomplete", ErrEnvironment
}

func checkedEnvironmentPaths(cfg Config) (environmentPaths, error) {
	var empty environmentPaths
	if !filepath.IsAbs(cfg.EnvironmentFile.Path) || filepath.Clean(cfg.EnvironmentFile.Path) != cfg.EnvironmentFile.Path || strings.IndexFunc(cfg.EnvironmentFile.Path, func(r rune) bool { return r < ' ' || r == 0x7f }) >= 0 {
		return empty, ErrEnvironment
	}
	if !validDigest(cfg.EnvironmentFile.SHA256) {
		return empty, ErrEnvironment
	}
	if !environmentAbsent(cfg.EnvironmentFile.Path) {
		if _, err := checkedPath(cfg.EnvironmentFile.Path, false); err != nil {
			return empty, ErrEnvironment
		}
	}
	parent := filepath.Dir(cfg.EnvironmentFile.Path)
	parentInfo, err := checkedPath(parent, true)
	if err != nil || !environmentParentPrivate(parentInfo) {
		return empty, ErrEnvironment
	}
	stateInfo, err := checkedPath(cfg.StateDir, true)
	if err != nil || !environmentFilePrivate(stateInfo) {
		return empty, ErrEnvironment
	}
	if cfg.NextEnvironmentFile != nil {
		if cfg.NextEnvironmentFile.Path == cfg.EnvironmentFile.Path || !validDigest(cfg.NextEnvironmentFile.SHA256) || strings.EqualFold(cfg.NextEnvironmentFile.SHA256, cfg.EnvironmentFile.SHA256) {
			return empty, ErrEnvironment
		}
		if _, err := environmentFileHash(cfg.NextEnvironmentFile.Path, cfg.NextEnvironmentFile.SHA256); err != nil {
			return empty, ErrEnvironment
		}
	}
	configSHA, err := ConfigHash(cfg)
	if err != nil {
		return empty, ErrEnvironment
	}
	name := filepath.Base(cfg.EnvironmentFile.Path) + ".lanqin-" + configSHA[:16]
	return environmentPaths{live: cfg.EnvironmentFile.Path, old: filepath.Join(parent, name+".old"), staged: filepath.Join(parent, name+".new"), nextSaved: filepath.Join(parent, name+".next"), journal: filepath.Join(cfg.StateDir, "env.journal"), parent: parent, configSHA: configSHA}, nil
}

func applyEnvironment(ctx context.Context, cfg Config, ops environmentOps) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	paths, err := checkedEnvironmentPaths(cfg)
	if err != nil {
		return ErrEnvironment
	}
	if cfg.NextEnvironmentFile == nil {
		_, err := environmentFileHash(paths.live, cfg.EnvironmentFile.SHA256)
		return err
	}
	if _, err := readEnvironmentJournal(paths, cfg); err == nil {
		status, statusErr := EnvironmentStatus(cfg)
		if statusErr == nil && status == "next" {
			return nil
		}
		return ErrEnvironment
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrEnvironment
	}
	if !environmentAbsent(paths.old) || !environmentAbsent(paths.staged) || !environmentAbsent(paths.nextSaved) {
		return ErrEnvironment
	}
	original, err := environmentFileHash(paths.live, cfg.EnvironmentFile.SHA256)
	if err != nil {
		return ErrEnvironment
	}
	journal := environmentJournal{Version: 1, ConfigSHA256: paths.configSHA, OriginalPath: paths.live, OriginalSHA256: strings.ToLower(cfg.EnvironmentFile.SHA256), NextSHA256: strings.ToLower(cfg.NextEnvironmentFile.SHA256)}
	if err := createEnvironmentJournal(paths, journal, ops); err != nil {
		return err
	}
	if err := copyNextEnvironment(ctx, cfg, paths); err != nil {
		return err
	}
	if err := ops.syncDirectory(paths.parent); err != nil {
		return ErrEnvironment
	}
	if err := appendEnvironmentEvent(paths, cfg, "staged"); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	current, err := environmentFileHash(paths.live, cfg.EnvironmentFile.SHA256)
	if err != nil || !os.SameFile(original, current) {
		return ErrEnvironment
	}
	if err := ops.rename(paths.live, paths.old); err != nil {
		return ErrEnvironment
	}
	if err := ops.syncDirectory(paths.parent); err != nil {
		return ErrEnvironment
	}
	if err := appendEnvironmentEvent(paths, cfg, "original-saved"); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if _, err := environmentFileHash(paths.staged, cfg.NextEnvironmentFile.SHA256); err != nil {
		return ErrEnvironment
	}
	if err := ops.rename(paths.staged, paths.live); err != nil {
		return ErrEnvironment
	}
	if err := ops.syncDirectory(paths.parent); err != nil {
		return ErrEnvironment
	}
	return appendEnvironmentEvent(paths, cfg, "next-installed")
}

func rollbackEnvironment(ctx context.Context, cfg Config, ops environmentOps) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	paths, err := checkedEnvironmentPaths(cfg)
	if err != nil {
		return ErrEnvironment
	}
	if cfg.NextEnvironmentFile == nil {
		_, err := environmentFileHash(paths.live, cfg.EnvironmentFile.SHA256)
		return err
	}
	journal, journalErr := readEnvironmentJournal(paths, cfg)
	if errors.Is(journalErr, os.ErrNotExist) {
		status, statusErr := EnvironmentStatus(cfg)
		if statusErr == nil && status == "original" {
			return nil
		}
		return ErrEnvironment
	} else if journalErr != nil {
		return ErrEnvironment
	}
	live, old, saved := environmentKnownFile(paths.live, cfg), environmentKnownFile(paths.old, cfg), environmentKnownFile(paths.nextSaved, cfg)
	if saved != "absent" && saved != "next" {
		return ErrEnvironment
	}
	if live == "original" && old == "absent" {
		if err := ops.syncDirectory(paths.parent); err != nil {
			return ErrEnvironment
		}
		if len(journal.Events) > 0 && journal.Events[len(journal.Events)-1] == "original-restored" {
			return nil
		}
		return appendEnvironmentEvent(paths, cfg, "original-restored")
	}
	if old != "original" || live != "absent" && live != "next" || live == "next" && saved != "absent" {
		return ErrEnvironment
	}
	if err := appendEnvironmentEvent(paths, cfg, "rollback-intent"); err != nil {
		return err
	}
	if live == "next" {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := ops.rename(paths.live, paths.nextSaved); err != nil {
			return ErrEnvironment
		}
		if err := ops.syncDirectory(paths.parent); err != nil {
			return ErrEnvironment
		}
		if err := appendEnvironmentEvent(paths, cfg, "next-saved"); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if _, err := environmentFileHash(paths.old, cfg.EnvironmentFile.SHA256); err != nil || !environmentAbsent(paths.live) {
		return ErrEnvironment
	}
	if err := ops.rename(paths.old, paths.live); err != nil {
		return ErrEnvironment
	}
	if err := ops.syncDirectory(paths.parent); err != nil {
		return ErrEnvironment
	}
	return appendEnvironmentEvent(paths, cfg, "original-restored")
}

func environmentAbsent(name string) bool {
	_, err := os.Lstat(name)
	return errors.Is(err, os.ErrNotExist)
}

func environmentKnownFile(name string, cfg Config) string {
	if environmentAbsent(name) {
		return "absent"
	}
	if _, err := environmentFileHash(name, cfg.EnvironmentFile.SHA256); err == nil {
		return "original"
	}
	if cfg.NextEnvironmentFile != nil {
		if _, err := environmentFileHash(name, cfg.NextEnvironmentFile.SHA256); err == nil {
			return "next"
		}
	}
	return "unknown"
}

func environmentFileHash(name, want string) (os.FileInfo, error) {
	before, err := checkedPath(name, false)
	if err != nil || before.Size() < 0 || before.Size() > 8<<20 || !environmentFilePrivate(before) {
		return nil, ErrEnvironment
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, ErrEnvironment
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, ErrEnvironment
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, (8<<20)+1))
	after, statErr := file.Stat()
	current, pathErr := os.Lstat(name)
	if err != nil || statErr != nil || pathErr != nil || n != before.Size() || !os.SameFile(before, after) || !os.SameFile(before, current) || !before.ModTime().Equal(after.ModTime()) || before.Size() != after.Size() || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), want) {
		return nil, ErrEnvironment
	}
	return opened, nil
}

func copyNextEnvironment(ctx context.Context, cfg Config, paths environmentPaths) error {
	info, err := environmentFileHash(cfg.NextEnvironmentFile.Path, cfg.NextEnvironmentFile.SHA256)
	if err != nil {
		return ErrEnvironment
	}
	source, err := os.Open(cfg.NextEnvironmentFile.Path)
	if err != nil {
		return ErrEnvironment
	}
	defer source.Close()
	opened, err := source.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return ErrEnvironment
	}
	destination, err := os.OpenFile(paths.staged, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return ErrEnvironment
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(destination, hash), io.LimitReader(source, (8<<20)+1))
	syncErr, closeErr := destination.Sync(), destination.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil || n != info.Size() || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), cfg.NextEnvironmentFile.SHA256) {
		return ErrEnvironment
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	_, err = environmentFileHash(paths.staged, cfg.NextEnvironmentFile.SHA256)
	return err
}

func createEnvironmentJournal(paths environmentPaths, value environmentJournal, ops environmentOps) error {
	data, err := json.Marshal(value)
	if err != nil {
		return ErrEnvironment
	}
	file, err := os.OpenFile(paths.journal, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return ErrEnvironment
	}
	_, writeErr := file.Write(append(append(data, '\n'), []byte("\"intent\"\n")...))
	syncErr, closeErr := file.Sync(), file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil || ops.syncDirectory(filepath.Dir(paths.journal)) != nil {
		return ErrEnvironment
	}
	return nil
}

func readEnvironmentJournal(paths environmentPaths, cfg Config) (environmentJournal, error) {
	var result environmentJournal
	info, err := os.Lstat(paths.journal)
	if errors.Is(err, os.ErrNotExist) {
		return result, os.ErrNotExist
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 || !environmentFilePrivate(info) {
		return result, ErrEnvironment
	}
	file, err := os.Open(paths.journal)
	if err != nil {
		return result, ErrEnvironment
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return result, ErrEnvironment
	}
	data, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil || int64(len(data)) != info.Size() || len(data) == 0 || data[len(data)-1] != '\n' {
		return result, ErrEnvironment
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 16384)
	if !scanner.Scan() {
		return result, ErrEnvironment
	}
	decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || result.Version != 1 || result.ConfigSHA256 != paths.configSHA || result.OriginalPath != paths.live || !strings.EqualFold(result.OriginalSHA256, cfg.EnvironmentFile.SHA256) || cfg.NextEnvironmentFile == nil || !strings.EqualFold(result.NextSHA256, cfg.NextEnvironmentFile.SHA256) {
		return environmentJournal{}, ErrEnvironment
	}
	for scanner.Scan() {
		var event string
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			return environmentJournal{}, ErrEnvironment
		}
		switch event {
		case "intent", "staged", "original-saved", "next-installed", "rollback-intent", "next-saved", "original-restored":
		default:
			return environmentJournal{}, ErrEnvironment
		}
		result.Events = append(result.Events, event)
		if len(result.Events) > 128 {
			return environmentJournal{}, ErrEnvironment
		}
	}
	if scanner.Err() != nil || len(result.Events) == 0 || result.Events[0] != "intent" {
		return environmentJournal{}, ErrEnvironment
	}
	return result, nil
}

func appendEnvironmentEvent(paths environmentPaths, cfg Config, event string) error {
	if _, err := readEnvironmentJournal(paths, cfg); err != nil {
		return ErrEnvironment
	}
	before, err := os.Lstat(paths.journal)
	if err != nil {
		return ErrEnvironment
	}
	file, err := os.OpenFile(paths.journal, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return ErrEnvironment
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		file.Close()
		return ErrEnvironment
	}
	data, _ := json.Marshal(event)
	_, writeErr := file.Write(append(data, '\n'))
	syncErr, closeErr := file.Sync(), file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return ErrEnvironment
	}
	return nil
}
