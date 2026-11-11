package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

const composeOriginalName = ".compose-original.json"

type composeOriginalBaseline struct {
	Version           int               `json:"version"`
	ConfigSHA256      string            `json:"configSHA256"`
	EnvironmentSHA256 string            `json:"environmentSHA256"`
	ContainerIDs      []string          `json:"containerIDs"`
	Roots             map[string]string `json:"rootFileIdentities"`
	PostfixImages     map[string]string `json:"postfixImages"`
	Images            map[string]string `json:"images"`
}

func postfixService(cfg Config) string {
	if cfg.Mode == "all-in-one" {
		return "lanqin-email"
	}
	return "postfix"
}

func composeAppIDs(cfg Config, containers []composeContainer) []string {
	ids := []string{}
	for _, container := range containers {
		if appContainer(cfg, container) {
			ids = append(ids, container.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func (c *ComposeController) rootIdentities(cfg Config) (map[string]string, error) {
	identity := c.rootIdentity
	if identity == nil {
		identity = composeRootIdentity
	}
	result := map[string]string{}
	for role, root := range map[string]string{"data": cfg.Targets.Data, "mail": cfg.Targets.Mail, "dkim": cfg.Targets.DKIM} {
		value, err := identity(root)
		if err != nil || value == "" || len(value) > 128 {
			return nil, errCompose
		}
		result[role] = value
	}
	return result, nil
}

func readComposeOriginal(cfg Config) (composeOriginalBaseline, error) {
	var baseline composeOriginalBaseline
	root, err := os.OpenRoot(cfg.StateDir)
	if err != nil {
		return baseline, errCompose
	}
	defer root.Close()
	info, err := root.Lstat(composeOriginalName)
	if err != nil {
		return baseline, err
	}
	if !info.Mode().IsRegular() || !privateInfo(info) || info.Size() < 1 || info.Size() > 64<<10 {
		return baseline, errCompose
	}
	file, err := root.Open(composeOriginalName)
	if err != nil {
		return baseline, errCompose
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return baseline, errCompose
	}
	data, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil || int64(len(data)) != info.Size() {
		return baseline, errCompose
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	hash, err := ConfigHash(cfg)
	if err != nil || decoder.Decode(&baseline) != nil || decoder.Decode(new(any)) != io.EOF || baseline.Version != 1 || baseline.ConfigSHA256 != hash || baseline.EnvironmentSHA256 != strings.ToLower(cfg.EnvironmentFile.SHA256) || len(baseline.ContainerIDs) < 1 || len(baseline.ContainerIDs) > 128 || len(baseline.Roots) != 3 || len(baseline.PostfixImages) != 1 || len(baseline.Images) != len(composeServices(cfg.Mode)) {
		return baseline, errCompose
	}
	seen := map[string]bool{}
	for _, id := range baseline.ContainerIDs {
		if !composeContainerID.MatchString(id) || seen[id] {
			return baseline, errCompose
		}
		seen[id] = true
	}
	for _, role := range []string{"data", "mail", "dkim"} {
		if baseline.Roots[role] == "" || len(baseline.Roots[role]) > 128 {
			return baseline, errCompose
		}
	}
	image := baseline.PostfixImages[postfixService(cfg)]
	if !strings.HasPrefix(image, "sha256:") || !composeContainerID.MatchString(strings.TrimPrefix(image, "sha256:")) {
		return baseline, errCompose
	}
	for _, service := range composeServices(cfg.Mode) {
		image := baseline.Images[service]
		if !strings.HasPrefix(image, "sha256:") || !composeContainerID.MatchString(strings.TrimPrefix(image, "sha256:")) {
			return baseline, errCompose
		}
	}
	if baseline.PostfixImages[postfixService(cfg)] != baseline.Images[postfixService(cfg)] {
		return baseline, errCompose
	}
	return baseline, nil
}

func (c *ComposeController) recordOriginalCompose(ctx context.Context, cfg Config, containers []composeContainer) error {
	if existing, err := readComposeOriginal(cfg); err == nil {
		return c.verifyOriginalCompose(cfg, containers, existing)
	} else if !errors.Is(err, os.ErrNotExist) {
		return errCompose
	}
	environment, _, err := composeEnvironmentVersion(cfg)
	if err != nil || environment != strings.ToLower(cfg.EnvironmentFile.SHA256) {
		return errCompose
	}
	roots, err := c.rootIdentities(cfg)
	if err != nil {
		return err
	}
	hash, err := ConfigHash(cfg)
	if err != nil {
		return err
	}
	baseline := composeOriginalBaseline{Version: 1, ConfigSHA256: hash, EnvironmentSHA256: environment, ContainerIDs: composeAppIDs(cfg, containers), Roots: roots, PostfixImages: map[string]string{}, Images: map[string]string{}}
	c.mu.Lock()
	for role, image := range c.images {
		baseline.Images[role] = image
	}
	c.mu.Unlock()
	for _, container := range containers {
		if container.Config.Labels["com.docker.compose.service"] != postfixService(cfg) || strings.EqualFold(container.Config.Labels["com.docker.compose.oneoff"], "true") {
			continue
		}
		if !container.State.Running || container.State.Paused || container.State.Restarting {
			return errCompose
		}
		output, err := c.invoke(ctx, cfg, []string{"exec", container.ID, "/usr/sbin/postconf", "-h", "queue_directory"})
		if err != nil {
			return err
		}
		correct := strings.TrimSpace(string(output)) == "/var/spool/postfix"
		clear(output)
		if !correct {
			return errCompose
		}
		if cfg.Driver == "sqlite" && c.verifySQLiteReader(ctx, cfg, container.ID) != nil {
			return errCompose
		}
		if previous := baseline.PostfixImages[postfixService(cfg)]; previous != "" && previous != container.Image {
			return errCompose
		}
		baseline.PostfixImages[postfixService(cfg)] = container.Image
	}
	if baseline.PostfixImages[postfixService(cfg)] == "" {
		return errCompose
	}
	data, err := json.Marshal(baseline)
	if err != nil || len(data) > 64<<10 {
		return errCompose
	}
	root, err := os.OpenRoot(cfg.StateDir)
	if err != nil {
		return errCompose
	}
	defer root.Close()
	file, err := root.OpenFile(composeOriginalName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errCompose
	}
	_, writeErr := file.Write(append(data, '\n'))
	syncErr, closeErr := file.Sync(), file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil || syncStateDirectory(cfg.StateDir) != nil {
		return errCompose
	}
	return nil
}

// Prove that the explicitly selected live database permissions can serve the
// actual Postfix account before stopping anything. Image IDs and the complete
// plan are already frozen by this same preflight.
func (c *ComposeController) verifySQLiteReader(ctx context.Context, cfg Config, id string) error {
	output, err := c.invoke(ctx, cfg, []string{"exec", id, "id", "-u", "postfix"})
	if err != nil {
		return errCompose
	}
	uid, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 32)
	clear(output)
	if err != nil || uid < 0 {
		return errCompose
	}
	if !cfg.SQLiteGroupRead {
		if uid != 0 && int(uid) != cfg.Owners.Data.UID {
			return errCompose
		}
		return nil
	}
	if uid == 0 || int(uid) == cfg.Owners.Data.UID || cfg.Owners.Data.GID <= 0 {
		return errCompose
	}
	// Postfix drops supplementary groups when changing UID; only its primary
	// group is sufficient authority to read the restored database.
	output, err = c.invoke(ctx, cfg, []string{"exec", id, "id", "-g", "postfix"})
	if err != nil {
		return errCompose
	}
	defer clear(output)
	gid, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 32)
	if err != nil || int(gid) != cfg.Owners.Data.GID {
		return errCompose
	}
	return nil
}

func (c *ComposeController) verifyOriginalCompose(cfg Config, containers []composeContainer, baseline composeOriginalBaseline) error {
	environment, _, err := composeEnvironmentVersion(cfg)
	if err != nil || environment != baseline.EnvironmentSHA256 {
		return errCompose
	}
	currentIDs := composeAppIDs(cfg, containers)
	expected := append([]string{}, baseline.ContainerIDs...)
	sort.Strings(expected)
	if len(currentIDs) != len(expected) {
		return errCompose
	}
	for i, id := range currentIDs {
		if id != expected[i] {
			return errCompose
		}
	}
	roots, err := c.rootIdentities(cfg)
	if err != nil {
		return err
	}
	for role, identity := range baseline.Roots {
		if roots[role] != identity {
			return errCompose
		}
	}
	return nil
}

// StartOriginal preserves the original writable container layers, including an
// unpersisted Postfix queue. It is only valid after proven unchanged data or an
// installer rollback that restored the original host directory identities.
func (c *ComposeController) StartOriginal(ctx context.Context, cfg Config) error {
	if err := c.VerifyStopped(ctx, cfg); err != nil {
		return err
	}
	baseline, err := readComposeOriginal(cfg)
	if err != nil {
		return errCompose
	}
	deployment, err := c.deployment(ctx, cfg)
	if err != nil {
		return err
	}
	before, err := c.inventory(ctx, cfg, deployment, true)
	if err != nil {
		return err
	}
	if err := c.verifyOriginalCompose(cfg, before, baseline); err != nil {
		return err
	}
	if _, err := c.compose(ctx, cfg, append([]string{"start"}, composeServices(cfg.Mode)...)...); err != nil {
		return err
	}
	after, err := c.inventory(ctx, cfg, deployment, true)
	if err != nil {
		return err
	}
	return c.verifyOriginalCompose(cfg, after, baseline)
}
