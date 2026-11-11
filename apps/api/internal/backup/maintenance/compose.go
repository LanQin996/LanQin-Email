package maintenance

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/go-sql-driver/mysql"
)

var (
	errCompose         = errors.New("deployment controller could not verify the selected Compose services; retain maintenance state")
	errComposeNotReady = errors.New("selected Compose services are not healthy")
	composeContainerID = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type composeCommand func(context.Context, Config, []string) ([]byte, error)

// ComposeController only operates the repository's fixed application service
// sets. It never shells out, controls a database service, removes a deployment,
// changes a volume, builds/pulls images or receives commands from the Web API.
type ComposeController struct {
	run                composeCommand
	mu                 sync.Mutex
	configHash         string
	rendered           map[string]string
	images             map[string]string
	imageCommands      map[string]composeImageCommand
	rootIdentity       func(string) (string, error)
	renderPinOperation string
}

func NewComposeController() *ComposeController { return &ComposeController{run: runComposeDocker} }

func composeServices(mode string) []string {
	if mode == "all-in-one" {
		return []string{"lanqin-email"}
	}
	if mode == "stack" {
		return []string{"api", "postfix", "dovecot", "rspamd", "web", "nginx"}
	}
	return nil
}

func composePrefix(cfg Config) []string {
	args := []string{"compose", "--ansi", "never", "--project-directory", cfg.ProjectDirectory, "--project-name", cfg.ProjectName, "--env-file", cfg.EnvironmentFile.Path}
	for _, file := range cfg.ComposeFiles {
		args = append(args, "--file", file.Path)
	}
	return args
}

func (c *ComposeController) invoke(ctx context.Context, cfg Config, args []string) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if VerifyPinnedFiles(cfg) != nil {
		return nil, ErrConfig
	}
	limited, cancel := context.WithTimeout(ctx, stepTimeout(cfg))
	defer cancel()
	runner := c.run
	if runner == nil {
		runner = runComposeDocker
	}
	data, err := runner(limited, cfg, args)
	if err != nil || len(data) > composeCommandOutputLimit(args) {
		clear(data)
		return nil, errCompose
	}
	return data, nil
}

func (c *ComposeController) compose(ctx context.Context, cfg Config, args ...string) ([]byte, error) {
	return c.invoke(ctx, cfg, append(composePrefix(cfg), args...))
}

type composeVolume struct {
	Type     string `json:"type"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

type composeService struct {
	Image       string             `json:"image"`
	Build       json.RawMessage    `json:"build"`
	Command     json.RawMessage    `json:"command"`
	Entrypoint  json.RawMessage    `json:"entrypoint"`
	Environment map[string]*string `json:"environment"`
	EnvFile     json.RawMessage    `json:"env_file"`
	Volumes     []composeVolume    `json:"volumes"`
	Healthcheck struct {
		Disable bool     `json:"disable"`
		Test    []string `json:"test"`
	} `json:"healthcheck"`
	Scale  int `json:"scale"`
	Deploy struct {
		Replicas *int `json:"replicas"`
	} `json:"deploy"`
}

type composeDeployment struct {
	Name     string                    `json:"name"`
	Services map[string]composeService `json:"services"`
}

type expectedComposeMount struct {
	source, target string
	readOnly       bool
}

func expectedComposeMounts(cfg Config, service string) []expectedComposeMount {
	data := expectedComposeMount{cfg.Targets.Data, cfg.RuntimeDataRoot, false}
	mail := expectedComposeMount{cfg.Targets.Mail, cfg.RuntimeMailRoot, false}
	dkim := expectedComposeMount{cfg.Targets.DKIM, cfg.RuntimeDKIMRoot, false}
	switch service {
	case "lanqin-email":
		return []expectedComposeMount{data, mail, dkim}
	case "api":
		mail.readOnly = true
		return []expectedComposeMount{data, mail}
	case "postfix", "dovecot":
		return []expectedComposeMount{data, mail}
	case "rspamd":
		data.readOnly = true
		return []expectedComposeMount{data, dkim}
	}
	return nil
}

func validateComposeMounts(cfg Config, service string, volumes []composeVolume) error {
	expected := expectedComposeMounts(cfg, service)
	matched := map[string]bool{}
	for _, volume := range volumes {
		if !strings.HasPrefix(volume.Target, "/") || path.Clean(volume.Target) != volume.Target {
			return errCompose
		}
		approved := false
		for _, want := range expected {
			if volume.Target == want.target {
				if matched[want.target] || volume.Type != "bind" || !filepath.IsAbs(volume.Source) || filepath.Clean(volume.Source) != filepath.Clean(want.source) || volume.ReadOnly != want.readOnly {
					return errCompose
				}
				matched[want.target], approved = true, true
			}
		}
		if approved {
			continue
		}
		for _, protected := range []string{"/etc/postfix", "/etc/supervisor", "/entrypoint.sh", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
			if volume.Target == protected || strings.HasPrefix(volume.Target, protected+"/") || strings.HasPrefix(protected, volume.Target+"/") {
				return errCompose
			}
		}
		for _, physical := range []string{cfg.Targets.Data, cfg.Targets.Mail, cfg.Targets.DKIM} {
			if volume.Source != "" && volume.Type == "bind" && overlap(volume.Source, physical) {
				return errCompose
			}
		}
		for _, root := range []string{cfg.RuntimeDataRoot, cfg.RuntimeMailRoot, cfg.RuntimeDKIMRoot} {
			if volume.Target == root || strings.HasPrefix(volume.Target, root+"/") || strings.HasPrefix(root, volume.Target+"/") {
				return errCompose
			}
		}
	}
	if len(matched) != len(expected) {
		return errCompose
	}
	return nil
}

func validateComposeEnvironmentFiles(cfg Config, deployment composeDeployment) error {
	for _, service := range deployment.Services {
		if len(service.EnvFile) == 0 || bytes.Equal(service.EnvFile, []byte("null")) {
			continue
		}
		var files []json.RawMessage
		if json.Unmarshal(service.EnvFile, &files) != nil || len(files) > 1 {
			return errCompose
		}
		for _, raw := range files {
			var name string
			if json.Unmarshal(raw, &name) != nil {
				var item struct {
					Path     string `json:"path"`
					Required *bool  `json:"required"`
				}
				if json.Unmarshal(raw, &item) != nil || item.Path == "" || item.Required != nil && !*item.Required {
					return errCompose
				}
				name = item.Path
			}
			if name != cfg.EnvironmentFile.Path {
				return errCompose
			}
		}
	}
	return nil
}

func (c *ComposeController) deployment(ctx context.Context, cfg Config) (composeDeployment, error) {
	return c.deploymentMode(ctx, cfg, false)
}

func (c *ComposeController) metadataDeployment(ctx context.Context, cfg Config) (composeDeployment, error) {
	return c.deploymentMode(ctx, cfg, true)
}

func (c *ComposeController) deploymentMode(ctx context.Context, cfg Config, metadataOnly bool) (composeDeployment, error) {
	var deployment composeDeployment
	if ValidateRecoveryConfig(cfg) != nil || cfg.RuntimeDataRoot != "/data" || cfg.RuntimeMailRoot != "/var/mail/vhosts" || cfg.RuntimeDKIMRoot != "/var/lib/rspamd/dkim" {
		return deployment, ErrConfig
	}
	configHash, err := ConfigHash(cfg)
	if err != nil {
		return deployment, ErrConfig
	}
	environmentDigest, environmentInfo, err := composeEnvironmentVersion(cfg)
	if err != nil {
		return deployment, ErrConfig
	}
	unresolved, err := c.compose(ctx, cfg, "config", "--no-env-resolution", "--format", "json")
	if err != nil {
		return deployment, err
	}
	defer clear(unresolved)
	var inputs composeDeployment
	if json.Unmarshal(unresolved, &inputs) != nil || validateComposeEnvironmentFiles(cfg, inputs) != nil {
		return deployment, errCompose
	}
	data, err := c.compose(ctx, cfg, "config", "--format", "json")
	if err != nil {
		return deployment, err
	}
	defer clear(data)
	if json.Unmarshal(data, &deployment) != nil || deployment.Name != cfg.ProjectName || len(deployment.Services) < len(composeServices(cfg.Mode)) || len(deployment.Services) > 8 {
		return deployment, errCompose
	}
	allowed := map[string]bool{"mysql": true, "postgres": true}
	for _, name := range composeServices(cfg.Mode) {
		allowed[name] = true
		if _, ok := deployment.Services[name]; !ok {
			return deployment, errCompose
		}
	}
	for name, service := range deployment.Services {
		if !allowed[name] || service.Scale > 1 || service.Deploy.Replicas != nil && *service.Deploy.Replicas != 1 || validateComposeMounts(cfg, name, service.Volumes) != nil {
			return deployment, errCompose
		}
		if name != "mysql" && name != "postgres" && (service.Healthcheck.Disable || len(service.Healthcheck.Test) < 1 || service.Healthcheck.Test[0] == "NONE") {
			return deployment, errCompose
		}
		if name != "mysql" && name != "postgres" && (service.Image == "" || strings.HasPrefix(service.Image, "-") || len(service.Image) > 512 || strings.IndexFunc(service.Image, unicode.IsSpace) >= 0 || strings.IndexFunc(service.Image, unicode.IsControl) >= 0) {
			return deployment, errCompose
		}
		if name != "mysql" && name != "postgres" && (len(service.Command) > 0 && !bytes.Equal(service.Command, []byte("null")) || len(service.Entrypoint) > 0 && !bytes.Equal(service.Entrypoint, []byte("null"))) {
			return deployment, errCompose
		}
		if cfg.Driver != "sqlite" && (name == "api" || name == "lanqin-email") && len(service.Build) > 0 && !bytes.Equal(service.Build, []byte("null")) {
			return deployment, errCompose
		}
		for _, value := range service.Environment {
			if value == nil {
				return deployment, errCompose
			}
		}
	}
	if validateComposeRuntime(cfg, deployment, false) != nil {
		return deployment, errCompose
	}
	imageIDs := map[string]string{}
	imageCommands := map[string]composeImageCommand{}
	if !metadataOnly {
		for _, service := range composeServices(cfg.Mode) {
			output, err := c.invoke(ctx, cfg, []string{"image", "inspect", "--format", "{{json .}}", "--", deployment.Services[service].Image})
			if err != nil {
				return deployment, err
			}
			var image struct {
				ID     string              `json:"Id"`
				Config composeImageCommand `json:"Config"`
			}
			parseErr := json.Unmarshal(output, &image)
			clear(output)
			if parseErr != nil || !strings.HasPrefix(image.ID, "sha256:") || !composeContainerID.MatchString(strings.TrimPrefix(image.ID, "sha256:")) {
				return deployment, errCompose
			}
			imageIDs[service] = image.ID
			imageCommands[service] = image.Config
		}
	}
	if !metadataOnly {
		if baseline, err := readComposeOriginal(cfg); err == nil {
			for service, id := range imageIDs {
				if baseline.Images[service] != id {
					return deployment, ErrConfig
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return deployment, ErrConfig
		}
	}
	// Compose may read files referenced by the pinned inputs. Freeze its actual
	// rendered result too; the explicitly pinned old/new env selects the slot.
	afterDigest, afterEnvironment, err := composeEnvironmentVersion(cfg)
	if err != nil || afterDigest != environmentDigest || !os.SameFile(environmentInfo, afterEnvironment) || !environmentInfo.ModTime().Equal(afterEnvironment.ModTime()) {
		return deployment, ErrConfig
	}
	var rendered any
	if json.Unmarshal(data, &rendered) != nil {
		return deployment, errCompose
	}
	canonical, err := json.Marshal(rendered)
	if err != nil {
		return deployment, errCompose
	}
	renderHash := sha256.Sum256(canonical)
	clear(canonical)
	if !metadataOnly {
		if err := c.verifyComposeRenderPin(ctx, cfg, environmentDigest, hex.EncodeToString(renderHash[:])); err != nil {
			return deployment, err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.configHash != "" && c.configHash != configHash {
		return deployment, ErrConfig
	}
	c.configHash = configHash
	if metadataOnly {
		return deployment, nil
	}
	if c.rendered == nil {
		c.rendered = map[string]string{}
	}
	if c.images == nil {
		c.images = map[string]string{}
	}
	if c.imageCommands == nil {
		c.imageCommands = map[string]composeImageCommand{}
	}
	for service, id := range imageIDs {
		if previous := c.images[service]; previous != "" && previous != id {
			return deployment, ErrConfig
		}
		c.images[service] = id
		c.imageCommands[service] = imageCommands[service]
	}
	slot := environmentDigest
	digest := hex.EncodeToString(renderHash[:])
	if previous := c.rendered[slot]; previous != "" && previous != digest {
		return deployment, ErrConfig
	}
	c.rendered[slot] = digest
	if VerifyPinnedFiles(cfg) != nil {
		return deployment, ErrConfig
	}
	return deployment, nil
}

func composeEnvironmentVersion(cfg Config) (string, os.FileInfo, error) {
	digest := strings.ToLower(cfg.EnvironmentFile.SHA256)
	info, err := environmentFileHash(cfg.EnvironmentFile.Path, digest)
	if err == nil {
		return digest, info, nil
	}
	if cfg.NextEnvironmentFile == nil {
		return "", nil, ErrConfig
	}
	digest = strings.ToLower(cfg.NextEnvironmentFile.SHA256)
	info, err = environmentFileHash(cfg.EnvironmentFile.Path, digest)
	return digest, info, err
}

type composeContainer struct {
	ID     string `json:"Id"`
	Image  string `json:"Image"`
	Config struct {
		Labels      map[string]string `json:"Labels"`
		Env         []string          `json:"Env"`
		Cmd         []string          `json:"Cmd"`
		Entrypoint  []string          `json:"Entrypoint"`
		Healthcheck struct {
			Test []string `json:"Test"`
		} `json:"Healthcheck"`
	} `json:"Config"`
	State struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		Restarting bool   `json:"Restarting"`
		Paused     bool   `json:"Paused"`
		Dead       bool   `json:"Dead"`
		Pid        int    `json:"Pid"`
		Health     struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	Mounts []struct {
		Type, Source, Destination string
		RW                        bool
	} `json:"Mounts"`
}

type composeImageCommand struct {
	Cmd        []string `json:"Cmd"`
	Entrypoint []string `json:"Entrypoint"`
}

func containerEnvironment(values []string) (map[string]string, error) {
	result := map[string]string{}
	if len(values) > 1024 {
		return nil, errCompose
	}
	for _, value := range values {
		key, value, ok := strings.Cut(value, "=")
		if !ok || key == "" {
			return nil, errCompose
		}
		if _, exists := result[key]; exists {
			return nil, errCompose
		}
		result[key] = value
	}
	return result, nil
}

func (c *ComposeController) inventory(ctx context.Context, cfg Config, deployment composeDeployment, checkEnvironment bool) ([]composeContainer, error) {
	data, err := c.invoke(ctx, cfg, []string{"ps", "--all", "--quiet", "--no-trunc", "--filter", "label=com.docker.compose.project=" + cfg.ProjectName})
	if err != nil {
		return nil, err
	}
	defer clear(data)
	ids := strings.Fields(string(data))
	if len(ids) > 128 {
		return nil, errCompose
	}
	if len(ids) == 0 {
		return []composeContainer{}, nil
	}
	expectedIDs := map[string]bool{}
	for _, id := range ids {
		if !composeContainerID.MatchString(id) || expectedIDs[id] {
			return nil, errCompose
		}
		expectedIDs[id] = true
	}
	inspect, err := c.invoke(ctx, cfg, append([]string{"inspect", "--type", "container"}, ids...))
	if err != nil {
		return nil, err
	}
	defer clear(inspect)
	var containers []composeContainer
	if json.Unmarshal(inspect, &containers) != nil || len(containers) != len(ids) {
		return nil, errCompose
	}
	allowed := map[string]bool{}
	actualRuntime := composeDeployment{Name: deployment.Name, Services: map[string]composeService{}}
	for name, service := range deployment.Services {
		actualRuntime.Services[name] = service
	}
	for _, service := range composeServices(cfg.Mode) {
		allowed[service] = true
	}
	for _, container := range containers {
		if !expectedIDs[container.ID] || container.Config.Labels["com.docker.compose.project"] != cfg.ProjectName {
			return nil, errCompose
		}
		// The deployment lock is anchored in this canonical directory. A copied
		// plan in another directory must not control the same Docker -p scope
		// while holding a different filesystem lock.
		workingDirectory := container.Config.Labels["com.docker.compose.project.working_dir"]
		if !filepath.IsAbs(workingDirectory) || filepath.Clean(workingDirectory) != workingDirectory || workingDirectory != cfg.ProjectDirectory {
			return nil, errCompose
		}
		delete(expectedIDs, container.ID)
		service := container.Config.Labels["com.docker.compose.service"]
		if !allowed[service] && service != "mysql" && service != "postgres" {
			return nil, errCompose
		}
		volumes := make([]composeVolume, 0, len(container.Mounts))
		for _, mount := range container.Mounts {
			volumes = append(volumes, composeVolume{Type: mount.Type, Source: mount.Source, Target: mount.Destination, ReadOnly: !mount.RW})
		}
		if validateComposeMounts(cfg, service, volumes) != nil {
			return nil, errCompose
		}
		if !allowed[service] {
			continue
		}
		if container.State.Dead || container.State.Pid < 0 {
			return nil, errCompose
		}
		switch container.State.Status {
		case "running":
			if !container.State.Running || container.State.Restarting || container.State.Pid == 0 {
				return nil, errCompose
			}
		case "restarting":
			if !container.State.Restarting {
				return nil, errCompose
			}
		case "paused":
			if !container.State.Paused || !container.State.Running {
				return nil, errCompose
			}
		case "exited", "created":
			if container.State.Running || container.State.Restarting || container.State.Paused || container.State.Pid != 0 {
				return nil, errCompose
			}
		default:
			return nil, errCompose
		}
		oneoff := strings.EqualFold(container.Config.Labels["com.docker.compose.oneoff"], "true")
		if checkEnvironment && !oneoff {
			c.mu.Lock()
			expectedImage, imageCommand := c.images[service], c.imageCommands[service]
			c.mu.Unlock()
			if container.Image != expectedImage || !slices.Equal(container.Config.Cmd, imageCommand.Cmd) || !slices.Equal(container.Config.Entrypoint, imageCommand.Entrypoint) {
				return nil, errCompose
			}
			actual, err := containerEnvironment(container.Config.Env)
			if err != nil {
				return nil, err
			}
			for key, value := range deployment.Services[service].Environment {
				actualValue, present := actual[key]
				if value == nil || !present || actualValue != *value {
					return nil, errCompose
				}
			}
			// Image defaults are not necessarily present in `compose config`.
			// Validate the real effective role environment too, so an inherited
			// DB path/DSN cannot silently point a healthy container elsewhere.
			actualService := actualRuntime.Services[service]
			actualService.Environment = map[string]*string{}
			for key, value := range actual {
				copy := value
				actualService.Environment[key] = &copy
			}
			actualRuntime.Services[service] = actualService
			if validateComposeRuntime(cfg, actualRuntime, false) != nil {
				return nil, errCompose
			}
			test := container.Config.Healthcheck.Test
			if len(test) == 0 || test[0] == "NONE" {
				return nil, errCompose
			}
		}
	}
	return containers, nil
}

func completeComposeServices(cfg Config, containers []composeContainer, exact bool) bool {
	counts := map[string]int{}
	for _, container := range containers {
		if appContainer(cfg, container) && !strings.EqualFold(container.Config.Labels["com.docker.compose.oneoff"], "true") {
			counts[container.Config.Labels["com.docker.compose.service"]]++
		}
	}
	for _, service := range composeServices(cfg.Mode) {
		if counts[service] < 1 || exact && counts[service] != 1 {
			return false
		}
	}
	return true
}

func appContainer(cfg Config, container composeContainer) bool {
	for _, service := range composeServices(cfg.Mode) {
		if container.Config.Labels["com.docker.compose.service"] == service {
			return true
		}
	}
	return false
}

func stoppedComposeContainers(cfg Config, containers []composeContainer) bool {
	for _, container := range containers {
		if appContainer(cfg, container) && (container.State.Running || container.State.Restarting || container.State.Paused || container.State.Pid != 0 || container.State.Status != "exited" && container.State.Status != "created") {
			return false
		}
	}
	return true
}

func (c *ComposeController) Preflight(ctx context.Context, cfg Config) error {
	if cfg.Preservation != nil {
		if _, err := preservationIdentity(cfg); err != nil {
			return err
		}
	}
	c.authorizeInitialRenderPins(cfg)
	deployment, err := c.deployment(ctx, cfg)
	if err != nil {
		return err
	}
	containers, err := c.inventory(ctx, cfg, deployment, true)
	if err != nil {
		return err
	}
	if !completeComposeServices(cfg, containers, false) {
		return errCompose
	}
	return c.recordOriginalCompose(ctx, cfg, containers)
}

func (c *ComposeController) PreflightRecovery(ctx context.Context, cfg Config) error {
	c.mu.Lock()
	c.renderPinOperation = ""
	c.mu.Unlock()
	deployment, err := c.metadataDeployment(ctx, cfg)
	if err != nil {
		return err
	}
	containers, err := c.inventory(ctx, cfg, deployment, false)
	if err != nil {
		return err
	}
	if !completeComposeServices(cfg, containers, false) {
		return errCompose
	}
	return nil
}

func (c *ComposeController) Stop(ctx context.Context, cfg Config) error {
	deployment, err := c.metadataDeployment(ctx, cfg)
	if err != nil {
		return err
	}
	if _, err := c.inventory(ctx, cfg, deployment, false); err != nil {
		return err
	}
	if _, err := c.compose(ctx, cfg, append([]string{"stop", "--timeout", "30"}, composeServices(cfg.Mode)...)...); err != nil {
		return err
	}
	containers, err := c.inventory(ctx, cfg, deployment, false)
	if err != nil {
		return err
	}
	remaining := []string{}
	for _, container := range containers {
		if appContainer(cfg, container) && (container.State.Running || container.State.Restarting || container.State.Paused) {
			remaining = append(remaining, container.ID)
		}
	}
	if len(remaining) > 0 {
		if _, err := c.invoke(ctx, cfg, append([]string{"container", "stop", "--time", "30"}, remaining...)); err != nil {
			return err
		}
	}
	return nil
}

func (c *ComposeController) VerifyStopped(ctx context.Context, cfg Config) error {
	deployment, err := c.metadataDeployment(ctx, cfg)
	if err != nil {
		return err
	}
	containers, err := c.inventory(ctx, cfg, deployment, false)
	if err != nil {
		return err
	}
	if !stoppedComposeContainers(cfg, containers) {
		return errCompose
	}
	return nil
}

func (c *ComposeController) Recreate(ctx context.Context, cfg Config) error {
	if err := c.EnsureSafePostfixQueues(ctx, cfg); err != nil {
		return err
	}
	deployment, err := c.deployment(ctx, cfg)
	if err != nil {
		return err
	}
	before, err := c.inventory(ctx, cfg, deployment, false)
	if err != nil || !stoppedComposeContainers(cfg, before) {
		return errCompose
	}
	old := map[string]bool{}
	for _, container := range before {
		if appContainer(cfg, container) {
			old[container.ID] = true
		}
	}
	args := append([]string{"up", "--detach", "--no-deps", "--force-recreate", "--no-build", "--pull", "never"}, composeServices(cfg.Mode)...)
	if _, err := c.compose(ctx, cfg, args...); err != nil {
		return err
	}
	after, err := c.inventory(ctx, cfg, deployment, true)
	if err != nil {
		return err
	}
	if !completeComposeServices(cfg, after, true) {
		return errCompose
	}
	for _, container := range after {
		if appContainer(cfg, container) && !strings.EqualFold(container.Config.Labels["com.docker.compose.oneoff"], "true") && old[container.ID] {
			return errCompose
		}
	}
	return nil
}

func (c *ComposeController) Healthy(ctx context.Context, cfg Config) error {
	deadline, cancel := context.WithTimeout(ctx, healthTimeout(cfg))
	defer cancel()
	for {
		deployment, err := c.deployment(deadline, cfg)
		if err != nil {
			return err
		}
		containers, err := c.inventory(deadline, cfg, deployment, true)
		if err != nil {
			return err
		}
		ready := completeComposeServices(cfg, containers, true)
		for _, container := range containers {
			if !appContainer(cfg, container) {
				continue
			}
			if strings.EqualFold(container.Config.Labels["com.docker.compose.oneoff"], "true") {
				if container.State.Running || container.State.Restarting || container.State.Paused {
					ready = false
				}
				continue
			}
			if container.State.Status != "running" || !container.State.Running || container.State.Paused || container.State.Restarting || container.State.Health.Status != "healthy" {
				ready = false
				continue
			}
			for _, command := range composeRoleChecks(container.Config.Labels["com.docker.compose.service"]) {
				if _, err := c.invoke(deadline, cfg, append([]string{"exec", container.ID}, command...)); err != nil {
					ready = false
					break
				}
			}
		}
		if ready {
			return nil
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-deadline.Done():
			timer.Stop()
			return errComposeNotReady
		case <-timer.C:
		}
	}
}

func composeRoleChecks(service string) [][]string {
	web := []string{"wget", "-q", "-T", "10", "-O", "/dev/null", "http://127.0.0.1/"}
	api := []string{"curl", "--fail", "--silent", "--show-error", "--max-time", "10", "http://127.0.0.1:8080/readyz"}
	postfix := []string{"postfix", "status"}
	dovecot := []string{"doveadm", "service", "status"}
	rspamd := []string{"rspamadm", "control", "stat"}
	switch service {
	case "lanqin-email":
		api[len(api)-1] = "http://127.0.0.1/readyz"
		return [][]string{api, postfix, dovecot, rspamd}
	case "api":
		return [][]string{api}
	case "postfix":
		return [][]string{postfix}
	case "dovecot":
		return [][]string{dovecot}
	case "rspamd":
		return [][]string{rspamd}
	case "web":
		return [][]string{web}
	case "nginx":
		web[len(web)-1] = "http://127.0.0.1/readyz"
		return [][]string{web}
	}
	return nil
}

type composeDatabaseIdentity struct{ host, port, database, user, password string }

func composeAPIIdentity(driver string, environment map[string]*string) (composeDatabaseIdentity, string, error) {
	var identity composeDatabaseIdentity
	get := func(key string) string {
		if value := environment[key]; value != nil {
			return *value
		}
		return ""
	}
	reference := "LANQIN_DATABASE_URL"
	dsn := get(reference)
	if dsn == "" {
		reference = "LANQIN_DB_DSN"
		dsn = get(reference)
	}
	if dsn == "" {
		return identity, "", errCompose
	}
	if driver == "mysql" {
		parsed, err := mysql.ParseDSN(dsn)
		if err != nil || parsed.Net != "tcp" {
			return identity, "", errCompose
		}
		host, port, err := net.SplitHostPort(parsed.Addr)
		if err != nil {
			return identity, "", errCompose
		}
		identity = composeDatabaseIdentity{host, port, parsed.DBName, parsed.User, parsed.Passwd}
	} else {
		parsed, err := url.Parse(dsn)
		if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Hostname() == "" || parsed.Port() == "" || parsed.User == nil || !strings.HasPrefix(parsed.Path, "/") {
			return identity, "", errCompose
		}
		connectionOptions := map[string]bool{"sslmode": true, "sslrootcert": true, "sslcert": true, "sslkey": true, "sslpassword": true, "ssl_min_protocol_version": true, "ssl_max_protocol_version": true, "channel_binding": true, "connect_timeout": true, "application_name": true}
		for key := range parsed.Query() {
			if !connectionOptions[key] {
				return identity, "", errCompose
			}
		}
		password, ok := parsed.User.Password()
		if !ok {
			return identity, "", errCompose
		}
		identity = composeDatabaseIdentity{parsed.Hostname(), parsed.Port(), strings.TrimPrefix(parsed.Path, "/"), parsed.User.Username(), password}
	}
	if identity.host == "" || identity.database == "" || identity.user == "" || identity.password == "" || strings.ContainsAny(identity.host, "/\\\x00\r\n") {
		return identity, "", errCompose
	}
	// Dovecot writes these keyword values without quoting. Whitespace, quotes
	// and backslash escapes can change the connection actually read by libpq;
	// reject this unsupported representation before stopping any live writer.
	for _, value := range []string{identity.host, identity.database, identity.user, identity.password} {
		if len(value) > 16384 || strings.ContainsAny(value, "'\\") || strings.IndexFunc(value, unicode.IsSpace) >= 0 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return identity, "", errCompose
		}
	}
	port, err := strconv.Atoi(identity.port)
	if err != nil || port < 1 || port > 65535 {
		return identity, "", errCompose
	}
	identity.port = strconv.Itoa(port)
	return identity, reference, nil
}

func validateComposeRuntime(cfg Config, deployment composeDeployment, newDatabase bool) error {
	apiName := "api"
	roles := []string{"api", "postfix", "dovecot", "rspamd"}
	if cfg.Mode == "all-in-one" {
		apiName = "lanqin-email"
		roles = []string{apiName}
	}
	get := func(environment map[string]*string, key string) string {
		if value := environment[key]; value != nil {
			return *value
		}
		return ""
	}
	apiEnvironment := deployment.Services[apiName].Environment
	for _, role := range roles {
		environment := deployment.Services[role].Environment
		if mailConfig := get(environment, "MAIL_CONFIG"); mailConfig != "" && mailConfig != "/etc/postfix" {
			return errCompose
		}
		// pgx/libpq merge these process variables even when the API URL names
		// host/database/user explicitly. A mail daemon may clean its environment
		// differently, so they cannot prove a shared schema or session identity.
		if cfg.Driver == "postgres" {
			for _, key := range []string{"PGOPTIONS", "PGSERVICE", "PGSERVICEFILE", "PGHOSTADDR"} {
				if get(environment, key) != "" {
					return errCompose
				}
			}
		}
		driver := get(environment, "LANQIN_DB_DRIVER")
		if driver == "" {
			driver = "sqlite"
		}
		if driver != cfg.Driver {
			return errCompose
		}
		if root := get(environment, "LANQIN_DB_PATH"); root != "" && root != cfg.RuntimeDataRoot+"/lanqin.db" {
			return errCompose
		}
	}
	if get(apiEnvironment, "LANQIN_DATA_DIR") != cfg.RuntimeDataRoot || get(apiEnvironment, "LANQIN_MAILDIR_ROOT") != cfg.RuntimeMailRoot {
		return errCompose
	}
	rspamdName := "rspamd"
	if cfg.Mode == "all-in-one" {
		rspamdName = apiName
	}
	if root := get(deployment.Services[rspamdName].Environment, "LANQIN_RSPAMD_DKIM_DIR"); root != "" && root != cfg.RuntimeDKIMRoot {
		return errCompose
	}
	if cfg.Driver == "sqlite" {
		if get(apiEnvironment, "LANQIN_DATABASE_URL") != "" || get(apiEnvironment, "LANQIN_DB_DSN") != "" {
			return errCompose
		}
		return nil
	}
	identity, _, err := composeAPIIdentity(cfg.Driver, apiEnvironment)
	if err != nil {
		return err
	}
	for _, role := range roles {
		if role == "api" {
			continue
		}
		environment := deployment.Services[role].Environment
		other := composeDatabaseIdentity{get(environment, "LANQIN_DB_HOST"), get(environment, "LANQIN_DB_PORT"), get(environment, "LANQIN_DB_NAME"), get(environment, "LANQIN_DB_USER"), get(environment, "LANQIN_DB_PASSWORD")}
		if !strings.EqualFold(identity.host, other.host) || identity.port != other.port || identity.database != other.database || identity.user != other.user || identity.password != other.password {
			return errCompose
		}
	}
	if newDatabase {
		if cfg.RuntimeDatabase == nil || !strings.EqualFold(identity.host, cfg.RuntimeDatabase.Host) || identity.port != strconv.Itoa(cfg.RuntimeDatabase.Port) || identity.database != cfg.RuntimeDatabase.Database || identity.user != cfg.RuntimeDatabase.User {
			return errCompose
		}
	}
	return nil
}

// ValidateNativeRuntime proves that the actual container-side connection opens
// the activated new database paired with the installed plan, without App.New,
// listeners, workers, migrations or publishing service ports.
func (c *ComposeController) ValidateNativeRuntime(ctx context.Context, cfg Config, planSHA string) error {
	if cfg.Driver != "mysql" && cfg.Driver != "postgres" || !validDigest(planSHA) {
		return ErrConfig
	}
	if err := c.VerifyStopped(ctx, cfg); err != nil {
		return err
	}
	deployment, err := c.deployment(ctx, cfg)
	if err != nil {
		return err
	}
	if err := validateComposeRuntime(cfg, deployment, true); err != nil {
		return err
	}
	service := "api"
	if cfg.Mode == "all-in-one" {
		service = "lanqin-email"
	}
	_, reference, err := composeAPIIdentity(cfg.Driver, deployment.Services[service].Environment)
	if err != nil {
		return err
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return errCompose
	}
	args := []string{"run", "--rm", "--no-deps", "--no-TTY", "--pull", "never", "--name", cfg.ProjectName + "-lanqin-verify-" + hex.EncodeToString(nonce[:]), "--label", "lanqin.backup.maintenance=verify-native", "--entrypoint", "/usr/local/bin/lanqin-backup", service, "verify-native", "--driver", cfg.Driver, "--dsn-env", reference, "--data-root", cfg.RuntimeDataRoot, "--mail-root", cfg.RuntimeMailRoot, "--expected-plan-sha256", planSHA}
	if _, err := c.compose(ctx, cfg, args...); err != nil {
		return err
	}
	return c.VerifyStopped(ctx, cfg)
}

type composeOutput struct {
	bytes.Buffer
	limit int
}

func (output *composeOutput) Write(data []byte) (int, error) {
	if len(data) > output.limit-output.Len() {
		return 0, errCompose
	}
	return output.Buffer.Write(data)
}

func runComposeDocker(ctx context.Context, cfg Config, args []string) ([]byte, error) {
	if runtime.GOOS != "linux" || !filepath.IsAbs(cfg.DockerPath) {
		return nil, errCompose
	}
	if _, err := checkedPath(cfg.DockerPath, false); err != nil {
		return nil, errCompose
	}
	command := exec.CommandContext(ctx, cfg.DockerPath, args...)
	command.Dir = cfg.ProjectDirectory
	command.Stdin = nil
	for _, key := range []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "XDG_RUNTIME_DIR", "TMPDIR"} {
		if cfg.Preservation != nil && key == cfg.Preservation.IdentityEnv {
			continue
		}
		if value, ok := os.LookupEnv(key); ok {
			command.Env = append(command.Env, key+"="+value)
		}
	}
	command.Env = append(command.Env, "DOCKER_CONTEXT=default", "COMPOSE_DISABLE_ENV_FILE=1", "COMPOSE_ANSI=never", "COMPOSE_INTERACTIVE_NO_CLI=1")
	output := &composeOutput{limit: composeCommandOutputLimit(args)}
	command.Stdout = output
	command.Stderr = io.Discard
	prepareComposeProcess(command)
	if err := command.Run(); err != nil {
		clear(output.Bytes())
		return nil, errCompose
	}
	return output.Bytes(), nil
}
