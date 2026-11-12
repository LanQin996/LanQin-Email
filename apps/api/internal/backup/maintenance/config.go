// Package maintenance coordinates explicit deployment maintenance with a fixed
// controller interface and deployment-owned, pinned whole-environment
// transitions. It never interprets shell hooks or guesses DSN substitutions,
// and successful file copying alone never permits starting mail writers.
package maintenance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode"

	"filippo.io/age"
	"lanqin-email-api/internal/backup/install"
)

type PathDigest struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type RuntimeDatabaseBinding struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	User     string `json:"user"`
}

type PreservationConfig struct {
	Recipient        string       `json:"recipient"`
	IdentityEnv      string       `json:"identityEnv"`
	Assets           []PathDigest `json:"assets,omitempty"`
	QuarantineQueues bool         `json:"quarantineQueues,omitempty"`
}

type Config struct {
	Version              int                     `json:"version"`
	Mode                 string                  `json:"mode"`
	DockerPath           string                  `json:"dockerPath"`
	ProjectDirectory     string                  `json:"projectDirectory"`
	ProjectName          string                  `json:"projectName"`
	ComposeFiles         []PathDigest            `json:"composeFiles"`
	EnvironmentFile      PathDigest              `json:"environmentFile"`
	NextEnvironmentFile  *PathDigest             `json:"nextEnvironmentFile,omitempty"`
	RuntimeDatabase      *RuntimeDatabaseBinding `json:"runtimeDatabase,omitempty"`
	StateDir             string                  `json:"stateDir"`
	Targets              install.Targets         `json:"targets"`
	Owners               install.Owners          `json:"owners"`
	RuntimeDataRoot      string                  `json:"runtimeDataRoot"`
	RuntimeMailRoot      string                  `json:"runtimeMailRoot"`
	RuntimeDKIMRoot      string                  `json:"runtimeDKIMRoot"`
	Workspace            string                  `json:"workspace"`
	Staged               string                  `json:"staged,omitempty"`
	ExpectedReportSHA256 string                  `json:"expectedReportSHA256,omitempty"`
	WebJob               string                  `json:"webJob,omitempty"`
	WebStateDB           string                  `json:"webStateDb,omitempty"`
	WebStateDSNEnv       string                  `json:"webStateDsnEnv,omitempty"`
	WebRestoreRoot       string                  `json:"webRestoreRoot,omitempty"`
	WebRecordedRoot      string                  `json:"webRecordedRoot,omitempty"`
	Driver               string                  `json:"driver"`
	SQLiteGroupRead      bool                    `json:"sqliteGroupRead,omitempty"`
	TargetDSNEnv         string                  `json:"targetDsnEnv,omitempty"`
	StepTimeoutSeconds   int                     `json:"stepTimeoutSeconds,omitempty"`
	HealthTimeoutSeconds int                     `json:"healthTimeoutSeconds,omitempty"`
	Preservation         *PreservationConfig     `json:"preservation,omitempty"`
}

var (
	ErrConfig             = errors.New("invalid or changed maintenance configuration")
	ErrState              = errors.New("maintenance state unavailable or requires explicit recovery")
	ErrLocked             = errors.New("another maintenance operation holds the deployment lock")
	ErrRecovery           = errors.New("maintenance requires operator recovery; retain state, transaction journals and old/new trees")
	ErrConfirmed          = errors.New("maintenance recovery requires explicit confirmation")
	ErrInstallUnchanged   = errors.New("maintenance preparation failed before any installation transaction began")
	ErrPostfixQueueUnsafe = errors.New("Postfix queues were not verified empty; keep original containers and queue data")
	projectPattern        = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	envPattern            = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	jobPattern            = regexp.MustCompile(`^bkj_[a-f0-9]{32}$`)
)

// MarshalJSON emits lower-camel nested target/owner fields while retaining the
// install package's existing concrete types for the CLI action adapters.
func (c Config) MarshalJSON() ([]byte, error) {
	type rawConfig Config
	type owner struct {
		UID int `json:"uid"`
		GID int `json:"gid"`
	}
	type roots struct {
		Data string `json:"data"`
		Mail string `json:"mail"`
		DKIM string `json:"dkim"`
	}
	type owners struct {
		Data owner `json:"data"`
		Mail owner `json:"mail"`
		DKIM owner `json:"dkim"`
	}
	return json.Marshal(struct {
		rawConfig
		Targets roots  `json:"targets"`
		Owners  owners `json:"owners"`
	}{rawConfig: rawConfig(c), Targets: roots{c.Targets.Data, c.Targets.Mail, c.Targets.DKIM}, Owners: owners{owner{c.Owners.Data.UID, c.Owners.Data.GID}, owner{c.Owners.Mail.UID, c.Owners.Mail.GID}, owner{c.Owners.DKIM.UID, c.Owners.DKIM.GID}}})
}

func DecodeConfig(reader io.Reader) (Config, error) {
	return decodeConfig(reader, "run")
}

func DecodeRecoveryConfig(reader io.Reader) (Config, error) { return decodeConfig(reader, "recover") }

// DecodeStatusConfig performs the same strict bounded decoding and explicit
// owner-field checks, without reading source files, live roots, deployment
// files or DSN values. ReadState still binds the exact config to its journal.
func DecodeStatusConfig(reader io.Reader) (Config, error) { return decodeConfig(reader, "status") }

func decodeConfig(reader io.Reader, mode string) (Config, error) {
	var cfg Config
	data, err := io.ReadAll(io.LimitReader(reader, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return cfg, ErrConfig
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cfg) != nil || decoder.Decode(new(any)) != io.EOF {
		return Config{}, ErrConfig
	}
	// UID/GID zero is legitimate, so require their explicit presence rather
	// than interpreting an omitted integer as authorization to install as root.
	var present struct {
		Owners struct {
			Data struct {
				UID *int `json:"uid"`
				GID *int `json:"gid"`
			} `json:"data"`
			Mail struct {
				UID *int `json:"uid"`
				GID *int `json:"gid"`
			} `json:"mail"`
			DKIM struct {
				UID *int `json:"uid"`
				GID *int `json:"gid"`
			} `json:"dkim"`
		} `json:"owners"`
	}
	if json.Unmarshal(data, &present) != nil || present.Owners.Data.UID == nil || present.Owners.Data.GID == nil || present.Owners.Mail.UID == nil || present.Owners.Mail.GID == nil || present.Owners.DKIM.UID == nil || present.Owners.DKIM.GID == nil {
		return Config{}, ErrConfig
	}
	var validErr error
	if mode == "status" {
		validErr = validateStatusConfig(cfg)
	} else if mode == "recover" {
		validErr = ValidateRecoveryConfig(cfg)
	} else {
		validErr = ValidateConfig(cfg)
	}
	if validErr != nil {
		return Config{}, ErrConfig
	}
	return cfg, nil
}

func validateStatusConfig(cfg Config) error {
	if cfg.SQLiteGroupRead && (cfg.Driver != "sqlite" || cfg.Owners.Data.GID <= 0) {
		return ErrConfig
	}
	if cfg.Version != 1 || cfg.Mode != "all-in-one" && cfg.Mode != "stack" || !projectPattern.MatchString(cfg.ProjectName) || cfg.Driver != "sqlite" && cfg.Driver != "mysql" && cfg.Driver != "postgres" || len(cfg.ComposeFiles) < 1 || len(cfg.ComposeFiles) > 8 || cfg.StepTimeoutSeconds < 0 || cfg.StepTimeoutSeconds > 300 || cfg.HealthTimeoutSeconds < 0 || cfg.HealthTimeoutSeconds > 900 {
		return ErrConfig
	}
	paths := []string{cfg.DockerPath, cfg.ProjectDirectory, cfg.StateDir, cfg.Workspace, cfg.Targets.Data, cfg.Targets.Mail, cfg.Targets.DKIM, cfg.EnvironmentFile.Path}
	for _, selected := range cfg.ComposeFiles {
		if !validDigest(selected.SHA256) {
			return ErrConfig
		}
		paths = append(paths, selected.Path)
	}
	if !validDigest(cfg.EnvironmentFile.SHA256) {
		return ErrConfig
	}
	if cfg.NextEnvironmentFile != nil {
		if !validDigest(cfg.NextEnvironmentFile.SHA256) {
			return ErrConfig
		}
		paths = append(paths, cfg.NextEnvironmentFile.Path)
	}
	if err := validatePreservationConfig(cfg, false); err != nil {
		return err
	}
	for _, source := range []string{cfg.Staged, cfg.WebRestoreRoot, cfg.WebStateDB} {
		if source != "" {
			paths = append(paths, source)
		}
	}
	for _, name := range paths {
		if !configPath(name) {
			return ErrConfig
		}
	}
	for _, root := range []string{cfg.RuntimeDataRoot, cfg.RuntimeMailRoot, cfg.RuntimeDKIMRoot} {
		if !runtimeRoot(root) {
			return ErrConfig
		}
	}
	if cfg.ExpectedReportSHA256 != "" && !validDigest(cfg.ExpectedReportSHA256) || cfg.WebJob != "" && !jobPattern.MatchString(cfg.WebJob) || cfg.WebRecordedRoot != "" && !runtimeRoot(cfg.WebRecordedRoot) {
		return ErrConfig
	}
	for _, reference := range []string{cfg.TargetDSNEnv, cfg.WebStateDSNEnv} {
		if reference != "" && !envPattern.MatchString(reference) {
			return ErrConfig
		}
	}
	for _, owner := range []install.Owner{cfg.Owners.Data, cfg.Owners.Mail, cfg.Owners.DKIM} {
		if owner.UID < 0 || owner.GID < 0 || int64(owner.UID) > 2147483647 || int64(owner.GID) > 2147483647 {
			return ErrConfig
		}
	}
	info, err := checkedPath(cfg.StateDir, true)
	if err != nil || !privateInfo(info) || !trustedParents(cfg.StateDir) {
		return ErrConfig
	}
	return nil
}

func ValidateConfig(cfg Config) error {
	return validateConfig(cfg, true)
}

// ValidateRecoveryConfig keeps the fixed configuration/pins authoritative, but
// does not require source inputs or live roots that an interrupted rename may
// have moved. Lower-level rollback still authenticates the actual tree markers.
func ValidateRecoveryConfig(cfg Config) error { return validateConfig(cfg, false) }

func validateConfig(cfg Config, runInputs bool) error {
	if cfg.SQLiteGroupRead && (cfg.Driver != "sqlite" || cfg.Owners.Data.GID <= 0) {
		return ErrConfig
	}
	if cfg.Version != 1 || cfg.Mode != "all-in-one" && cfg.Mode != "stack" || !projectPattern.MatchString(cfg.ProjectName) || cfg.Driver != "sqlite" && cfg.Driver != "mysql" && cfg.Driver != "postgres" || len(cfg.ComposeFiles) < 1 || len(cfg.ComposeFiles) > 8 || cfg.StepTimeoutSeconds < 0 || cfg.StepTimeoutSeconds > 300 || cfg.HealthTimeoutSeconds < 0 || cfg.HealthTimeoutSeconds > 900 {
		return ErrConfig
	}
	if cfg.Driver == "sqlite" && cfg.TargetDSNEnv != "" || cfg.Driver != "sqlite" && !envPattern.MatchString(cfg.TargetDSNEnv) {
		return ErrConfig
	}
	if cfg.Driver == "sqlite" {
		if cfg.NextEnvironmentFile != nil || cfg.RuntimeDatabase != nil {
			return ErrConfig
		}
	} else {
		if cfg.NextEnvironmentFile == nil || cfg.RuntimeDatabase == nil || cfg.NextEnvironmentFile.Path == cfg.EnvironmentFile.Path {
			return ErrConfig
		}
		binding := cfg.RuntimeDatabase
		if binding.Host == "" || len(binding.Host) > 253 || binding.Port < 1 || binding.Port > 65535 || binding.Database == "" || len(binding.Database) > 128 || binding.User == "" || len(binding.User) > 128 || strings.ContainsAny(binding.Host, "/\\@?#") || strings.IndexFunc(binding.Host+binding.Database+binding.User, unicode.IsControl) >= 0 {
			return ErrConfig
		}
	}
	for _, owner := range []install.Owner{cfg.Owners.Data, cfg.Owners.Mail, cfg.Owners.DKIM} {
		if owner.UID < 0 || owner.GID < 0 || int64(owner.UID) > 2147483647 || int64(owner.GID) > 2147483647 {
			return ErrConfig
		}
	}
	for _, root := range []string{cfg.RuntimeDataRoot, cfg.RuntimeMailRoot, cfg.RuntimeDKIMRoot} {
		if !runtimeRoot(root) {
			return ErrConfig
		}
	}
	if overlap(cfg.RuntimeDataRoot, cfg.RuntimeMailRoot) || overlap(cfg.RuntimeDataRoot, cfg.RuntimeDKIMRoot) || overlap(cfg.RuntimeMailRoot, cfg.RuntimeDKIMRoot) {
		return ErrConfig
	}
	dockerInfo, dockerErr := checkedPath(cfg.DockerPath, false)
	if dockerErr != nil || !trustedMaintenanceInfo(dockerInfo) || !trustedParents(cfg.DockerPath) {
		return ErrConfig
	}
	projectInfo, err := checkedPath(cfg.ProjectDirectory, true)
	if err != nil || !trustedMaintenanceInfo(projectInfo) || !trustedParents(cfg.ProjectDirectory) {
		return ErrConfig
	}
	for _, directory := range []string{cfg.StateDir, cfg.Workspace} {
		info, err := checkedPath(directory, true)
		if err != nil || !privateInfo(info) || !trustedParents(directory) {
			return ErrConfig
		}
	}
	targets := []string{cfg.Targets.Data, cfg.Targets.Mail, cfg.Targets.DKIM}
	for i, directory := range targets {
		if !configPath(directory) {
			return ErrConfig
		}
		if _, err := checkedPath(directory, true); err != nil && (runInputs || !missingTarget(directory)) {
			return ErrConfig
		}
		if overlap(directory, cfg.StateDir) || overlap(directory, cfg.Workspace) || containsPath(directory, cfg.ProjectDirectory) {
			return ErrConfig
		}
		for _, source := range []string{cfg.Staged, cfg.WebRestoreRoot} {
			if source != "" && overlap(directory, source) {
				return ErrConfig
			}
		}
		for j := 0; j < i; j++ {
			if overlap(directory, targets[j]) {
				return ErrConfig
			}
		}
	}
	if overlap(cfg.StateDir, cfg.Workspace) {
		return ErrConfig
	}
	web := cfg.WebJob != "" || cfg.WebStateDB != "" || cfg.WebStateDSNEnv != "" || cfg.WebRestoreRoot != "" || cfg.WebRecordedRoot != ""
	if web {
		if cfg.Staged != "" || cfg.ExpectedReportSHA256 != "" || !jobPattern.MatchString(cfg.WebJob) || cfg.WebRestoreRoot == "" {
			return ErrConfig
		}
		if !configPath(cfg.WebRestoreRoot) {
			return ErrConfig
		}
		if _, err := checkedPath(cfg.WebRestoreRoot, true); runInputs && err != nil {
			return ErrConfig
		}
		if cfg.WebRecordedRoot != "" && !runtimeRoot(cfg.WebRecordedRoot) {
			return ErrConfig
		}
		if cfg.Driver == "sqlite" {
			if cfg.WebStateDSNEnv != "" {
				return ErrConfig
			}
			if !configPath(cfg.WebStateDB) {
				return ErrConfig
			}
			if _, err := checkedPath(cfg.WebStateDB, false); runInputs && err != nil {
				return ErrConfig
			}
		} else if cfg.WebStateDB != "" || !envPattern.MatchString(cfg.WebStateDSNEnv) || cfg.WebStateDSNEnv == cfg.TargetDSNEnv {
			return ErrConfig
		}
	} else {
		if !configPath(cfg.Staged) || !validDigest(cfg.ExpectedReportSHA256) {
			return ErrConfig
		}
		if _, err := checkedPath(cfg.Staged, true); runInputs && err != nil {
			return ErrConfig
		}
	}
	if err := validatePreservationConfig(cfg, true); err != nil {
		return err
	}
	return verifyPinnedFiles(cfg, !runInputs)
}

func VerifyPinnedFiles(cfg Config) error {
	return verifyPinnedFiles(cfg, false)
}

func verifyPinnedFiles(cfg Config, allowIncompleteEnvironment bool) error {
	if err := validatePreservationConfig(cfg, true); err != nil {
		return err
	}
	seen := map[string]bool{}
	selectedFiles := append(append([]PathDigest{}, cfg.ComposeFiles...), cfg.EnvironmentFile)
	if cfg.NextEnvironmentFile != nil {
		selectedFiles = append(selectedFiles, *cfg.NextEnvironmentFile)
	}
	if cfg.Preservation != nil {
		for _, asset := range cfg.Preservation.Assets {
			selectedFiles = append(selectedFiles, asset)
		}
	}
	for _, selected := range selectedFiles {
		if !validDigest(selected.SHA256) {
			return ErrConfig
		}
		for _, target := range []string{cfg.Targets.Data, cfg.Targets.Mail, cfg.Targets.DKIM} {
			if containsPath(target, selected.Path) {
				return ErrConfig
			}
		}
		info, err := checkedPath(selected.Path, false)
		if err != nil && allowIncompleteEnvironment && selected.Path == cfg.EnvironmentFile.Path && knownIncompleteEnvironment(cfg) {
			continue
		}
		if err != nil || info.Size() > 8<<20 || !trustedMaintenanceInfo(info) || !trustedParents(selected.Path) || seen[configPathKey(selected.Path)] {
			return ErrConfig
		}
		seen[configPathKey(selected.Path)] = true
		file, err := os.Open(selected.Path)
		if err != nil {
			return ErrConfig
		}
		opened, statErr := file.Stat()
		hash := sha256.New()
		var n int64
		var copyErr error
		containsRecoveryIdentity := false
		// Compose and env pins are read before any Docker invocation. When
		// preservation is enabled, reject accidentally embedded age recovery
		// identities here as well as in explicitly selected additional assets.
		if cfg.Preservation != nil {
			content, readErr := io.ReadAll(io.LimitReader(file, (8<<20)+1))
			n, copyErr = int64(len(content)), readErr
			containsRecoveryIdentity = bytes.Contains(content, []byte("AGE-SECRET-KEY-"))
			hash.Write(content)
			clear(content)
		} else {
			n, copyErr = io.Copy(hash, io.LimitReader(file, (8<<20)+1))
		}
		closeErr := file.Close()
		actual := hex.EncodeToString(hash.Sum(nil))
		matches := strings.EqualFold(actual, selected.SHA256)
		if selected.Path == cfg.EnvironmentFile.Path && cfg.NextEnvironmentFile != nil {
			matches = matches || strings.EqualFold(actual, cfg.NextEnvironmentFile.SHA256)
		}
		if statErr != nil || copyErr != nil || closeErr != nil || !os.SameFile(info, opened) || n != info.Size() || !matches || containsRecoveryIdentity {
			return ErrConfig
		}
	}
	return nil
}

// Preservation selects only explicit deployment assets. Recovery private keys
// are supplied separately at execution time and never read during decoding.
func validatePreservationConfig(cfg Config, checkFiles bool) error {
	if cfg.Preservation == nil {
		return nil
	}
	preservation := cfg.Preservation
	if preservation.Recipient == "" || strings.TrimSpace(preservation.Recipient) != preservation.Recipient || !envPattern.MatchString(preservation.IdentityEnv) || len(preservation.Assets) > 24 {
		return ErrConfig
	}
	if _, err := age.ParseX25519Recipient(preservation.Recipient); err != nil {
		return ErrConfig
	}
	seen := map[string]bool{}
	var identities []os.FileInfo
	reserved := append(append([]PathDigest{}, cfg.ComposeFiles...), cfg.EnvironmentFile)
	if cfg.NextEnvironmentFile != nil {
		reserved = append(reserved, *cfg.NextEnvironmentFile)
	}
	for _, selected := range reserved {
		seen[configPathKey(selected.Path)] = true
		if checkFiles {
			if info, err := checkedPath(selected.Path, false); err == nil {
				identities = append(identities, info)
			}
		}
	}
	for _, asset := range preservation.Assets {
		key := configPathKey(asset.Path)
		if !configPath(asset.Path) || !validDigest(asset.SHA256) || seen[key] {
			return ErrConfig
		}
		seen[key] = true
		for _, excluded := range []string{cfg.Targets.Data, cfg.Targets.Mail, cfg.Targets.DKIM, cfg.Workspace, cfg.StateDir, cfg.Staged, cfg.WebRestoreRoot} {
			if overlap(asset.Path, excluded) {
				return ErrConfig
			}
		}
		if !checkFiles {
			continue
		}
		info, err := checkedPath(asset.Path, false)
		if err != nil || info.Size() > 8<<20 || !trustedMaintenanceInfo(info) || !trustedParents(asset.Path) {
			return ErrConfig
		}
		for _, previous := range identities {
			if os.SameFile(previous, info) {
				return ErrConfig
			}
		}
		identities = append(identities, info)
	}
	return nil
}

func configPathKey(name string) string {
	name = filepath.Clean(name)
	if runtime.GOOS == "windows" {
		return strings.ToLower(name)
	}
	return name
}

func knownIncompleteEnvironment(cfg Config) bool {
	if cfg.NextEnvironmentFile == nil || !environmentAbsent(cfg.EnvironmentFile.Path) {
		return false
	}
	paths, err := checkedEnvironmentPaths(cfg)
	if err != nil {
		return false
	}
	if _, err := readEnvironmentJournal(paths, cfg); err != nil {
		return false
	}
	status, _ := EnvironmentStatus(cfg)
	return status == "incomplete"
}

func ConfigHash(cfg Config) (string, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", ErrConfig
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// ValidatePlanFile is for a privileged CLI before opening a selected plan. API
// users must not be able to replace the plan, its parent path or its ownership.
// The caller should also compare the opened handle's identity while reading.
func ValidatePlanFile(name string) error {
	info, err := checkedPath(name, false)
	if err != nil || info.Size() < 1 || info.Size() > 64<<10 || !privateInfo(info) || !trustedParents(name) {
		return ErrConfig
	}
	return nil
}
func validDigest(s string) bool {
	data, err := hex.DecodeString(s)
	return err == nil && len(data) == sha256.Size
}
func runtimeRoot(s string) bool {
	return s != "/" && strings.HasPrefix(s, "/") && path.Clean(s) == s && !strings.ContainsAny(s, "\\:\x00") && strings.IndexFunc(s, unicode.IsControl) < 0 && len(s) <= 4096
}
func overlap(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		a, b = strings.ToLower(a), strings.ToLower(b)
	}
	relative, err := filepath.Rel(a, b)
	if err == nil && (relative == "." || filepath.IsLocal(relative)) {
		return true
	}
	relative, err = filepath.Rel(b, a)
	return err == nil && (relative == "." || filepath.IsLocal(relative))
}
func containsPath(root, name string) bool {
	if runtime.GOOS == "windows" {
		root, name = strings.ToLower(root), strings.ToLower(name)
	}
	relative, err := filepath.Rel(root, name)
	return err == nil && (relative == "." || filepath.IsLocal(relative))
}
func checkedPath(name string, directory bool) (os.FileInfo, error) {
	if !configPath(name) {
		return nil, ErrConfig
	}
	info, err := os.Lstat(name)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || directory != info.IsDir() || !directory && !info.Mode().IsRegular() {
		return nil, ErrConfig
	}
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil || runtime.GOOS != "windows" && resolved != name || runtime.GOOS == "windows" && !strings.EqualFold(resolved, name) {
		return nil, ErrConfig
	}
	return info, nil
}
func configPath(name string) bool {
	return name != "" && filepath.IsAbs(name) && filepath.Clean(name) == name && strings.IndexFunc(name, unicode.IsControl) < 0
}
func missingTarget(name string) bool {
	if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	_, err := checkedPath(filepath.Dir(name), true)
	return err == nil
}
func privateInfo(info os.FileInfo) bool {
	return environmentFilePrivate(info)
}

func trustedParents(name string) bool {
	for directory := filepath.Dir(name); ; directory = filepath.Dir(directory) {
		info, err := checkedPath(directory, true)
		if err != nil || !trustedMaintenanceParent(info) {
			return false
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return true
		}
	}
}
func stepTimeout(cfg Config) time.Duration {
	if cfg.StepTimeoutSeconds == 0 {
		return time.Minute
	}
	return time.Duration(cfg.StepTimeoutSeconds) * time.Second
}
func healthTimeout(cfg Config) time.Duration {
	if cfg.HealthTimeoutSeconds == 0 {
		return 2 * time.Minute
	}
	return time.Duration(cfg.HealthTimeoutSeconds) * time.Second
}
