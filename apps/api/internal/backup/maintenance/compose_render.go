package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
)

type composeRenderPin struct {
	Version           int    `json:"version"`
	ConfigSHA256      string `json:"configSHA256"`
	EnvironmentSHA256 string `json:"environmentSHA256"`
	RenderSHA256      string `json:"renderSHA256"`
}

func composeRenderPinName(environment string) string {
	return "compose-render-" + environment + ".json"
}

func readComposeRenderPin(cfg Config, environment string) (composeRenderPin, error) {
	var pin composeRenderPin
	if !validDigest(environment) {
		return pin, ErrConfig
	}
	root, err := os.OpenRoot(cfg.StateDir)
	if err != nil {
		return pin, ErrConfig
	}
	defer root.Close()
	name := composeRenderPinName(environment)
	info, err := root.Lstat(name)
	if err != nil {
		return pin, err
	}
	if !info.Mode().IsRegular() || !privateInfo(info) || info.Size() < 1 || info.Size() > 4096 {
		return pin, ErrConfig
	}
	file, err := root.Open(name)
	if err != nil {
		return pin, ErrConfig
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return pin, ErrConfig
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || int64(len(data)) != info.Size() {
		return pin, ErrConfig
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	hash, err := ConfigHash(cfg)
	if err != nil || decoder.Decode(&pin) != nil || decoder.Decode(new(any)) != io.EOF || pin.Version != 1 || pin.ConfigSHA256 != hash || pin.EnvironmentSHA256 != environment || !validDigest(pin.RenderSHA256) {
		return pin, ErrConfig
	}
	return pin, nil
}

func (c *ComposeController) authorizeInitialRenderPins(cfg Config) {
	state, err := ReadState(cfg)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.renderPinOperation = ""
	if err == nil && state.Sequence == 1 && state.Phase == "preflight" && state.PendingAction == "preflight" && !state.Committed && !state.NeedsRecovery {
		c.renderPinOperation = state.OperationID
	}
}

func (c *ComposeController) mayCreateComposeRenderPin(cfg Config, environment string) bool {
	c.mu.Lock()
	operation := c.renderPinOperation
	c.mu.Unlock()
	if operation == "" {
		return false
	}
	state, err := ReadState(cfg)
	if err != nil || state.OperationID != operation || state.Committed || state.NeedsRecovery {
		return false
	}
	if environment == strings.ToLower(cfg.EnvironmentFile.SHA256) {
		if state.Sequence != 1 || state.Phase != "preflight" || state.PendingAction != "preflight" {
			return false
		}
		// Once original-container authority exists, a missing old slot is a
		// recovery problem, even if the first preflight state was last flushed.
		_, err := readComposeOriginal(cfg)
		return errors.Is(err, os.ErrNotExist)
	}
	if cfg.NextEnvironmentFile == nil || environment != strings.ToLower(cfg.NextEnvironmentFile.SHA256) || state.Phase != "installing" && state.Phase != "installed" {
		return false
	}
	if _, err := readComposeRenderPin(cfg, strings.ToLower(cfg.EnvironmentFile.SHA256)); err != nil {
		return false
	}
	_, err = readComposeOriginal(cfg)
	return err == nil
}

func (c *ComposeController) verifyComposeRenderPin(ctx context.Context, cfg Config, environment, digest string) error {
	if ctx.Err() != nil || !validDigest(environment) || !validDigest(digest) {
		return ErrConfig
	}
	if pin, err := readComposeRenderPin(cfg, environment); err == nil {
		if pin.RenderSHA256 != digest {
			return ErrConfig
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrConfig
	}
	// Only the first operation's live controller may establish old/next slots.
	// A cold Recover never turns a missing pin into authority for new code/spec.
	if !c.mayCreateComposeRenderPin(cfg, environment) {
		return ErrConfig
	}
	hash, err := ConfigHash(cfg)
	if err != nil {
		return ErrConfig
	}
	data, err := json.Marshal(composeRenderPin{Version: 1, ConfigSHA256: hash, EnvironmentSHA256: environment, RenderSHA256: digest})
	if err != nil || len(data) > 4096 {
		return ErrConfig
	}
	root, err := os.OpenRoot(cfg.StateDir)
	if err != nil {
		return ErrConfig
	}
	defer root.Close()
	file, err := root.OpenFile(composeRenderPinName(environment), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return ErrConfig
	}
	_, writeErr := file.Write(append(data, '\n'))
	syncErr, closeErr := file.Sync(), file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil || syncStateDirectory(cfg.StateDir) != nil {
		return ErrConfig
	}
	return nil
}
