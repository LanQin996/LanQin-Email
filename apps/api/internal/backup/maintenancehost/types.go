// Package maintenancehost is a finite, peer-authenticated local maintenance
// interface. Web requests select preapproved plans, never paths or commands.
package maintenancehost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode"

	"lanqin-email-api/internal/backup/maintenance"
)

var (
	ErrUnavailable = errors.New("maintenance host unavailable")
	ErrConflict    = errors.New("maintenance host request conflicts with existing operation")
	ErrNotFound    = errors.New("maintenance host request not found")
	planPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	hexID          = regexp.MustCompile(`^[a-f0-9]{32}$`)
	hexSHA         = regexp.MustCompile(`^[a-f0-9]{64}$`)
	webJob         = regexp.MustCompile(`^bkj_[a-f0-9]{32}$`)
)

type Plan struct {
	ID              string `json:"id"`
	TemplateSHA256  string `json:"templateSHA256"`
	Driver          string `json:"driver"`
	QueueQuarantine bool   `json:"queueQuarantine"`
}

type Request struct {
	ID             string `json:"id"`
	PlanID         string `json:"planId"`
	TemplateSHA256 string `json:"templateSHA256"`
	WebJob         string `json:"webJob"`
	ReportSHA256   string `json:"reportSHA256"`
	Actor          string `json:"actor"`
}

type Status struct {
	Version   int       `json:"version"`
	ID        string    `json:"id"`
	PlanID    string    `json:"planId"`
	WebJob    string    `json:"webJob"`
	State     string    `json:"state"`
	Phase     string    `json:"phase"`
	ErrorCode string    `json:"errorCode,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Progress  *Progress `json:"progress,omitempty"`
}

// Progress exposes only finite stage names and timestamps, never executor data.
type Progress struct {
	Step      string    `json:"step"`
	StartedAt time.Time `json:"startedAt"`
}

func ValidProgress(status Status) bool {
	if status.Progress == nil {
		return true // Legacy records and helpers remain readable.
	}
	p := status.Progress
	if p.StartedAt.Before(status.CreatedAt) || p.StartedAt.After(status.UpdatedAt) {
		return false
	}
	switch p.Step {
	case "queued", "checking-source", "copying", "checking-copy", "importing", "preflight", "stopping", "installing", "validating", "committing", "starting", "health-check", "rolling-back", "recovering-original", "completed":
		return true
	}
	return false
}

type Source struct {
	Driver       string `json:"driver"`
	DBPath       string `json:"dbPath,omitempty"`
	DSNEnv       string `json:"dsnEnv,omitempty"`
	RestoreRoot  string `json:"restoreRoot"`
	RecordedRoot string `json:"recordedRoot,omitempty"`
}

type ApprovedPlan struct {
	ID       string                 `json:"id"`
	Template maintenance.PathDigest `json:"template"`
	Source   Source                 `json:"source"`
}

type Config struct {
	Version  int            `json:"version"`
	Socket   string         `json:"socket"`
	StateDir string         `json:"stateDir"`
	APIUID   int            `json:"apiUid"`
	Plans    []ApprovedPlan `json:"plans"`
}

type Outcome struct{ State, ErrorCode string }
type Executor func(context.Context, Request, ApprovedPlan, func(string) error) (Outcome, error)

func ValidRequest(r Request) bool {
	return hexID.MatchString(r.ID) && planPattern.MatchString(r.PlanID) && hexSHA.MatchString(r.TemplateSHA256) && webJob.MatchString(r.WebJob) && hexSHA.MatchString(r.ReportSHA256) && len(r.Actor) > 0 && len(r.Actor) <= 128 && regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(r.Actor)
}

func DecodeConfig(reader io.Reader) (Config, error) {
	var cfg Config
	data, err := io.ReadAll(io.LimitReader(reader, (64<<10)+1))
	var explicit struct {
		APIUID *int `json:"apiUid"`
	}
	if err != nil || len(data) > 64<<10 || json.Unmarshal(data, &explicit) != nil || explicit.APIUID == nil || decode(bytes.NewReader(data), &cfg, 64<<10) != nil || cfg.Version != 1 || cfg.APIUID < 0 || int64(cfg.APIUID) > 2147483647 || len(cfg.Plans) < 1 || len(cfg.Plans) > 16 || !absolute(cfg.Socket) || runtime.GOOS == "linux" && len(cfg.Socket) > 100 || !absolute(cfg.StateDir) || filepath.Dir(cfg.Socket) == cfg.StateDir || !trustedPath(cfg.StateDir, true, true) || !trustedPath(filepath.Dir(cfg.Socket), true, false) {
		return Config{}, ErrUnavailable
	}
	seen := map[string]bool{}
	for _, p := range cfg.Plans {
		if seen[p.ID] || !planPattern.MatchString(p.ID) || !hexSHA.MatchString(p.Template.SHA256) || !trustedPath(p.Template.Path, false, true) || !absolute(p.Source.RestoreRoot) || p.Source.Driver != "sqlite" && p.Source.Driver != "postgres" && p.Source.Driver != "mysql" {
			return Config{}, ErrUnavailable
		}
		seen[p.ID] = true
		if p.Source.Driver == "sqlite" && (!absolute(p.Source.DBPath) || p.Source.DSNEnv != "") || p.Source.Driver != "sqlite" && (p.Source.DBPath != "" || !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`).MatchString(p.Source.DSNEnv)) {
			return Config{}, ErrUnavailable
		}
		template, err := LoadTemplate(p)
		if err != nil || overlap(cfg.StateDir, p.Source.RestoreRoot) || overlap(cfg.StateDir, p.Template.Path) {
			return Config{}, ErrUnavailable
		}
		for _, target := range []string{template.Targets.Data, template.Targets.Mail, template.Targets.DKIM} {
			if overlap(cfg.StateDir, target) || overlap(filepath.Dir(cfg.Socket), target) || overlap(p.Source.RestoreRoot, target) {
				return Config{}, ErrUnavailable
			}
		}
	}
	return cfg, nil
}

func LoadTemplate(plan ApprovedPlan) (maintenance.Config, error) {
	var cfg maintenance.Config
	if !trustedPath(plan.Template.Path, false, true) {
		return cfg, ErrUnavailable
	}
	data, err := readTrusted(plan.Template.Path, 64<<10)
	if err != nil {
		return cfg, ErrUnavailable
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != plan.Template.SHA256 {
		return cfg, ErrUnavailable
	}
	cfg, err = maintenance.DecodeRecoveryConfig(bytes.NewReader(data))
	if err != nil || cfg.Driver != plan.Source.Driver || cfg.WebJob != "" || cfg.WebStateDB != "" || cfg.WebStateDSNEnv != "" || cfg.WebRestoreRoot != "" || cfg.WebRecordedRoot != "" {
		return maintenance.Config{}, ErrUnavailable
	}
	return cfg, nil
}

func readTrusted(name string, limit int64) ([]byte, error) {
	if !trustedPath(name, false, true) {
		return nil, ErrUnavailable
	}
	before, err := os.Lstat(name)
	if err != nil || before.Size() < 1 || before.Size() > limit {
		return nil, ErrUnavailable
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, ErrUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	after, se := f.Stat()
	if err != nil || se != nil || int64(len(data)) != before.Size() || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return nil, ErrUnavailable
	}
	return data, nil
}

func absolute(name string) bool {
	return name != "" && filepath.IsAbs(name) && filepath.Clean(name) == name && strings.IndexFunc(name, unicode.IsControl) < 0
}

func overlap(a, b string) bool {
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		relative, err := filepath.Rel(pair[0], pair[1])
		if err == nil && (relative == "." || filepath.IsLocal(relative)) {
			return true
		}
	}
	return false
}

func validRecord(r record) bool {
	s := r.Status
	switch r.InterruptedPhase {
	case "", "queued", "preparing", "importing", "maintenance":
	default:
		return false
	}
	if !ValidRequest(r.Request) || s.Version != 1 || s.ID != r.Request.ID || s.PlanID != r.Request.PlanID || s.WebJob != r.Request.WebJob || s.CreatedAt.IsZero() || s.UpdatedAt.Before(s.CreatedAt) || !ValidProgress(s) {
		return false
	}
	switch s.State {
	case "queued":
		if s.Phase != "queued" {
			return false
		}
	case "preparing":
		if s.Phase != "preparing" && s.Phase != "importing" {
			return false
		}
	case "maintaining":
		if s.Phase != "maintenance" {
			return false
		}
	case "succeeded", "failed":
		if s.Phase != "completed" {
			return false
		}
	case "needsRecovery":
		if s.Phase != "completed" && s.Phase != "interrupted" {
			return false
		}
	default:
		return false
	}
	switch s.ErrorCode {
	case "", "unavailable", "source_unavailable", "preparation_failed", "import_failed", "maintenance_failed", "interrupted":
	default:
		return false
	}
	if (s.State == "queued" || s.State == "preparing" || s.State == "maintaining" || s.State == "succeeded") && s.ErrorCode != "" {
		return false
	}
	return true
}
func decode(r io.Reader, out any, limit int64) error {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil || int64(len(data)) > limit {
		return ErrUnavailable
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		return ErrUnavailable
	}
	return nil
}
