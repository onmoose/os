package lifecycle

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/onmoose/os/internal/caddy"
	"github.com/onmoose/os/internal/protocol"
)

// DockerDriver is the narrow surface lifecycle needs from Docker. Production
// is a thin `docker`/`docker compose` CLI wrapper; tests use a recording fake
// (see internal/lifecycle/lifecycletest). Extracting this is the enabling
// refactor for the test plan in docs/dev/testing-brain.md.
type DockerDriver interface {
	Pull(ctx context.Context, image string) error
	ImageInspect(ctx context.Context, image string) (RepoDigests, error)
	ComposeUp(ctx context.Context, dir, project string) (string, error)
	ComposeDown(ctx context.Context, dir, project string) (string, error)
	ComposeStop(ctx context.Context, dir, project string) (string, error)
	// ServiceUp brings up a managed-service compose project
	// (SERVICE_PROVISIONING.md # Tier 1). Unlike ComposeUp it uses only the
	// generated compose.yml + .env (no per-app override): a service instance is a
	// brain-owned container, not a user app.
	ServiceUp(ctx context.Context, dir, project string) (string, error)
	// ControlPlaneUp brings up the control-plane stack (Caddy + moose-ui) from a
	// single compose.yml in dir (CONTROL_PLANE.md # Caddy is moose substrate; #
	// the dashboard UI is a brain-launched container). Like ServiceUp it has no
	// per-app override and no .env — the control-plane compose is a fixed,
	// brain-owned project, not a user app. The docker-socket-proxy is *not* part
	// of this project: it is host-agent-seeded transport (the brain cannot bring
	// up its own sole Docker path), launched separately before the brain.
	ControlPlaneUp(ctx context.Context, dir, project string) (string, error)
	// RunOneOff runs an ephemeral client container against a managed service
	// (`docker run --rm --network <network> --env-file <envFile> <image> <args…>`)
	// and returns combined output. The brain uses it to provision per-app
	// databases/roles by running the service's own client (e.g. psql) in a
	// throwaway container joined to the service network — the *container* joins
	// it, never the brain (DECISIONS.md 2026-06-02 — control plane off
	// app-reachable networks). It replaces `docker exec`, which the
	// docker-socket-proxy denies (CONTROL_PLANE.md # Docker socket exposure;
	// DECISIONS.md 2026-06-14). --env-file carries the service superuser password
	// out of host argv; --rm removes the container on exit.
	RunOneOff(ctx context.Context, image, network, envFile string, args []string) (string, error)
	// ContainerHealth reads a container's healthcheck status by name
	// (`docker inspect -f {{.State.Health.Status}}`): "healthy", "starting",
	// "unhealthy", or "none" when the container declares no healthcheck. Used to
	// poll a managed service to readiness without an in-container exec — the
	// service compose already defines the per-engine healthcheck.
	ContainerHealth(ctx context.Context, container string) (string, error)
	Inspect(ctx context.Context, instanceID, mainService string) (running bool, health string, err error)
	NetworkCreate(ctx context.Context, name string, internal bool) error
	NetworkRemove(ctx context.Context, name string) error
	PSManaged(ctx context.Context) (map[string]bool, error)
	// RestartCounts reports each managed instance's cumulative container
	// RestartCount (max across the instance's containers), keyed by
	// instance_id. Used by the brain's container-restart-loop detector to
	// sample restart deltas over a window (HEALTH.md # Detector catalog, locus
	// D). Read-only; needs only the proxy's CONTAINERS endpoint family.
	RestartCounts(ctx context.Context) (map[string]int, error)
	// ManagedContainers reports every managed container's owning instance_id,
	// compose service, running state, and StartedAt. Used by the brain's
	// app-unresponsive probe to gate on the main service's steady-running state
	// and start-period grace (HEALTH.md # Detector catalog, locus C). Read-only;
	// needs only the proxy's CONTAINERS endpoint family.
	ManagedContainers(ctx context.Context) ([]ManagedContainer, error)
	// RemoveContainersByInstance is the orphan-teardown escape hatch: when the
	// instance dir is gone, compose can't drive the cleanup, so we kill all
	// containers labeled moose.instance_id=<id> directly.
	RemoveContainersByInstance(ctx context.Context, instanceID string) error
	// RemoveImage removes one locally-stored image by its pinned `repo@sha256:…`
	// reference (APP_LIFECYCLE.md # stop, start, uninstall — uninstall-time image
	// reclaim). Un-forced: if the image is still referenced (another tag, a
	// stopped container), docker refuses and the caller treats it as best-effort.
	RemoveImage(ctx context.Context, ref string) error
	// UsernsRemap reports whether the daemon runs with userns-remap: `docker
	// info` lists "name=userns" in its security options. The brain checks it
	// against host-agent's remap_base before every install (APP_ISOLATION.md
	// # User-namespace tiers). Needs only the proxy's INFO endpoint.
	UsernsRemap(ctx context.Context) (bool, error)
	// ImageUser returns the user a local image sets (its Config.User, the
	// Dockerfile USER), "" when it sets none. The image tier reads it after the
	// pull (APP_ISOLATION.md # User-namespace tiers).
	ImageUser(ctx context.Context, ref string) (string, error)
	// ImageUserFiles returns a local image's /etc/passwd and /etc/group, nil
	// for one that is missing or is not a regular file. It creates a container
	// from the image without starting it, copies the two files out of it and
	// removes it with its anonymous volumes, so no code from the image runs.
	// The container is removed on every path, the error ones too. It carries the moose.image_user_probe and
	// moose.instance_id labels, so one left by a brain that stopped half way
	// can be found and removed.
	ImageUserFiles(ctx context.Context, instanceID, ref string) (passwd, group []byte, err error)
}

// ManagedContainer is one managed container's identity and liveness, as read
// from `docker inspect`. Service is the compose service name
// (com.docker.compose.service label); StartedAt is zero for a never-started
// container.
type ManagedContainer struct {
	InstanceID string
	Service    string
	Running    bool
	StartedAt  time.Time
}

// RepoDigests is the `RepoDigests` field of `docker image inspect`: a list of
// `repo@sha256:…` strings the local image is known under.
type RepoDigests []string

// CaddyDriver is the slice of caddy.Client that lifecycle uses. AddRoute takes a
// caddy.RouteConfig (a concrete value type the provider exports) so lifecycle can
// pass the resolved forward-auth + Cookie-strip policy it builds (#306).
type CaddyDriver interface {
	EnsureServer(ctx context.Context) error
	AddRoute(ctx context.Context, cfg caddy.RouteConfig) error
	AddSplashRoute(ctx context.Context, instanceID, host, appName, state string) error
	RemoveRoute(ctx context.Context, instanceID string) error
}

// HostDriver is the slice of hostclient.Client that lifecycle uses.
type HostDriver interface {
	Publish(ctx context.Context, slug string) (protocol.PublishResponse, error)
	Unpublish(ctx context.Context, slug string) error
	// ResolveHome returns the owner's home directory path, UID, and GID. Used
	// by writeOverride to build bind-mount sources and the user: directive for
	// personal-scope app instances.
	ResolveHome(ctx context.Context, user string) (protocol.ResolveHomeResponse, error)
	// PrepareUserFolder makes sure a personal folder source, <home>/<rel>,
	// exists and is owned by the user before compose up (#519). The brain
	// cannot do it: its container does not mount /home, so host-agent does it
	// as a narrow named op (CONTROL_PLANE.md # Locked: host-agent hardening
	// directives). rel starts with the capitalized folder, e.g. "Documents/Work".
	PrepareUserFolder(ctx context.Context, user, rel string) error
	// WellKnownIdentity returns the fixed host service identities: the moose-app
	// service UID/GID a household instance runs as, and the moose-shared GID a
	// shared-source folder mount joins via group_add.
	WellKnownIdentity(ctx context.Context) (protocol.WellKnownIdentityResponse, error)
	// AllocateAppServiceIdentity reserves a dedicated UID/GID pair from the
	// host's app-service band for a folderless `service_user: true` instance
	// (APP_ISOLATION.md # Runtime identity & data ownership). Called once at
	// install; the pair is persisted on the instance row and pinned as the
	// compose user:. ReleaseAppServiceIdentity returns it to the band at
	// uninstall / install rollback (idempotent on the host side).
	AllocateAppServiceIdentity(ctx context.Context, instanceID string) (protocol.AllocateAppServiceIdentityResponse, error)
	ReleaseAppServiceIdentity(ctx context.Context, uid int) error
	// SystemStatus carries the data drive's free/total bytes (statfs snapshot)
	// the install-plan footprint reports as free_bytes (BRAIN_UI_PROTOCOL.md #
	// install-plan). Read-only; advisory, so InstallFootprint swallows its error.
	SystemStatus(ctx context.Context) (protocol.SystemStatus, error)
	// SystemGPU is the host GPU capability report (presence + vendor + render
	// group GID) behind the `gpu: true` install path: Present false is the
	// hard capacity refusal (ErrNoGPU) raised before any Docker work, and
	// RenderGID feeds the override's /dev/dri group_add (APP_ISOLATION.md #
	// GPU). Queried only when the manifest declares gpu: true.
	SystemGPU(ctx context.Context) (protocol.SystemGPU, error)
}

// Admitter validates a verbatim compose. Default impl is admission.Check; tests
// inject admission.CheckStructure to skip the `docker compose config -q`
// syntax pass that needs a real daemon.
type Admitter func(ctx context.Context, composeBytes []byte) error

// NewCLIDocker returns the production driver: shells out to `docker` and
// `docker compose` exactly as the previous inline calls did.
func NewCLIDocker() DockerDriver { return cliDocker{} }

type cliDocker struct{}

func (cliDocker) Pull(ctx context.Context, image string) error {
	if out, err := exec.CommandContext(ctx, "docker", "pull", image).CombinedOutput(); err != nil {
		return fmt.Errorf("pull %s: %w\n%s", image, err, out)
	}
	return nil
}

func (cliDocker) ImageInspect(ctx context.Context, image string) (RepoDigests, error) {
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect",
		"--format", "{{json .RepoDigests}}", image).Output()
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", image, err)
	}
	var rd RepoDigests
	if err := json.Unmarshal(out, &rd); err != nil {
		return nil, fmt.Errorf("parse RepoDigests for %s: %w", image, err)
	}
	return rd, nil
}

func (cliDocker) ComposeUp(ctx context.Context, dir, project string) (string, error) {
	return composeRun(ctx, dir, project, "up", "-d")
}

func (cliDocker) ComposeDown(ctx context.Context, dir, project string) (string, error) {
	return composeRun(ctx, dir, project, "down", "-v")
}

func (cliDocker) ComposeStop(ctx context.Context, dir, project string) (string, error) {
	return composeRun(ctx, dir, project, "stop")
}

func composeRun(ctx context.Context, dir, project string, args ...string) (string, error) {
	base := []string{
		"compose",
		"-f", "compose.yml",
		"-f", "compose.override.yml",
		"--env-file", ".env",
		"-p", project,
	}
	cmd := exec.CommandContext(ctx, "docker", append(base, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (cliDocker) ServiceUp(ctx context.Context, dir, project string) (string, error) {
	// A service project has a single generated compose.yml + .env — no override.
	base := []string{"compose", "-f", "compose.yml", "--env-file", ".env", "-p", project, "up", "-d"}
	cmd := exec.CommandContext(ctx, "docker", base...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (cliDocker) ControlPlaneUp(ctx context.Context, dir, project string) (string, error) {
	// Single compose.yml, no override, no .env — same shape as ServiceUp.
	base := []string{"compose", "-f", "compose.yml", "-p", project, "up", "-d"}
	cmd := exec.CommandContext(ctx, "docker", base...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (cliDocker) RunOneOff(ctx context.Context, image, network, envFile string, args []string) (string, error) {
	full := append([]string{"run", "--rm", "--network", network, "--env-file", envFile, image}, args...)
	out, err := exec.CommandContext(ctx, "docker", full...).CombinedOutput()
	return string(out), err
}

func (cliDocker) ContainerHealth(ctx context.Context, container string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f",
		`{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}`, container).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w\n%s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (cliDocker) Inspect(ctx context.Context, instanceID, mainService string) (bool, string, error) {
	cid, err := exec.CommandContext(ctx, "docker", "ps", "-q",
		"--filter", "label=moose.instance_id="+instanceID,
		"--filter", "label=com.docker.compose.service="+mainService).Output()
	container := strings.TrimSpace(string(cid))
	if err != nil || container == "" {
		return false, "", fmt.Errorf("main_service container not found")
	}
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f",
		`{{.State.Running}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}`,
		container).Output()
	if err != nil {
		return false, "", err
	}
	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) < 2 {
		return false, "", fmt.Errorf("unexpected inspect output: %q", out)
	}
	return parts[0] == "true", parts[1], nil
}

func (cliDocker) NetworkCreate(ctx context.Context, name string, internal bool) error {
	args := []string{"network", "create"}
	if internal {
		args = append(args, "--internal")
	}
	args = append(args, name)
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil && strings.Contains(string(out), "already exists") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

func (cliDocker) NetworkRemove(ctx context.Context, name string) error {
	return exec.CommandContext(ctx, "docker", "network", "rm", name).Run()
}

func (cliDocker) RemoveContainersByInstance(ctx context.Context, instanceID string) error {
	ids, err := exec.CommandContext(ctx, "docker", "ps", "-aq",
		"--filter", "label=moose.instance_id="+instanceID).Output()
	if err != nil {
		return err
	}
	for _, cid := range strings.Fields(string(ids)) {
		_ = exec.CommandContext(ctx, "docker", "rm", "-f", cid).Run()
	}
	return nil
}

func (cliDocker) RemoveImage(ctx context.Context, ref string) error {
	if out, err := exec.CommandContext(ctx, "docker", "rmi", ref).CombinedOutput(); err != nil {
		return fmt.Errorf("rmi %s: %w\n%s", ref, err, out)
	}
	return nil
}

func (cliDocker) UsernsRemap(ctx context.Context) (bool, error) {
	out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{json .SecurityOptions}}").Output()
	if err != nil {
		return false, fmt.Errorf("docker info: %w", err)
	}
	return usernsInSecurityOptions(out)
}

func (cliDocker) ImageUser(ctx context.Context, ref string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{json .Config.User}}", ref).Output()
	if err != nil {
		return "", fmt.Errorf("inspect the user of %s: %w", ref, err)
	}
	var user string
	if err := json.Unmarshal(bytes.TrimSpace(out), &user); err != nil {
		return "", fmt.Errorf("parse the user of %s %q: %w", ref, strings.TrimSpace(string(out)), err)
	}
	return user, nil
}

func (cliDocker) ImageUserFiles(ctx context.Context, instanceID, ref string) ([]byte, []byte, error) {
	// --pull never: the image was pulled or loaded by resolveImages. The
	// entrypoint is a path that does not exist, and the container is never
	// started anyway; it only has to exist so docker cp can read its files.
	var stderr bytes.Buffer
	create := exec.CommandContext(ctx, "docker", "create", "--pull", "never", "--network", "none",
		"--entrypoint", "/moose-image-user-probe",
		"--label", "moose.instance_id="+instanceID, "--label", "moose.image_user_probe=true", ref)
	create.Stderr = &stderr
	out, err := create.Output()
	if err != nil {
		return nil, nil, fmt.Errorf("create a probe container from %s: %w: %s", ref, err, strings.TrimSpace(stderr.String()))
	}
	cid := strings.TrimSpace(string(out))
	defer func() {
		// -v: an image that declares a VOLUME gets an anonymous volume for
		// the probe too, and it would outlive the container.
		if out, err := exec.Command("docker", "rm", "-f", "-v", cid).CombinedOutput(); err != nil {
			slog.Warn("image user probe container not removed",
				"instance_id", instanceID, "image", ref, "err", err, "output", strings.TrimSpace(string(out)))
		}
	}()
	passwd, err := copyUserFile(ctx, cid, "/etc/passwd")
	if err != nil {
		return nil, nil, fmt.Errorf("read /etc/passwd of %s: %w", ref, err)
	}
	group, err := copyUserFile(ctx, cid, "/etc/group")
	if err != nil {
		return nil, nil, fmt.Errorf("read /etc/group of %s: %w", ref, err)
	}
	return passwd, group, nil
}

// copyUserFile copies one file out of a container with docker cp, and nil
// when the image has no such file or it is not a regular file.
func copyUserFile(ctx context.Context, cid, path string) ([]byte, error) {
	var stderr bytes.Buffer
	cp := exec.CommandContext(ctx, "docker", "cp", cid+":"+path, "-")
	cp.Stderr = &stderr
	pipe, err := cp.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cp.Start(); err != nil {
		return nil, err
	}
	capped := &capReader{r: pipe, left: maxUserTar}
	b, kind, readErr := readUserFile(capped)
	switch {
	case readErr != nil || kind == entryOther:
		// Nothing more is needed: an error, or a symlink or a directory in
		// place of the file, whose archive could be large. Stop docker cp
		// rather than read the rest.
		if err := cp.Process.Kill(); err != nil {
			slog.Warn("docker cp not stopped", "src", path, "err", err)
		}
		waitErr := cp.Wait()
		if readErr != nil {
			return nil, readErr
		}
		slog.Info("image user file is not a regular file, treated as missing", "src", path, "err", waitErr)
		return nil, nil
	case kind == entryRegular:
		// Only the tar padding is left. Read it, still capped, so docker cp
		// is not stuck on a full pipe.
		if _, err := io.Copy(io.Discard, capped); err != nil {
			if kerr := cp.Process.Kill(); kerr != nil {
				slog.Warn("docker cp not stopped", "src", path, "err", kerr)
			}
			if werr := cp.Wait(); werr != nil {
				slog.Info("docker cp stopped", "src", path, "err", werr)
			}
			return nil, err
		}
	}
	if err := cp.Wait(); err != nil {
		// The daemon's answer for a path the image does not have.
		if strings.Contains(stderr.String(), "Could not find the file") {
			return nil, nil
		}
		return nil, fmt.Errorf("docker cp: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return b, nil
}

// maxUserFile is the largest /etc/passwd or /etc/group the brain reads, and
// maxUserTar the tar stream docker cp writes around one such file.
const (
	maxUserFile = 1 << 20
	maxUserTar  = maxUserFile + 64<<10
)

// What the first entry of a docker cp stream is.
const (
	entryNone    = iota // an empty stream: docker cp found nothing
	entryRegular        // a regular file, read
	entryOther          // a symlink, a directory or anything else
)

// readUserFile reads the first entry of the tar stream docker cp writes for
// a single path. Only a regular file is read: a symlink or anything else is
// reported as entryOther and treated as missing, since its target is not in
// the stream.
func readUserFile(r io.Reader) ([]byte, int, error) {
	tr := tar.NewReader(r)
	h, err := tr.Next()
	if err == io.EOF {
		return nil, entryNone, nil
	}
	if err != nil {
		return nil, entryNone, err
	}
	if h.Typeflag != tar.TypeReg {
		return nil, entryOther, nil
	}
	if h.Size > maxUserFile {
		return nil, entryOther, fmt.Errorf("%s is %d bytes, more than the %d this reads", h.Name, h.Size, maxUserFile)
	}
	b, err := io.ReadAll(tr)
	if err != nil {
		return nil, entryOther, err
	}
	return b, entryRegular, nil
}

// capReader fails once more than left bytes are read. io.LimitReader would
// end the stream quietly instead, and the tar reader could take that for the
// end of the archive.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, errors.New("the copied file is too big")
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// usernsInSecurityOptions parses `docker info`'s SecurityOptions (a JSON list
// such as ["name=seccomp,profile=builtin","name=userns"]) and reports whether
// the userns entry is there.
func usernsInSecurityOptions(raw []byte) (bool, error) {
	var opts []string
	if err := json.Unmarshal(bytes.TrimSpace(raw), &opts); err != nil {
		return false, fmt.Errorf("parse docker security options %q: %w", strings.TrimSpace(string(raw)), err)
	}
	for _, o := range opts {
		for _, kv := range strings.Split(o, ",") {
			if kv == "name=userns" {
				return true, nil
			}
		}
	}
	return false, nil
}

func (cliDocker) RestartCounts(ctx context.Context) (map[string]int, error) {
	ids, err := exec.CommandContext(ctx, "docker", "ps", "-aq",
		"--filter", "label=moose.managed=true").Output()
	if err != nil {
		return nil, err
	}
	idList := strings.Fields(string(ids))
	res := map[string]int{}
	if len(idList) == 0 {
		return res, nil
	}
	// One `docker inspect` over all managed containers: one line each, mapping
	// the owning instance_id to that container's cumulative RestartCount.
	args := append([]string{"inspect", "--format",
		`{{index .Config.Labels "moose.instance_id"}} {{.RestartCount}}`}, idList...)
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		n, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		// An instance can have several containers; the loop is per-container, so
		// take the max — raise if any one container is crash-looping.
		if n > res[parts[0]] {
			res[parts[0]] = n
		}
	}
	return res, nil
}

func (cliDocker) ManagedContainers(ctx context.Context) ([]ManagedContainer, error) {
	ids, err := exec.CommandContext(ctx, "docker", "ps", "-aq",
		"--filter", "label=moose.managed=true").Output()
	if err != nil {
		return nil, err
	}
	idList := strings.Fields(string(ids))
	if len(idList) == 0 {
		return nil, nil
	}
	// One `docker inspect` over all managed containers: instance_id, compose
	// service, running flag, and StartedAt (RFC3339) per line.
	args := append([]string{"inspect", "--format",
		`{{index .Config.Labels "moose.instance_id"}} {{index .Config.Labels "com.docker.compose.service"}} {{.State.Running}} {{.State.StartedAt}}`}, idList...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil && stdout.Len() == 0 {
		// Docker daemon unreachable (not a TOCTOU race — that produces partial
		// stdout). Return the error so the caller can skip this tick.
		return nil, err
	}
	// TOCTOU: a container removed between the `docker ps` above and this inspect
	// causes Docker to exit non-zero but still outputs the containers it inspected.
	// Parse partial output — the missing containers simply won't appear.
	var res []ManagedContainer
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		parts := strings.Fields(line)
		if len(parts) != 4 {
			continue
		}
		// Zero time on a parse failure (never-started container) — the caller's
		// start-period gate treats it as not-yet-eligible, which is correct.
		started, _ := time.Parse(time.RFC3339Nano, parts[3])
		res = append(res, ManagedContainer{
			InstanceID: parts[0],
			Service:    parts[1],
			Running:    parts[2] == "true",
			StartedAt:  started,
		})
	}
	return res, nil
}

func (cliDocker) PSManaged(ctx context.Context) (map[string]bool, error) {
	out, err := exec.CommandContext(ctx, "docker", "ps", "-a",
		"--filter", "label=moose.managed=true",
		"--format", `{{.Label "moose.instance_id"}} {{.State}}`).Output()
	if err != nil {
		return nil, err
	}
	res := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}
		id := parts[0]
		running := len(parts) > 1 && parts[1] == "running"
		res[id] = res[id] || running
	}
	return res, nil
}
