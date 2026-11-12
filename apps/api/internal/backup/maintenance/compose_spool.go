package maintenance

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxPostfixArchiveBytes = 64 << 20
const maxPostfixArchiveEntries = 100000
const maxPostfixCaptureBytes = 256 << 20

var ErrPostfixQueue = errors.New("Postfix queue is not proven empty; retain the stopped containers and their queued mail")

func composeCommandOutputLimit(args []string) int {
	if len(args) == 3 && args[0] == "cp" && args[2] == "-" && strings.HasSuffix(args[1], ":/var/spool/postfix") && composeContainerID.MatchString(strings.TrimSuffix(args[1], ":/var/spool/postfix")) {
		return maxPostfixArchiveBytes
	}
	return 8 << 20
}

func postfixContainerIDs(cfg Config, containers []composeContainer) []string {
	ids := []string{}
	for _, container := range containers {
		if container.Config.Labels["com.docker.compose.service"] == postfixService(cfg) {
			ids = append(ids, container.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// EnsureEmptyPostfixQueues never extracts or prints the Docker copy tar. The
// queue's writable container layer is a separate data source from Maildir.
func (c *ComposeController) EnsureEmptyPostfixQueues(ctx context.Context, cfg Config) error {
	_, _, err := c.inspectPostfixArchives(ctx, cfg, true)
	return err
}

type postfixArchive struct {
	ID   string
	Data []byte
}

func clearPostfixArchives(archives []postfixArchive) {
	for _, archive := range archives {
		clear(archive.Data)
	}
}

func postfixRuntimeBinding(cfg Config, containers []composeContainer, environment string) ([]byte, error) {
	type containerBinding struct{ ID, Service, Image string }
	bindings := []containerBinding{}
	for _, container := range containers {
		if appContainer(cfg, container) {
			bindings = append(bindings, containerBinding{container.ID, container.Config.Labels["com.docker.compose.service"], container.Image})
		}
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].ID < bindings[j].ID })
	return json.Marshal(struct {
		EnvironmentSHA256 string             `json:"environmentSHA256"`
		Containers        []containerBinding `json:"containers"`
	}{environment, bindings})
}

// inspectPostfixArchives never extracts the Docker tar. Capture mode permits
// ordinary queued files, while retaining all container, layout and size checks.
func (c *ComposeController) inspectPostfixArchives(ctx context.Context, cfg Config, empty bool) ([]postfixArchive, []byte, error) {
	collected := []postfixArchive{}
	success := false
	defer func() {
		if !success {
			clearPostfixArchives(collected)
		}
	}()
	if err := c.VerifyStopped(ctx, cfg); err != nil {
		return nil, nil, ErrPostfixQueue
	}
	baseline, err := readComposeOriginal(cfg)
	if err != nil {
		return nil, nil, ErrPostfixQueue
	}
	deployment, err := c.metadataDeployment(ctx, cfg)
	if err != nil {
		return nil, nil, ErrPostfixQueue
	}
	containers, err := c.inventory(ctx, cfg, deployment, false)
	if err != nil || !stoppedComposeContainers(cfg, containers) {
		return nil, nil, ErrPostfixQueue
	}
	environment, _, err := composeEnvironmentVersion(cfg)
	if err != nil {
		return nil, nil, ErrPostfixQueue
	}
	binding, err := postfixRuntimeBinding(cfg, containers, environment)
	if err != nil {
		return nil, nil, ErrPostfixQueue
	}
	ids := postfixContainerIDs(cfg, containers)
	if len(ids) == 0 {
		return nil, nil, ErrPostfixQueue
	}
	var total int64
	for _, container := range containers {
		if container.Config.Labels["com.docker.compose.service"] != postfixService(cfg) {
			continue
		}
		if container.Image != baseline.PostfixImages[postfixService(cfg)] {
			return nil, nil, ErrPostfixQueue
		}
		environment, err := containerEnvironment(container.Config.Env)
		if err != nil || environment["MAIL_CONFIG"] != "" && environment["MAIL_CONFIG"] != "/etc/postfix" {
			return nil, nil, ErrPostfixQueue
		}
		if strings.EqualFold(container.Config.Labels["com.docker.compose.oneoff"], "true") && (container.Config.Labels["lanqin.backup.maintenance"] != "verify-native" || len(container.Config.Entrypoint) != 1 || container.Config.Entrypoint[0] != "/usr/local/bin/lanqin-backup" || len(container.Config.Cmd) == 0 || container.Config.Cmd[0] != "verify-native") {
			return nil, nil, ErrPostfixQueue
		}
		archive, err := c.invoke(ctx, cfg, []string{"cp", container.ID + ":/var/spool/postfix", "-"})
		if err != nil {
			return nil, nil, ErrPostfixQueue
		}
		validationErr := validatePostfixArchive(ctx, archive, empty)
		total += int64(len(archive))
		if validationErr == nil && !empty && total <= maxPostfixCaptureBytes {
			collected = append(collected, postfixArchive{container.ID, archive})
		} else {
			clear(archive)
		}
		if validationErr != nil || !empty && total > maxPostfixCaptureBytes {
			return nil, nil, ErrPostfixQueue
		}
	}
	if err := c.VerifyStopped(ctx, cfg); err != nil {
		return nil, nil, ErrPostfixQueue
	}
	after, err := c.inventory(ctx, cfg, deployment, false)
	if err != nil {
		return nil, nil, ErrPostfixQueue
	}
	afterEnvironment, _, err := composeEnvironmentVersion(cfg)
	afterBinding, bindErr := postfixRuntimeBinding(cfg, after, afterEnvironment)
	if err != nil || bindErr != nil || !bytes.Equal(binding, afterBinding) || !stoppedComposeContainers(cfg, after) {
		return nil, nil, ErrPostfixQueue
	}
	afterIDs := postfixContainerIDs(cfg, after)
	if len(ids) != len(afterIDs) {
		return nil, nil, ErrPostfixQueue
	}
	for i, id := range ids {
		if id != afterIDs[i] {
			return nil, nil, ErrPostfixQueue
		}
	}
	sort.Slice(collected, func(i, j int) bool { return collected[i].ID < collected[j].ID })
	success = true
	return collected, binding, nil
}

func validateEmptyPostfixArchive(ctx context.Context, data []byte) error {
	return validatePostfixArchive(ctx, data, true)
}

func validatePostfixArchive(ctx context.Context, data []byte, empty bool) error {
	if len(data) < 1024 || len(data) > maxPostfixArchiveBytes || len(data)%512 != 0 {
		return ErrPostfixQueue
	}
	reader := bytes.NewReader(data)
	archive := tar.NewReader(reader)
	queues := map[string]bool{"active": true, "deferred": true, "incoming": true, "hold": true, "maildrop": true, "corrupt": true, "bounce": true, "defer": true, "trace": true, "flush": true, "saved": true}
	otherRoots := map[string]bool{"dev": true, "etc": true, "lib": true, "lib64": true, "pid": true, "private": true, "public": true, "usr": true}
	seen := map[string]bool{}
	entries := 0
	for {
		if ctx.Err() != nil {
			return ErrPostfixQueue
		}
		before := reader.Len()
		header, err := archive.Next()
		if err == io.EOF {
			if before-reader.Len() < 1024 {
				return ErrPostfixQueue
			}
			for _, value := range data[len(data)-reader.Len():] {
				if value != 0 {
					return ErrPostfixQueue
				}
			}
			break
		}
		if err != nil {
			return ErrPostfixQueue
		}
		entries++
		if entries > maxPostfixArchiveEntries {
			return ErrPostfixQueue
		}
		name := strings.TrimSuffix(header.Name, "/")
		if !utf8.ValidString(name) || len(name) > 4096 || !fs.ValidPath(name) || path.Clean(name) != name || strings.ContainsAny(name, "\\:\x00") || strings.IndexFunc(name, unicode.IsControl) >= 0 || seen[name] || header.Size < 0 || header.Size > maxPostfixArchiveBytes {
			return ErrPostfixQueue
		}
		for key := range header.PAXRecords {
			if key != "path" && key != "mtime" && key != "atime" && key != "ctime" && !strings.HasPrefix(key, "SCHILY.xattr.") {
				return ErrPostfixQueue
			}
		}
		seen[name] = true
		parts := strings.Split(name, "/")
		if parts[0] != "postfix" || len(parts) > 64 {
			return ErrPostfixQueue
		}
		if len(parts) == 1 {
			if header.Typeflag != tar.TypeDir || header.Size != 0 {
				return ErrPostfixQueue
			}
			continue
		}
		if !seen["postfix"] {
			return ErrPostfixQueue
		}
		// Postfix's master leaves this empty restart marker in an otherwise
		// empty stopped spool. It carries no queued mail. Do not generalize this
		// exception to other root files, links, or nonempty marker payloads.
		if name == "postfix/restart" {
			if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA || header.Size != 0 || header.Linkname != "" {
				return ErrPostfixQueue
			}
			continue
		}
		if queues[parts[1]] {
			if header.Typeflag != tar.TypeDir && (empty || len(parts) == 2 || header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA) || header.Typeflag == tar.TypeDir && header.Size != 0 || header.Linkname != "" {
				return ErrPostfixQueue
			}
			if _, err := io.Copy(io.Discard, &postfixArchiveReader{ctx: ctx, reader: archive}); err != nil {
				return ErrPostfixQueue
			}
		} else {
			if !otherRoots[parts[1]] || len(parts) == 2 && header.Typeflag != tar.TypeDir {
				return ErrPostfixQueue
			}
			if header.Typeflag != tar.TypeDir && header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
				return ErrPostfixQueue
			}
			if header.Typeflag == tar.TypeDir && header.Size != 0 {
				return ErrPostfixQueue
			}
			if _, err := io.Copy(io.Discard, &postfixArchiveReader{ctx: ctx, reader: archive}); err != nil {
				return ErrPostfixQueue
			}
		}
	}
	for _, required := range []string{"postfix", "postfix/active", "postfix/deferred", "postfix/incoming", "postfix/hold", "postfix/maildrop", "postfix/corrupt"} {
		if !seen[required] {
			return ErrPostfixQueue
		}
	}
	return nil
}

type postfixArchiveReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *postfixArchiveReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}
