package maintenance

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"lanqin-email-api/internal/backup/install"
)

const stateJournalName = "state.journal"

type State struct {
	Version                  int                 `json:"version"`
	OperationID              string              `json:"operationId"`
	ConfigSHA256             string              `json:"configSHA256"`
	Phase                    string              `json:"phase"`
	PendingAction            string              `json:"pendingAction,omitempty"`
	Sequence                 int                 `json:"sequence"`
	UpdatedAt                time.Time           `json:"updatedAt"`
	Transaction              install.JournalInfo `json:"transaction"`
	Committed                bool                `json:"committed"`
	NeedsRecovery            bool                `json:"needsRecovery"`
	ServicesStopped          bool                `json:"servicesStopped"`
	RollbackCompleted        bool                `json:"rollbackCompleted"`
	OriginalServicesRestored bool                `json:"originalServicesRestored"`
	OriginalDataUnchanged    bool                `json:"originalDataUnchanged"`
	LastErrorCode            string              `json:"lastErrorCode,omitempty"`
}

type journalRecord struct {
	Version        int    `json:"version"`
	PreviousSHA256 string `json:"previousSHA256"`
	State          State  `json:"state"`
}

type stateJournal struct {
	file      *os.File
	directory string
	previous  string
	count     int
	bytes     int64
}

func createStateJournal(cfg Config) (*stateJournal, error) {
	root, err := os.OpenRoot(cfg.StateDir)
	if err != nil {
		return nil, ErrState
	}
	defer root.Close()
	file, err := root.OpenFile(stateJournalName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, ErrState
	}
	if syncStateDirectory(cfg.StateDir) != nil {
		file.Close()
		return nil, ErrState
	}
	return &stateJournal{file: file, directory: cfg.StateDir}, nil
}

func (j *stateJournal) save(state *State) error {
	if j.count >= 128 {
		return ErrState
	}
	state.Sequence = j.count + 1
	state.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(journalRecord{Version: 1, PreviousSHA256: j.previous, State: *state})
	if err != nil || len(data) > 64<<10 {
		return ErrState
	}
	data = append(data, '\n')
	if int64(len(data)) > (2<<20)-j.bytes {
		return ErrState
	}
	n, err := j.file.Write(data)
	if err != nil || n != len(data) || j.file.Sync() != nil {
		return ErrState
	}
	digest := sha256.Sum256(data)
	j.previous = hex.EncodeToString(digest[:])
	j.count++
	j.bytes += int64(len(data))
	return nil
}

// ReadState reads only this operation's bounded private journal. Incomplete or
// modified records are errors, never permission to replay an interrupted step.
func ReadState(cfg Config) (State, error) {
	state, _, _, err := readState(cfg)
	return state, err
}

func readState(cfg Config) (State, string, int, error) {
	var empty State
	info, err := checkedPath(cfg.StateDir, true)
	if err != nil || !privateInfo(info) {
		return empty, "", 0, ErrState
	}
	name := filepath.Join(cfg.StateDir, stateJournalName)
	info, err = checkedPath(name, false)
	if err != nil || !privateInfo(info) || info.Size() < 1 || info.Size() > 2<<20 {
		return empty, "", 0, ErrState
	}
	file, err := os.Open(name)
	if err != nil {
		return empty, "", 0, ErrState
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return empty, "", 0, ErrState
	}
	reader := bufio.NewReader(io.LimitReader(file, (2<<20)+1))
	previous, count := "", 0
	var current State
	configHash, err := ConfigHash(cfg)
	if err != nil {
		return empty, "", 0, ErrState
	}
	for {
		line, err := reader.ReadBytes('\n')
		if err == io.EOF && len(line) == 0 {
			break
		}
		if err != nil || len(line) > 64<<10 || count >= 128 {
			return empty, "", 0, ErrState
		}
		var record journalRecord
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF || record.Version != 1 || record.PreviousSHA256 != previous || record.State.Sequence != count+1 || record.State.ConfigSHA256 != configHash || record.State.Version != 1 || !validDigest(record.State.ConfigSHA256) || !validOperationID(record.State.OperationID) || record.State.UpdatedAt.IsZero() || !validStatePhase(record.State.Phase) {
			return empty, "", 0, ErrState
		}
		if count > 0 && (current.OperationID != record.State.OperationID || current.Committed && !record.State.Committed) {
			return empty, "", 0, ErrState
		}
		current = record.State
		digest := sha256.Sum256(line)
		previous = hex.EncodeToString(digest[:])
		count++
	}
	if count == 0 {
		return empty, "", 0, ErrState
	}
	final, err := file.Stat()
	if err != nil || final.Size() != info.Size() || !final.ModTime().Equal(info.ModTime()) {
		return empty, "", 0, ErrState
	}
	return current, previous, count, nil
}

func openStateJournal(cfg Config) (*stateJournal, State, error) {
	state, previous, count, err := readState(cfg)
	if err != nil {
		return nil, state, err
	}
	name := filepath.Join(cfg.StateDir, stateJournalName)
	before, err := checkedPath(name, false)
	if err != nil {
		return nil, state, ErrState
	}
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return nil, state, ErrState
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		file.Close()
		return nil, state, ErrState
	}
	return &stateJournal{file: file, directory: cfg.StateDir, previous: previous, count: count, bytes: opened.Size()}, state, nil
}

func validOperationID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16
}
func validStatePhase(phase string) bool {
	switch phase {
	case "preflight", "stopping", "stopped", "installing", "installed", "validated", "committed", "starting", "healthy", "failed":
		return true
	}
	return false
}
