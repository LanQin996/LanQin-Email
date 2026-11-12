package maintenance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

type fakeComposeDocker struct {
	t                        *testing.T
	cfg                      Config
	deployment               composeDeployment
	containers               []composeContainer
	calls                    [][]string
	leaveComposeRunning      bool
	leaveSupplementalRunning bool
	keepContainerIDs         bool
	commandFailure           string
	imageChanged             bool
	imageHealthcheck         []string
	onConfig                 func()
	directoryIDs             map[string]string
	spoolArchives            map[string][]byte
	queueDirectory           string
	postfixUID               string
	postfixGroups            string
}

func composeString(value string) *string { return &value }

func newFakeComposeDocker(t *testing.T, mode string) (Config, *ComposeController, *fakeComposeDocker) {
	t.Helper()
	cfg := faultConfig(t)
	cfg.Mode = mode
	fake := &fakeComposeDocker{t: t, cfg: cfg}
	fake.setDeployment("sqlite", composeDatabaseIdentity{})
	return cfg, fake.controller(), fake
}

func (f *fakeComposeDocker) controller() *ComposeController {
	if _, err := os.Lstat(filepath.Join(f.cfg.StateDir, stateJournalName)); os.IsNotExist(err) {
		journal, err := createStateJournal(f.cfg)
		if err != nil {
			f.t.Fatal(err)
		}
		hash, err := ConfigHash(f.cfg)
		if err != nil {
			f.t.Fatal(err)
		}
		state := State{Version: 1, OperationID: strings.Repeat("1", 32), ConfigSHA256: hash, Phase: "preflight", PendingAction: "preflight"}
		if err := journal.save(&state); err != nil {
			f.t.Fatal(err)
		}
		if err := journal.file.Close(); err != nil {
			f.t.Fatal(err)
		}
	}
	if f.directoryIDs == nil {
		f.directoryIDs = map[string]string{}
	}
	return &ComposeController{run: f.run, rootIdentity: func(name string) (string, error) {
		if value := f.directoryIDs[name]; value != "" {
			return value, nil
		}
		hash := sha256.Sum256([]byte(name))
		value := hex.EncodeToString(hash[:])
		f.directoryIDs[name] = value
		return value, nil
	}}
}

func fakeComposePhase(t *testing.T, cfg Config, phase, pending string) {
	t.Helper()
	journal, state, err := openStateJournal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.file.Close()
	state.Phase, state.PendingAction = phase, pending
	if err := journal.save(&state); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeComposeDocker) setDeployment(driver string, identity composeDatabaseIdentity) {
	f.deployment = composeDeployment{Name: f.cfg.ProjectName, Services: map[string]composeService{}}
	for _, name := range append(composeServices(f.cfg.Mode), "mysql", "postgres") {
		service := composeService{Image: "synthetic.example.invalid/" + name + ":fixture", Environment: map[string]*string{}}
		if name != "mysql" && name != "postgres" {
			service.Healthcheck.Test = []string{"CMD", "synthetic-healthcheck"}
		}
		for _, mount := range expectedComposeMounts(f.cfg, name) {
			service.Volumes = append(service.Volumes, composeVolume{Type: "bind", Source: mount.source, Target: mount.target, ReadOnly: mount.readOnly})
		}
		if name != "web" && name != "nginx" && name != "mysql" && name != "postgres" {
			service.EnvFile, _ = json.Marshal([]map[string]any{{"path": f.cfg.EnvironmentFile.Path, "required": true}})
			for key, value := range map[string]string{"LANQIN_DB_DRIVER": driver, "LANQIN_DATA_DIR": f.cfg.RuntimeDataRoot, "LANQIN_DB_PATH": f.cfg.RuntimeDataRoot + "/lanqin.db", "LANQIN_MAILDIR_ROOT": f.cfg.RuntimeMailRoot, "LANQIN_RSPAMD_DKIM_DIR": f.cfg.RuntimeDKIMRoot} {
				service.Environment[key] = composeString(value)
			}
			if driver != "sqlite" {
				dsn := "postgres://" + identity.user + ":" + identity.password + "@" + identity.host + ":" + identity.port + "/" + identity.database + "?sslmode=disable"
				if driver == "mysql" {
					dsn = identity.user + ":" + identity.password + "@tcp(" + identity.host + ":" + identity.port + ")/" + identity.database + "?parseTime=true"
				}
				for key, value := range map[string]string{"LANQIN_DATABASE_URL": dsn, "LANQIN_DB_HOST": identity.host, "LANQIN_DB_PORT": identity.port, "LANQIN_DB_NAME": identity.database, "LANQIN_DB_USER": identity.user, "LANQIN_DB_PASSWORD": identity.password} {
					service.Environment[key] = composeString(value)
				}
			}
		}
		f.deployment.Services[name] = service
	}
	f.containers = nil
	names := make([]string, 0, len(f.deployment.Services))
	for name := range f.deployment.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	for i, name := range names {
		f.containers = append(f.containers, f.container(name, i+1))
	}
}

func (f *fakeComposeDocker) container(name string, index int) composeContainer {
	service := f.deployment.Services[name]
	var container composeContainer
	container.ID = fmt.Sprintf("%064x", index)
	container.Image = "sha256:" + strings.Repeat("c", 64)
	container.Config.Cmd = []string{"/synthetic-entrypoint"}
	container.Config.Labels = map[string]string{"com.docker.compose.project": f.cfg.ProjectName, "com.docker.compose.service": name, "com.docker.compose.oneoff": "False", "com.docker.compose.project.working_dir": f.cfg.ProjectDirectory}
	keys := make([]string, 0, len(service.Environment))
	for key := range service.Environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		container.Config.Env = append(container.Config.Env, key+"="+*service.Environment[key])
	}
	container.Config.Healthcheck.Test = append([]string{}, service.Healthcheck.Test...)
	if len(container.Config.Healthcheck.Test) == 0 {
		container.Config.Healthcheck.Test = append([]string{}, f.imageHealthcheck...)
	}
	container.State.Status, container.State.Running, container.State.Pid, container.State.Health.Status = "running", true, index+100, "healthy"
	for _, volume := range service.Volumes {
		container.Mounts = append(container.Mounts, struct {
			Type, Source, Destination string
			RW                        bool
		}{volume.Type, volume.Source, volume.Target, !volume.ReadOnly})
	}
	return container
}

func (f *fakeComposeDocker) run(ctx context.Context, cfg Config, args []string) ([]byte, error) {
	f.t.Helper()
	if _, ok := ctx.Deadline(); !ok {
		f.t.Fatal("Docker command had no bounded context")
	}
	if cfg.DockerPath != f.cfg.DockerPath || cfg.ProjectDirectory != f.cfg.ProjectDirectory {
		f.t.Fatal("Docker command lost explicit executable or project directory")
	}
	f.calls = append(f.calls, append([]string{}, args...))
	if len(args) == 0 {
		f.t.Fatal("empty Docker command")
	}
	if args[0] == "compose" {
		prefix := composePrefix(cfg)
		if len(args) <= len(prefix) || !reflect.DeepEqual(args[:len(prefix)], prefix) {
			f.t.Fatal("Compose command escaped explicit file/project/env arguments")
		}
		args = args[len(prefix):]
		if f.commandFailure == args[0] {
			return []byte("never-echo-compose-secret postgres://private-secret@host/database"), errors.New("never-echo-compose-secret")
		}
		switch args[0] {
		case "config":
			if f.onConfig != nil {
				callback := f.onConfig
				f.onConfig = nil
				callback()
			}
			return json.Marshal(f.deployment)
		case "stop":
			if !reflect.DeepEqual(args, append([]string{"stop", "--timeout", "30"}, composeServices(cfg.Mode)...)) {
				f.t.Fatal("stop widened fixed service set")
			}
			for i := range f.containers {
				if appContainer(cfg, f.containers[i]) && !f.leaveComposeRunning {
					f.stop(i)
				}
			}
			return nil, nil
		case "up":
			if !reflect.DeepEqual(args, append([]string{"up", "--detach", "--no-deps", "--force-recreate", "--no-build", "--pull", "never"}, composeServices(cfg.Mode)...)) {
				f.t.Fatal("recreate used start/build/pull/dependency/other-service operation")
			}
			for i := range f.containers {
				if appContainer(cfg, f.containers[i]) && !strings.EqualFold(f.containers[i].Config.Labels["com.docker.compose.oneoff"], "true") {
					id := f.containers[i].ID
					service := f.containers[i].Config.Labels["com.docker.compose.service"]
					f.containers[i] = f.container(service, i+201)
					if f.keepContainerIDs {
						f.containers[i].ID = id
					}
				}
			}
			return nil, nil
		case "run":
			return nil, nil
		case "start":
			if !reflect.DeepEqual(args, append([]string{"start"}, composeServices(cfg.Mode)...)) {
				f.t.Fatal("original start widened fixed services")
			}
			for i := range f.containers {
				if appContainer(cfg, f.containers[i]) && !strings.EqualFold(f.containers[i].Config.Labels["com.docker.compose.oneoff"], "true") {
					f.containers[i].State.Running = true
					f.containers[i].State.Status = "running"
					f.containers[i].State.Pid = 100 + i
					f.containers[i].State.Health.Status = "healthy"
				}
			}
			return nil, nil
		default:
			f.t.Fatal("unexpected Compose operation", args[0])
		}
	}
	if f.commandFailure == args[0] {
		return []byte("never-echo-compose-secret"), errors.New("never-echo-compose-secret")
	}
	switch args[0] {
	case "image":
		if len(args) != 6 || !reflect.DeepEqual(args[:5], []string{"image", "inspect", "--format", "{{json .}}", "--"}) {
			f.t.Fatal("image presence check escaped fixed argv")
		}
		known := false
		for _, service := range f.deployment.Services {
			if service.Image == args[5] {
				known = true
			}
		}
		if !known {
			f.t.Fatal("unplanned image inspected")
		}
		imageID := "sha256:" + strings.Repeat("c", 64)
		if f.imageChanged {
			imageID = "sha256:" + strings.Repeat("d", 64)
		}
		imageConfig := composeImageCommand{Cmd: []string{"/synthetic-entrypoint"}}
		imageConfig.Healthcheck.Test = append([]string{}, f.imageHealthcheck...)
		return json.Marshal(struct {
			ID     string              `json:"Id"`
			Config composeImageCommand `json:"Config"`
		}{imageID, imageConfig})
	case "cp":
		if len(args) != 3 || args[2] != "-" || !strings.HasSuffix(args[1], ":/var/spool/postfix") {
			f.t.Fatal("copy command was not the fixed spool read")
		}
		id := strings.TrimSuffix(args[1], ":/var/spool/postfix")
		found := false
		for _, container := range f.containers {
			if container.ID == id && container.Config.Labels["com.docker.compose.service"] == postfixService(cfg) && !container.State.Running {
				found = true
			}
		}
		if !found {
			f.t.Fatal("spool archive read an unverified or running container")
		}
		if data, ok := f.spoolArchives[id]; ok {
			return append([]byte{}, data...), nil
		}
		return emptyPostfixTar(f.t), nil
	case "ps":
		want := []string{"ps", "--all", "--quiet", "--no-trunc", "--filter", "label=com.docker.compose.project=" + cfg.ProjectName}
		if !reflect.DeepEqual(args, want) {
			f.t.Fatal("container enumeration lacked exact project label")
		}
		ids := []string{}
		for _, container := range f.containers {
			ids = append(ids, container.ID)
		}
		return []byte(strings.Join(ids, "\n")), nil
	case "inspect":
		if len(args) != len(f.containers)+3 || args[1] != "--type" || args[2] != "container" {
			f.t.Fatal("inspect did not use enumerated exact IDs")
		}
		for i, container := range f.containers {
			if args[i+3] != container.ID {
				f.t.Fatal("inspect changed enumerated container identity")
			}
		}
		return json.Marshal(f.containers)
	case "container":
		if len(args) < 5 || !reflect.DeepEqual(args[:4], []string{"container", "stop", "--time", "30"}) {
			f.t.Fatal("uncontrolled container operation")
		}
		for _, id := range args[4:] {
			found := false
			for i := range f.containers {
				if f.containers[i].ID == id {
					found = true
					if !appContainer(cfg, f.containers[i]) {
						f.t.Fatal("database/unrelated container stopped")
					}
					if !f.leaveSupplementalRunning {
						f.stop(i)
					}
				}
			}
			if !found {
				f.t.Fatal("unknown container ID stopped")
			}
		}
		return nil, nil
	case "exec":
		if len(args) < 3 {
			f.t.Fatal("empty health command")
		}
		for _, container := range f.containers {
			if container.ID == args[1] {
				if reflect.DeepEqual(args[2:], []string{"id", "-u", "postfix"}) {
					if f.postfixUID != "" {
						return []byte(f.postfixUID + "\n"), nil
					}
					return []byte("0\n"), nil
				}
				if reflect.DeepEqual(args[2:], []string{"id", "-g", "postfix"}) {
					if f.postfixGroups != "" {
						return []byte(f.postfixGroups + "\n"), nil
					}
					return []byte("0\n"), nil
				}
				if reflect.DeepEqual(args[2:], []string{"/usr/sbin/postconf", "-h", "queue_directory"}) && container.Config.Labels["com.docker.compose.service"] == postfixService(cfg) {
					if f.queueDirectory != "" {
						return []byte(f.queueDirectory + "\n"), nil
					}
					return []byte("/var/spool/postfix\n"), nil
				}
				for _, check := range composeRoleChecks(container.Config.Labels["com.docker.compose.service"]) {
					if reflect.DeepEqual(check, args[2:]) {
						return nil, nil
					}
				}
			}
		}
		f.t.Fatal("health command was not a fixed role check")
	default:
		f.t.Fatal("unknown Docker operation", args[0])
	}
	return nil, errCompose
}

func (f *fakeComposeDocker) stop(index int) {
	f.containers[index].State.Status = "exited"
	f.containers[index].State.Running = false
	f.containers[index].State.Restarting = false
	f.containers[index].State.Paused = false
	f.containers[index].State.Pid = 0
}

func TestComposeControllerFixedServicesStopRecreateAndRoleHealth(t *testing.T) {
	for _, mode := range []string{"all-in-one", "stack"} {
		t.Run(mode, func(t *testing.T) {
			cfg, controller, fake := newFakeComposeDocker(t, mode)
			if err := controller.Preflight(t.Context(), cfg); err != nil {
				t.Fatal("preflight", err)
			}
			if err := controller.VerifyStopped(t.Context(), cfg); err == nil {
				t.Fatal("running writers advertised stopped")
			}
			if err := controller.Stop(t.Context(), cfg); err != nil {
				t.Fatal("stop", err)
			}
			if err := controller.VerifyStopped(t.Context(), cfg); err != nil {
				t.Fatal("stopped", err)
			}
			for _, container := range fake.containers {
				if !appContainer(cfg, container) && !container.State.Running {
					t.Fatal("database service stopped")
				}
			}
			if err := controller.Recreate(t.Context(), cfg); err != nil {
				t.Fatal("recreate", err)
			}
			if err := controller.Healthy(t.Context(), cfg); err != nil {
				t.Fatal("healthy", err)
			}
			for _, call := range fake.calls {
				for _, value := range call {
					if value == "down" || value == "rm" || value == "prune" || value == "volume" || value == "start" {
						t.Fatal("controller issued forbidden deployment operation")
					}
				}
			}
		})
	}
}

func TestComposeControllerBindsExplicitAndInheritedImageHealthchecks(t *testing.T) {
	for _, mode := range []string{"all-in-one", "stack"} {
		for _, scenario := range []string{"inherited", "explicit-overrides-image", "missing", "image-none", "compose-none", "compose-disabled", "actual-healthcheck-changed"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				cfg, controller, fake := newFakeComposeDocker(t, mode)
				fake.imageHealthcheck = []string{"CMD", "synthetic-image-healthcheck"}
				for _, name := range composeServices(mode) {
					service := fake.deployment.Services[name]
					if scenario != "explicit-overrides-image" {
						service.Healthcheck.Test = nil
					}
					if scenario == "compose-none" {
						service.Healthcheck.Test = []string{"NONE"}
					}
					if scenario == "compose-disabled" {
						service.Healthcheck.Disable = true
					}
					fake.deployment.Services[name] = service
				}
				if scenario == "missing" {
					fake.imageHealthcheck = nil
				}
				if scenario == "image-none" {
					fake.imageHealthcheck = []string{"NONE"}
				}
				for i, container := range fake.containers {
					service := container.Config.Labels["com.docker.compose.service"]
					if !appContainer(cfg, container) {
						continue
					}
					fake.containers[i] = fake.container(service, i+1)
					if scenario == "actual-healthcheck-changed" {
						fake.containers[i].Config.Healthcheck.Test = []string{"CMD", "unapproved-healthcheck"}
					}
				}
				err := controller.Preflight(t.Context(), cfg)
				accepted := scenario == "inherited" || scenario == "explicit-overrides-image"
				if (err == nil) != accepted {
					t.Fatal("unexpected healthcheck admission", err)
				}
				if accepted {
					if err := controller.Healthy(t.Context(), cfg); err != nil {
						t.Fatal("healthy deployment rejected", err)
					}
				}
				for _, call := range fake.calls {
					for _, value := range call {
						if value == "stop" || value == "start" || value == "up" {
							t.Fatal("read-only healthcheck admission controlled services")
						}
					}
				}
			})
		}
	}
}

func TestComposeControllerRefusesUnprovenConfigContainersAndBindings(t *testing.T) {
	for _, scenario := range []string{"file-changed", "unknown-service", "missing-service", "wrong-config-mount", "wrong-read-only", "shadow-mount", "shadow-postfix-config", "command-override", "entrypoint-override", "actual-command-override", "custom-mail-config", "custom-queue-directory", "wrong-actual-mount", "wrong-project", "wrong-working-directory", "missing-working-directory", "database-wrong-working-directory", "wrong-service", "unknown-state", "missing-health", "stale-env", "image-db-path", "image-missing", "image-changed", "replicas", "unlisted-env-file", "render-changed", "environment-during-render"} {
		t.Run(scenario, func(t *testing.T) {
			cfg, controller, fake := newFakeComposeDocker(t, "stack")
			var containerIndex int
			for i, container := range fake.containers {
				if container.Config.Labels["com.docker.compose.service"] == "api" {
					containerIndex = i
				}
			}
			switch scenario {
			case "file-changed":
				if err := os.WriteFile(cfg.ComposeFiles[0].Path, []byte("changed trusted file"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unknown-service":
				fake.deployment.Services["unexpected-writer"] = composeService{}
			case "missing-service":
				delete(fake.deployment.Services, "postfix")
			case "wrong-config-mount":
				service := fake.deployment.Services["api"]
				service.Volumes[0].Source = cfg.Targets.Mail
				fake.deployment.Services["api"] = service
			case "wrong-read-only":
				service := fake.deployment.Services["api"]
				service.Volumes[1].ReadOnly = false
				fake.deployment.Services["api"] = service
			case "shadow-mount":
				service := fake.deployment.Services["api"]
				service.Volumes = append(service.Volumes, composeVolume{Type: "bind", Source: cfg.ProjectDirectory, Target: cfg.RuntimeDataRoot + "/shadow"})
				fake.deployment.Services["api"] = service
			case "shadow-postfix-config":
				service := fake.deployment.Services["postfix"]
				service.Volumes = append(service.Volumes, composeVolume{Type: "bind", Source: cfg.ProjectDirectory, Target: "/etc/postfix/main.cf", ReadOnly: true})
				fake.deployment.Services["postfix"] = service
			case "command-override":
				service := fake.deployment.Services["postfix"]
				service.Command = json.RawMessage(`["postfix","-c","/custom","start"]`)
				fake.deployment.Services["postfix"] = service
			case "entrypoint-override":
				service := fake.deployment.Services["postfix"]
				service.Entrypoint = json.RawMessage(`["/custom-entrypoint"]`)
				fake.deployment.Services["postfix"] = service
			case "actual-command-override":
				fake.containers[containerIndex].Config.Cmd = []string{"/custom-entrypoint"}
			case "custom-mail-config":
				service := fake.deployment.Services["postfix"]
				service.Environment["MAIL_CONFIG"] = composeString("/custom")
				fake.deployment.Services["postfix"] = service
			case "custom-queue-directory":
				fake.queueDirectory = "/custom-spool"
			case "wrong-actual-mount":
				fake.containers[containerIndex].Mounts[0].Source = cfg.Targets.Mail
			case "wrong-project":
				fake.containers[containerIndex].Config.Labels["com.docker.compose.project"] = "other-project"
			case "wrong-working-directory":
				fake.containers[containerIndex].Config.Labels["com.docker.compose.project.working_dir"] = cfg.StateDir
			case "missing-working-directory":
				delete(fake.containers[containerIndex].Config.Labels, "com.docker.compose.project.working_dir")
			case "database-wrong-working-directory":
				for i, container := range fake.containers {
					if container.Config.Labels["com.docker.compose.service"] == "postgres" {
						fake.containers[i].Config.Labels["com.docker.compose.project.working_dir"] = cfg.StateDir
					}
				}
			case "wrong-service":
				fake.containers[containerIndex].Config.Labels["com.docker.compose.service"] = "other-service"
			case "unknown-state":
				fake.containers[containerIndex].State.Status = "unknown"
			case "missing-health":
				service := fake.deployment.Services["api"]
				service.Healthcheck.Test = nil
				fake.deployment.Services["api"] = service
			case "stale-env":
				fake.containers[containerIndex].Config.Env = []string{"LANQIN_DB_DRIVER=sqlite"}
			case "image-db-path":
				service := fake.deployment.Services["api"]
				delete(service.Environment, "LANQIN_DB_PATH")
				fake.deployment.Services["api"] = service
				actual := []string{}
				for _, value := range fake.containers[containerIndex].Config.Env {
					if !strings.HasPrefix(value, "LANQIN_DB_PATH=") {
						actual = append(actual, value)
					}
				}
				fake.containers[containerIndex].Config.Env = append(actual, "LANQIN_DB_PATH=/unrelated/image-default.db")
			case "image-missing":
				fake.commandFailure = "image"
			case "image-changed":
				if err := controller.Preflight(t.Context(), cfg); err != nil {
					t.Fatal(err)
				}
				fake.imageChanged = true
			case "replicas":
				service := fake.deployment.Services["api"]
				service.Scale = 2
				fake.deployment.Services["api"] = service
			case "unlisted-env-file":
				service := fake.deployment.Services["api"]
				service.EnvFile = json.RawMessage(`["/unlisted/secret.env"]`)
				fake.deployment.Services["api"] = service
			case "render-changed":
				if err := controller.Preflight(t.Context(), cfg); err != nil {
					t.Fatal(err)
				}
				service := fake.deployment.Services["web"]
				service.Environment["UNPINNED_INPUT"] = composeString("changed")
				fake.deployment.Services["web"] = service
			case "environment-during-render":
				fake.onConfig = func() {
					if err := os.WriteFile(cfg.EnvironmentFile.Path, []byte("unexpected environment"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			err := controller.Preflight(t.Context(), cfg)
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatal("unsafe/unproven deployment accepted or echoed", scenario, err)
			}
			for _, call := range fake.calls {
				if len(call) > len(composePrefix(cfg)) && call[0] == "compose" && call[len(composePrefix(cfg))] == "stop" {
					t.Fatal("failed preflight stopped services")
				}
			}
		})
	}
}

func TestComposeControllerFindsAndStopsRemainingOrphanWriterIDs(t *testing.T) {
	cfg, controller, fake := newFakeComposeDocker(t, "all-in-one")
	fake.leaveComposeRunning = true
	orphan := fake.container("lanqin-email", 99)
	orphan.Config.Labels["com.docker.compose.oneoff"] = "True"
	fake.containers = append(fake.containers, orphan)
	replica := fake.container("lanqin-email", 98)
	fake.containers = append(fake.containers, replica)
	if err := controller.Stop(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := controller.VerifyStopped(t.Context(), cfg); err != nil {
		t.Fatal("remaining fixed-service writer was ignored", err)
	}
	found, foundReplica := false, false
	for _, call := range fake.calls {
		if len(call) > 4 && call[0] == "container" {
			for _, id := range call[4:] {
				if id == orphan.ID {
					found = true
				}
				if id == replica.ID {
					foundReplica = true
				}
			}
		}
	}
	if !found || !foundReplica {
		t.Fatal("extra project/service writer not included in supplemental stop")
	}
	fake.containers[len(fake.containers)-1].State.Running = true
	fake.containers[len(fake.containers)-1].State.Status = "running"
	fake.containers[len(fake.containers)-1].State.Pid = 999
	if err := controller.VerifyStopped(t.Context(), cfg); err == nil {
		t.Fatal("post-stop new orphan writer was not detected")
	}
}

func TestComposeControllerRefusesUnchangedContainerIDAndMissingHealth(t *testing.T) {
	cfg, controller, fake := newFakeComposeDocker(t, "all-in-one")
	if err := controller.Preflight(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := controller.Stop(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	fake.keepContainerIDs = true
	if err := controller.Recreate(t.Context(), cfg); err == nil {
		t.Fatal("old bind-mount container accepted after host directory rename")
	}
	for i := range fake.containers {
		if appContainer(cfg, fake.containers[i]) {
			fake.containers[i].State.Health.Status = "starting"
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if err := controller.Healthy(ctx, cfg); err == nil {
		t.Fatal("running but unready services accepted as healthy")
	}
}

func TestComposeControllerErrorsNeverEchoDockerEnvironmentOrDSN(t *testing.T) {
	cfg, controller, fake := newFakeComposeDocker(t, "all-in-one")
	fake.commandFailure = "config"
	if err := controller.Preflight(t.Context(), cfg); err == nil || strings.Contains(err.Error(), "never-echo") || strings.Contains(err.Error(), "postgres://") {
		t.Fatal("Docker provider output escaped into safe controller error")
	}
	var output composeOutput
	output.limit = 4
	if _, err := output.Write([]byte("private-overflow")); err == nil || output.Len() != 0 {
		t.Fatal("command output was not bounded before retaining private bytes")
	}
}

func TestComposeControllerNativeRuntimeUsesNewIdentityAndReadOnlyOneoff(t *testing.T) {
	for _, mode := range []string{"all-in-one", "stack"} {
		t.Run(mode, func(t *testing.T) {
			cfg := environmentConfig(t)
			cfg.Mode = mode
			fake := &fakeComposeDocker{t: t, cfg: cfg}
			fake.setDeployment("postgres", composeDatabaseIdentity{"database", "5432", "old", "old", "synthetic-old-secret"})
			controller := fake.controller()
			if err := controller.Preflight(t.Context(), cfg); err != nil {
				t.Fatal("old native runtime rejected", err)
			}
			if err := controller.Stop(t.Context(), cfg); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cfg.EnvironmentFile.Path, []byte(nextEnvironmentFixture), 0o600); err != nil {
				t.Fatal(err)
			}
			before := append([]composeContainer{}, fake.containers...)
			fake.setDeployment("postgres", composeDatabaseIdentity{"database", "5432", "new", "new", "synthetic-next-secret"})
			fake.containers = before
			fakeComposePhase(t, cfg, "installed", "validate")
			planSHA := strings.Repeat("b", 64)
			if err := controller.ValidateNativeRuntime(t.Context(), cfg, planSHA); err != nil {
				t.Fatal("new container DB/file identity verification failed", err)
			}
			seen := false
			for _, call := range fake.calls {
				if len(call) > len(composePrefix(cfg)) && call[0] == "compose" && call[len(composePrefix(cfg))] == "run" {
					seen = true
					arguments := strings.Join(call, " ")
					for _, required := range []string{"--rm", "--no-deps", "--no-TTY", "--pull never", "--entrypoint /usr/local/bin/lanqin-backup", "verify-native --driver postgres --dsn-env LANQIN_DATABASE_URL", "--expected-plan-sha256 " + planSHA} {
						if !strings.Contains(arguments, required) {
							t.Fatal("native helper lost fixed readonly argv")
						}
					}
					if strings.Contains(arguments, "--service-ports") || strings.Contains(arguments, "synthetic-next-secret") {
						t.Fatal("native helper published ports or exposed credentials")
					}
				}
			}
			if !seen {
				t.Fatal("native installed DB/tree never verified from actual container namespace")
			}
			if err := controller.Recreate(t.Context(), cfg); err != nil {
				t.Fatal("new native environment not used for recreated application services", err)
			}
			if err := controller.Healthy(t.Context(), cfg); err != nil {
				t.Fatal("new native application roles not healthy", err)
			}
			cfg.RuntimeDatabase.Database = "wrong"
			if err := controller.ValidateNativeRuntime(t.Context(), cfg, planSHA); err == nil {
				t.Fatal("wrong new runtime DB identity accepted")
			}
		})
	}
}

func TestComposeNativeRuntimeRejectsCrossServiceOrTargetDatabaseDrift(t *testing.T) {
	for _, driver := range []string{"postgres", "mysql"} {
		for _, mode := range []string{"all-in-one", "stack"} {
			for _, damage := range []string{"password", "mail-host", "target", "driver", "dsn-override", "search-path", "role", "session-authorization"} {
				if driver == "mysql" && (damage == "search-path" || damage == "role" || damage == "session-authorization") {
					continue
				}
				t.Run(driver+"/"+mode+"/"+damage, func(t *testing.T) {
					cfg := environmentConfig(t)
					cfg.Driver, cfg.Mode = driver, mode
					port := "5432"
					if driver == "mysql" {
						port = "3306"
						cfg.RuntimeDatabase.Port = 3306
					}
					fake := &fakeComposeDocker{t: t, cfg: cfg}
					fake.setDeployment(driver, composeDatabaseIdentity{"database", port, "new", "new", "synthetic-next-secret"})
					if err := validateComposeRuntime(cfg, fake.deployment, true); err != nil {
						t.Fatal("valid explicit target identity rejected", err)
					}
					role, api := "postfix", "api"
					if mode == "all-in-one" {
						role, api = "lanqin-email", "lanqin-email"
					}
					service := fake.deployment.Services[role]
					switch damage {
					case "password":
						service.Environment["LANQIN_DB_PASSWORD"] = composeString("other-synthetic-secret")
					case "mail-host":
						service.Environment["LANQIN_DB_HOST"] = composeString("other-database")
					case "target":
						cfg.RuntimeDatabase.Database = "not-installed-target"
					case "driver":
						service.Environment["LANQIN_DB_DRIVER"] = composeString("sqlite")
					case "dsn-override":
						service = fake.deployment.Services[api]
						if driver == "postgres" {
							service.Environment["LANQIN_DATABASE_URL"] = composeString("postgres://new:synthetic-next-secret@database:5432/new?host=unrelated")
						} else {
							service.Environment["LANQIN_DATABASE_URL"] = composeString("new:synthetic-next-secret@tcp(unrelated:3306)/new")
						}
						role = api
					case "search-path", "role", "session-authorization":
						key := map[string]string{"search-path": "search_path", "role": "role", "session-authorization": "session_authorization"}[damage]
						service = fake.deployment.Services[api]
						service.Environment["LANQIN_DATABASE_URL"] = composeString("postgres://new:synthetic-next-secret@database:5432/new?sslmode=disable&" + key + "=unrelated")
						role = api
					}
					fake.deployment.Services[role] = service
					if err := validateComposeRuntime(cfg, fake.deployment, true); err == nil {
						t.Fatal("unproven native API/mail/target identity accepted")
					}
				})
			}
		}
	}
}

func TestComposeNativeRuntimeRejectsUnquotedLibpqCredentialEscapes(t *testing.T) {
	for _, password := range []string{`ab\cd`, `'ab'`} {
		t.Run(password, func(t *testing.T) {
			cfg := environmentConfig(t)
			cfg.Mode = "stack"
			fake := &fakeComposeDocker{t: t, cfg: cfg}
			fake.setDeployment("postgres", composeDatabaseIdentity{"database", "5432", "new", "new", "synthetic-next-secret"})
			// Both environments contain exactly the same password. The API URL
			// preserves it, while Dovecot's unquoted keyword string would strip
			// backslash escapes or enclosing quotes. Do not claim those match.
			for _, role := range []string{"api", "postfix", "dovecot", "rspamd"} {
				service := fake.deployment.Services[role]
				service.Environment["LANQIN_DB_PASSWORD"] = composeString(password)
				if role == "api" {
					dsn := (&url.URL{Scheme: "postgres", Host: "database:5432", Path: "/new", User: url.UserPassword("new", password), RawQuery: "sslmode=disable"}).String()
					service.Environment["LANQIN_DATABASE_URL"] = composeString(dsn)
				}
				fake.deployment.Services[role] = service
			}
			if err := validateComposeRuntime(cfg, fake.deployment, true); err == nil {
				t.Fatal("API URL and unquoted mail-service credentials could be interpreted differently")
			}
		})
	}
}
