package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/onmoose/os/internal/caddy"
	"github.com/onmoose/os/internal/hostclient"
	"github.com/onmoose/os/internal/protocol"
)

// call records one driver invocation across any fake. We keep it as a flat
// `<method>(<arg1>,<arg2>,…)` string so assertions can substring-match without
// caring about call shape; tests that need exact args inspect typed fields on
// the fake instead.
type call struct {
	method string
	args   []any
}

func (c call) String() string {
	parts := make([]string, len(c.args))
	for i, a := range c.args {
		parts[i] = fmt.Sprintf("%v", a)
	}
	out := c.method + "("
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out + ")"
}

// fakeDocker is a scriptable, recording stand-in for DockerDriver.
//
// Defaults: every operation succeeds. The interesting tests set one field
// (e.g. PullErr) to drive a specific failure mode.
type fakeDocker struct {
	mu sync.Mutex

	digests map[string]string // image → digest returned by ImageInspect
	loaded  map[string]bool   // image present locally with NO RepoDigest (docker-loaded)
	// present holds the digest refs a Pull put in the local store, so
	// ensureImages finds an installed app's images on a later start (#588).
	present map[string]bool
	pullErr map[string]error // per-ref Pull error (nil = success)
	// pullErrAll fails Pull for every ref, tag or digest form alike. An
	// unreachable registry is a property of the box, not of one ref — keying such
	// a failure by ref would let a pull the code makes under a different ref
	// (Door-1 pulls `name@sha256:…`, never the tag) succeed against a registry the
	// test says is gone.
	pullErrAll error
	// pullFails makes the next N Pull calls fail with pullFailErr, the way a
	// registry answers a short rate-limit spike (#586).
	pullFails   int
	pullFailErr error

	composeUp func(ctx context.Context, dir, project string) (string, error)
	inspect   func(id, mainService string) (running bool, health string, err error)
	psManaged map[string]bool // returned by PSManaged
	// psManagedFails makes the next N PSManaged calls fail, the way Docker
	// through a proxy that is not up yet does after a reboot (#540).
	psManagedFails int
	restartCounts  map[string]int     // returned by RestartCounts
	managed        []ManagedContainer // returned by ManagedContainers

	// running is what RunningContainers lists, by compose project, and
	// runningErr makes it fail. StopContainer takes a container off the list.
	// stopContainer, when set, does StopContainer's work in its place.
	running       map[string][]string
	runningErr    error
	stopContainer func(container string) error

	composeUpErr      error // simple "always fail compose up"
	composeDownErr    error
	composeStopErr    error
	removeImageErr    error
	serviceUpErr      error
	controlPlaneUpErr error
	networkCreateErr  error // forces NetworkCreate to return this error

	// attached is the networks each container is on, by container name, as
	// NetworkConnect/NetworkDisconnect leave them and ContainerNetworks reads
	// them. networkConnectErr, networkDisconnectErr and containerNetworksErr
	// force those calls to fail. A failed disconnect leaves attached as it is.
	attached             map[string]map[string]bool
	networkConnectErr    error
	networkDisconnectErr error
	containerNetworksErr error

	// networks is each network NetworkCreate made and its internal flag, as
	// NetworkInspect reads it (#633). Like Docker, a create of a network that
	// exists keeps the old flag. composeCreateErr forces ComposeCreate to fail.
	networks         map[string]bool
	composeCreateErr error

	// runOneOff drives RunOneOff: it returns scripted output/error per invocation
	// (matched on args). Default (nil) returns ("", nil) — provisioning only cares
	// about the error. containerHealth, when set, overrides the readiness status
	// ContainerHealth returns (default "healthy", so waitServiceReady passes).
	runOneOff func(image, network, envFile string, args []string) (string, error)
	// containerHealth overrides the status ContainerHealth returns (default
	// "healthy"); containerHealthErr forces its error path (readiness inspect
	// fails). When both are set the error wins.
	containerHealth    string
	containerHealthErr error

	// runStream drives RunOneOffStream. Default (nil) writes "dump:<last arg>"
	// to stdout and drains stdin into streamed, so a snapshot test can check
	// that a restore fed back the bytes the dump wrote.
	runStream func(image, network, envFile string, args []string, stdin io.Reader, stdout io.Writer) (string, error)
	streamed  []string

	// usernsRemap is what UsernsRemap reports: whether the daemon runs with
	// userns-remap. The zero value is no remap, like the dev loop's Docker.
	// usernsRemapErr forces the docker info failure.
	usernsRemap    bool
	usernsRemapErr error

	// imageUsers is what ImageUser reports per image ref (Config.User); a
	// ref not in it has no user. imagePasswd and imageGroup are what
	// ImageUserFiles returns per ref (nil: the file is missing).
	// imageFilesErr forces the probe failure.
	imageUsers    map[string]string
	imagePasswd   map[string]string
	imageGroup    map[string]string
	imageFilesErr error

	calls []call
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		digests:   map[string]string{},
		loaded:    map[string]bool{},
		present:   map[string]bool{},
		pullErr:   map[string]error{},
		psManaged: map[string]bool{},
		attached:  map[string]map[string]bool{},
		networks:  map[string]bool{},
	}
}

func (f *fakeDocker) record(method string, args ...any) {
	f.mu.Lock()
	f.calls = append(f.calls, call{method: method, args: args})
	f.mu.Unlock()
}

func (f *fakeDocker) Calls() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]call, len(f.calls))
	copy(out, f.calls)
	return out
}

// methods records all methods called, in order.
func (f *fakeDocker) methods() []string {
	cs := f.Calls()
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.method
	}
	return out
}

// pulled returns the image refs Pull was called with, in order.
func (f *fakeDocker) pulled() []string {
	var out []string
	for _, c := range f.Calls() {
		if c.method == "Pull" && len(c.args) == 1 {
			out = append(out, fmt.Sprintf("%v", c.args[0]))
		}
	}
	return out
}

func (f *fakeDocker) called(method string) bool {
	for _, m := range f.methods() {
		if m == method {
			return true
		}
	}
	return false
}

func (f *fakeDocker) Pull(_ context.Context, image string) error {
	f.record("Pull", image)
	f.mu.Lock()
	if f.pullFails > 0 {
		f.pullFails--
		f.mu.Unlock()
		return f.pullFailErr
	}
	f.mu.Unlock()
	if f.pullErrAll != nil {
		return f.pullErrAll
	}
	if err := f.pullErr[image]; err != nil {
		return err
	}
	if strings.Contains(image, "@") {
		f.mu.Lock()
		f.present[image] = true
		f.mu.Unlock()
	}
	return nil
}

func (f *fakeDocker) ImageInspect(_ context.Context, image string) (RepoDigests, error) {
	f.record("ImageInspect", image)
	if d, ok := f.digests[image]; ok {
		return RepoDigests{repoOf(image) + "@" + d}, nil
	}
	// A docker-loaded (offline-bundle) image is present but carries no
	// RepoDigest — inspect succeeds with an empty list, mirroring the CLI.
	if f.loaded[image] {
		return RepoDigests{}, nil
	}
	// Pulled by digest: the store names it under the ref it was pulled as.
	f.mu.Lock()
	present := f.present[image]
	f.mu.Unlock()
	if present {
		return RepoDigests{image}, nil
	}
	// Absent: inspect errors, as `docker image inspect` does for a missing image.
	return nil, fmt.Errorf("fakeDocker: image %q not present", image)
}

func (f *fakeDocker) ComposeUp(ctx context.Context, dir, project string) (string, error) {
	f.record("ComposeUp", dir, project)
	if f.composeUp != nil {
		return f.composeUp(ctx, dir, project)
	}
	if f.composeUpErr != nil {
		return "boom", f.composeUpErr
	}
	return "", nil
}

func (f *fakeDocker) ServiceUp(_ context.Context, dir, project string) (string, error) {
	f.record("ServiceUp", dir, project)
	if f.serviceUpErr != nil {
		return "boom", f.serviceUpErr
	}
	return "", nil
}

func (f *fakeDocker) ControlPlaneUp(_ context.Context, dir, project string) (string, error) {
	f.record("ControlPlaneUp", dir, project)
	if f.controlPlaneUpErr != nil {
		return "boom", f.controlPlaneUpErr
	}
	return "", nil
}

func (f *fakeDocker) ControlPlaneRecreate(_ context.Context, dir, project, service string) (string, error) {
	f.record("ControlPlaneRecreate", dir, project, service)
	if f.controlPlaneUpErr != nil {
		return "boom", f.controlPlaneUpErr
	}
	return "", nil
}

func (f *fakeDocker) RunOneOff(_ context.Context, image, network, envFile string, args []string) (string, error) {
	f.record("RunOneOff", image, network, envFile, strings.Join(args, " "))
	if f.runOneOff != nil {
		return f.runOneOff(image, network, envFile, args)
	}
	// The snapshot's size query (dumpEstimate) reads a number back.
	if q := strings.Join(args, " "); strings.Contains(q, "pg_database_size") || strings.Contains(q, "information_schema.tables") {
		return "0", nil
	}
	return "", nil
}

func (f *fakeDocker) RunOneOffStream(_ context.Context, image, network, envFile string, args []string, stdin io.Reader, stdout io.Writer) (string, error) {
	f.record("RunOneOffStream", image, network, envFile, strings.Join(args, " "))
	if f.runStream != nil {
		return f.runStream(image, network, envFile, args, stdin, stdout)
	}
	if stdin != nil {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return "", err
		}
		f.mu.Lock()
		f.streamed = append(f.streamed, string(b))
		f.mu.Unlock()
	}
	if stdout != nil && len(args) > 0 {
		if _, err := io.WriteString(stdout, "dump:"+args[len(args)-1]); err != nil {
			return "", err
		}
	}
	return "", nil
}

func (f *fakeDocker) ContainerHealth(_ context.Context, container string) (string, error) {
	f.record("ContainerHealth", container)
	if f.containerHealthErr != nil {
		return "", f.containerHealthErr
	}
	if f.containerHealth != "" {
		return f.containerHealth, nil
	}
	return "healthy", nil
}

func (f *fakeDocker) ComposeDown(_ context.Context, dir, project string) (string, error) {
	f.record("ComposeDown", dir, project)
	return "", f.composeDownErr
}

func (f *fakeDocker) ComposeStop(_ context.Context, dir, project string) (string, error) {
	f.record("ComposeStop", dir, project)
	return "", f.composeStopErr
}

func (f *fakeDocker) RunningContainers(_ context.Context, project string) ([]string, error) {
	f.record("RunningContainers", project)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runningErr != nil {
		return nil, f.runningErr
	}
	return append([]string(nil), f.running[project]...), nil
}

func (f *fakeDocker) StopContainer(_ context.Context, container string) (string, error) {
	f.record("StopContainer", container)
	if f.stopContainer != nil {
		return "", f.stopContainer(container)
	}
	f.removeRunning(container)
	return "", nil
}

// removeRunning takes a container off the running list of every project.
func (f *fakeDocker) removeRunning(container string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for project, ids := range f.running {
		f.running[project] = slices.DeleteFunc(ids, func(id string) bool { return id == container })
	}
}

func (f *fakeDocker) ComposeCreate(_ context.Context, dir, project string) (string, error) {
	f.record("ComposeCreate", dir, project)
	return "", f.composeCreateErr
}

func (f *fakeDocker) Inspect(_ context.Context, id, main string) (bool, string, error) {
	f.record("Inspect", id, main)
	if f.inspect != nil {
		return f.inspect(id, main)
	}
	return true, "healthy", nil
}

func (f *fakeDocker) NetworkCreate(_ context.Context, name string, internal bool) error {
	f.record("NetworkCreate", name, internal)
	if f.networkCreateErr != nil {
		return f.networkCreateErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.networks[name]; !ok {
		f.networks[name] = internal
	}
	return nil
}

func (f *fakeDocker) NetworkRemove(_ context.Context, name string) error {
	f.record("NetworkRemove", name)
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.networks, name)
	return nil
}

func (f *fakeDocker) NetworkInspect(_ context.Context, name string) (NetworkInfo, error) {
	f.record("NetworkInspect", name)
	f.mu.Lock()
	defer f.mu.Unlock()
	internal, ok := f.networks[name]
	if !ok {
		return NetworkInfo{}, nil
	}
	info := NetworkInfo{Exists: true, Internal: internal}
	for c, nets := range f.attached {
		if nets[name] {
			info.Containers = append(info.Containers, c)
		}
	}
	sort.Strings(info.Containers)
	return info, nil
}

// networkInternal is a network's flag; ok is false when it does not exist.
func (f *fakeDocker) networkInternal(name string) (internal, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	internal, ok = f.networks[name]
	return internal, ok
}

func (f *fakeDocker) NetworkConnect(_ context.Context, network, container string) error {
	f.record("NetworkConnect", network, container)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.networkConnectErr != nil {
		return f.networkConnectErr
	}
	if f.attached[container] == nil {
		f.attached[container] = map[string]bool{}
	}
	f.attached[container][network] = true
	return nil
}

func (f *fakeDocker) NetworkDisconnect(_ context.Context, network, container string) error {
	f.record("NetworkDisconnect", network, container)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.networkDisconnectErr != nil {
		return f.networkDisconnectErr
	}
	delete(f.attached[container], network)
	return nil
}

func (f *fakeDocker) ContainerNetworks(_ context.Context, container string) (map[string]bool, error) {
	f.record("ContainerNetworks", container)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.containerNetworksErr != nil {
		return nil, f.containerNetworksErr
	}
	out := map[string]bool{}
	for n := range f.attached[container] {
		out[n] = true
	}
	return out, nil
}

func (f *fakeDocker) PSManaged(_ context.Context) (map[string]bool, error) {
	f.record("PSManaged")
	f.mu.Lock()
	failing := f.psManagedFails > 0
	if failing {
		f.psManagedFails--
	}
	f.mu.Unlock()
	if failing {
		return nil, errors.New("exit status 1: Cannot connect to the Docker daemon at tcp://docker-proxy:2375")
	}
	out := make(map[string]bool, len(f.psManaged))
	for k, v := range f.psManaged {
		out[k] = v
	}
	return out, nil
}

func (f *fakeDocker) RestartCounts(_ context.Context) (map[string]int, error) {
	f.record("RestartCounts")
	out := make(map[string]int, len(f.restartCounts))
	for k, v := range f.restartCounts {
		out[k] = v
	}
	return out, nil
}

func (f *fakeDocker) ManagedContainers(_ context.Context) ([]ManagedContainer, error) {
	f.record("ManagedContainers")
	out := make([]ManagedContainer, len(f.managed))
	copy(out, f.managed)
	return out, nil
}

func (f *fakeDocker) RemoveContainersByInstance(_ context.Context, id string) error {
	f.record("RemoveContainersByInstance", id)
	return nil
}

func (f *fakeDocker) RemoveImage(_ context.Context, ref string) error {
	f.record("RemoveImage", ref)
	return f.removeImageErr
}

func (f *fakeDocker) UsernsRemap(_ context.Context) (bool, error) {
	f.record("UsernsRemap")
	return f.usernsRemap, f.usernsRemapErr
}

func (f *fakeDocker) ImageUser(_ context.Context, ref string) (string, error) {
	f.record("ImageUser", ref)
	return f.imageUsers[ref], nil
}

func (f *fakeDocker) ImageUserFiles(_ context.Context, instanceID, ref string) ([]byte, []byte, error) {
	f.record("ImageUserFiles", instanceID, ref)
	if f.imageFilesErr != nil {
		return nil, nil, f.imageFilesErr
	}
	var passwd, group []byte
	if p, ok := f.imagePasswd[ref]; ok {
		passwd = []byte(p)
	}
	if g, ok := f.imageGroup[ref]; ok {
		group = []byte(g)
	}
	return passwd, group, nil
}

// --- caddy fake ----------------------------------------------------------

type fakeCaddy struct {
	mu      sync.Mutex
	routes  map[string]string            // instanceID → "splash:<state>" | "upstream:<addr>"
	configs map[string]caddy.RouteConfig // instanceID → last AddRoute policy (#306)
	calls   []call
	// writeErr, when set, fails every AddRoute and AddSplashRoute, which then
	// change nothing.
	writeErr error
}

func newFakeCaddy() *fakeCaddy {
	return &fakeCaddy{routes: map[string]string{}, configs: map[string]caddy.RouteConfig{}}
}

func (c *fakeCaddy) record(method string, args ...any) {
	c.mu.Lock()
	c.calls = append(c.calls, call{method: method, args: args})
	c.mu.Unlock()
}

func (c *fakeCaddy) EnsureServer(context.Context) error {
	c.record("EnsureServer")
	return nil
}

func (c *fakeCaddy) AddRoute(_ context.Context, cfg caddy.RouteConfig) error {
	c.record("AddRoute", cfg.InstanceID, cfg.Host, cfg.Upstream)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writeErr != nil {
		return c.writeErr
	}
	c.routes[cfg.InstanceID] = "upstream:" + cfg.Upstream
	c.configs[cfg.InstanceID] = cfg
	return nil
}

func (c *fakeCaddy) AddSplashRoute(ctx context.Context, id, host, name, state string) error {
	c.record("AddSplashRoute", id, host, name, state)
	// Like the real client, a write on a dead context never lands.
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writeErr != nil {
		return c.writeErr
	}
	c.routes[id] = "splash:" + state
	return nil
}

func (c *fakeCaddy) RemoveRoute(_ context.Context, id string) error {
	c.record("RemoveRoute", id)
	c.mu.Lock()
	delete(c.routes, id)
	c.mu.Unlock()
	return nil
}

func (c *fakeCaddy) route(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.routes[id]
}

// config returns the last RouteConfig AddRoute recorded for an instance, so
// #306 tests can assert on the resolved forward-auth + Cookie-strip policy.
func (c *fakeCaddy) config(id string) caddy.RouteConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.configs[id]
}

func (c *fakeCaddy) called(method string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range c.calls {
		if k.method == method {
			return true
		}
	}
	return false
}

// --- host fake -----------------------------------------------------------

type fakeHost struct {
	mu        sync.Mutex
	published map[string]bool
	calls     []call

	// resolveHomeErr, when set, is returned by ResolveHome (e.g.
	// hostclient.ErrUnknownUser to exercise the deleted-owner path).
	resolveHomeErr error

	// homeRoot is the parent of each fake user's home dir. Set to a writable
	// temp dir by newTestEnv so the install path can MkdirAll a personal folder
	// source under it (a real /home/<user> isn't writable in a unit test).
	homeRoot string

	// systemStatus is returned by SystemStatus; tests set DataDiskFreeBytes to
	// assert the install-plan free figure. statusErr forces the host-error path
	// (FreeBytes must degrade to 0).
	systemStatus protocol.SystemStatus
	statusErr    error
	// resources is returned by SystemResources, for the fit check (#599). The
	// zero value reports no memory, which reads as "fit unknown", so a test
	// that does not set it installs as before. resourcesErr forces the
	// host-error path.
	resources    protocol.SystemResources
	resourcesErr error

	// gpu is returned by SystemGPU; the zero value reports no usable GPU, so
	// the capacity-refusal test needs no setup and stanza tests set Present +
	// RenderGID explicitly. gpuErr forces the host-error path.
	gpu    protocol.SystemGPU
	gpuErr error

	// allocated counts app-service identities handed out (UIDs are band start +
	// counter); released records every UID returned. allocErr forces the
	// allocation host-failure path.
	allocated int
	released  []int
	allocErr  error

	// publishErr forces Publish to fail (the mDNS-down path); publishName, when
	// set, overrides the returned name to exercise the box-qualified collision
	// fallback (a published name differing from the primary <slug>.local).
	publishErr  error
	publishName string

	// prepareErr forces PrepareUserFolder to fail (a host-agent refusal, such as
	// a symlink in the way). Every call is also recorded in calls.
	prepareErr error

	// pinUnsupported makes PinFolder report an old host-agent (404), so a test
	// can exercise the prepare-folder fallback. pinErr forces any other pin
	// failure. The default walks the source with Lstat and returns the real
	// path: the fake host runs unprivileged and cannot mount, and the real path
	// is a no-op pin for the fake compose.
	pinUnsupported bool
	pinErr         error
	// pinPath, when set, is what a successful PinFolder returns, so a test can
	// assert the brain writes the pin (not the real path) into compose.pins.yml.
	pinPath string
	// sharedRoot is where the fake's shared tree lives; newTestEnv sets it to
	// the Manager's sharedRoot.
	sharedRoot string

	// remapBase is the remap_base WellKnownIdentity reports (nil: no remap,
	// like the fake host-agent). wellKnownErr forces that call to fail.
	remapBase    *int
	wellKnownErr error
}

func newFakeHost() *fakeHost { return &fakeHost{published: map[string]bool{}} }

func (h *fakeHost) Publish(_ context.Context, slug string) (protocol.PublishResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, call{method: "Publish", args: []any{slug}})
	if h.publishErr != nil {
		return protocol.PublishResponse{}, h.publishErr
	}
	h.published[slug] = true
	name := slug + ".local"
	if h.publishName != "" {
		name = h.publishName
	}
	return protocol.PublishResponse{Name: name}, nil
}

func (h *fakeHost) Unpublish(_ context.Context, slug string) error {
	h.mu.Lock()
	h.calls = append(h.calls, call{method: "Unpublish", args: []any{slug}})
	delete(h.published, slug)
	h.mu.Unlock()
	return nil
}

func (h *fakeHost) ResolveHome(_ context.Context, user string) (protocol.ResolveHomeResponse, error) {
	h.mu.Lock()
	h.calls = append(h.calls, call{method: "ResolveHome", args: []any{user}})
	h.mu.Unlock()
	if h.resolveHomeErr != nil {
		return protocol.ResolveHomeResponse{}, h.resolveHomeErr
	}
	return protocol.ResolveHomeResponse{HomePath: filepath.Join(h.homeRoot, user), UID: 3000, GID: 3000}, nil
}

func (h *fakeHost) PrepareUserFolder(_ context.Context, user, rel string) error {
	h.mu.Lock()
	h.calls = append(h.calls, call{method: "PrepareUserFolder", args: []any{user, rel}})
	h.mu.Unlock()
	return h.prepareErr
}

func (h *fakeHost) PinFolder(_ context.Context, req protocol.PinFolderRequest) (protocol.PinFolderResponse, error) {
	h.mu.Lock()
	h.calls = append(h.calls, call{method: "PinFolder", args: []any{req.Username, req.Shared, req.Path}})
	h.mu.Unlock()
	if h.pinUnsupported {
		return protocol.PinFolderResponse{}, hostclient.ErrPinUnsupported
	}
	if h.pinErr != nil {
		return protocol.PinFolderResponse{}, h.pinErr
	}
	root := h.sharedRoot
	if !req.Shared {
		root = filepath.Join(h.homeRoot, req.Username)
	}
	cur := root
	for _, part := range strings.Split(req.Path, "/") {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			// Most unit tests never make the folder source; Docker would create
			// a missing bind source. Keep the real path as the no-op pin. A test
			// that wants the refusal creates the source first.
			return protocol.PinFolderResponse{Path: filepath.Join(root, filepath.FromSlash(req.Path))}, nil
		}
		if err != nil {
			return protocol.PinFolderResponse{}, err
		}
		if !fi.IsDir() {
			return protocol.PinFolderResponse{}, hostclient.ErrFolderSourceSwapped
		}
	}
	if h.pinPath != "" {
		return protocol.PinFolderResponse{Path: h.pinPath}, nil
	}
	return protocol.PinFolderResponse{Path: cur}, nil
}

func (h *fakeHost) WellKnownIdentity(_ context.Context) (protocol.WellKnownIdentityResponse, error) {
	h.mu.Lock()
	h.calls = append(h.calls, call{method: "WellKnownIdentity"})
	h.mu.Unlock()
	if h.wellKnownErr != nil {
		return protocol.WellKnownIdentityResponse{}, h.wellKnownErr
	}
	return protocol.WellKnownIdentityResponse{MooseAppUID: 2000, MooseAppGID: 2000, MooseSharedGID: 2001, RemapBase: h.remapBase}, nil
}

func (h *fakeHost) SystemStatus(_ context.Context) (protocol.SystemStatus, error) {
	h.mu.Lock()
	h.calls = append(h.calls, call{method: "SystemStatus"})
	h.mu.Unlock()
	if h.statusErr != nil {
		return protocol.SystemStatus{}, h.statusErr
	}
	return h.systemStatus, nil
}

func (h *fakeHost) SystemResources(_ context.Context) (protocol.SystemResources, error) {
	h.mu.Lock()
	h.calls = append(h.calls, call{method: "SystemResources"})
	h.mu.Unlock()
	if h.resourcesErr != nil {
		return protocol.SystemResources{}, h.resourcesErr
	}
	return h.resources, nil
}

func (h *fakeHost) SystemGPU(_ context.Context) (protocol.SystemGPU, error) {
	h.mu.Lock()
	h.calls = append(h.calls, call{method: "SystemGPU"})
	h.mu.Unlock()
	if h.gpuErr != nil {
		return protocol.SystemGPU{}, h.gpuErr
	}
	return h.gpu, nil
}

// AllocateAppServiceIdentity hands out sequential UIDs from the band start,
// mirroring the fake host-agent. allocErr forces the host-failure path.
func (h *fakeHost) AllocateAppServiceIdentity(_ context.Context, instanceID string) (protocol.AllocateAppServiceIdentityResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, call{method: "AllocateAppServiceIdentity", args: []any{instanceID}})
	if h.allocErr != nil {
		return protocol.AllocateAppServiceIdentityResponse{}, h.allocErr
	}
	uid := protocol.AppServiceUIDMin + h.allocated
	h.allocated++
	return protocol.AllocateAppServiceIdentityResponse{UID: uid, GID: uid}, nil
}

func (h *fakeHost) ReleaseAppServiceIdentity(_ context.Context, uid int) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, call{method: "ReleaseAppServiceIdentity", args: []any{uid}})
	h.released = append(h.released, uid)
	return nil
}

func (h *fakeHost) isPublished(slug string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.published[slug]
}

// publishCount reports how many times Publish was called for a slug, so a test
// can assert a *re*-publish (e.g. Start re-asserting an already-announced name)
// rather than just the eventual published state.
func (h *fakeHost) publishCount(slug string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.calls {
		if c.method == "Publish" && len(c.args) == 1 && c.args[0] == slug {
			n++
		}
	}
	return n
}

// dropPublished simulates the host-agent losing its process-local Avahi entry
// group for a slug (e.g. a mid-life host-agent restart) — the name goes dark
// without a recorded call, so a test can verify a later Publish re-announces it.
func (h *fakeHost) dropPublished(slug string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.published, slug)
}

func (h *fakeHost) called(method string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.calls {
		if c.method == method {
			return true
		}
	}
	return false
}

// callsTo returns every recorded call to one method, in order.
func (h *fakeHost) callsTo(method string) []call {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []call
	for _, c := range h.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}
