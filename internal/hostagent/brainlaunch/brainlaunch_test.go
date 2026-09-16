package brainlaunch

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/onmoose/moose/internal/protocol"
)

// fakeDocker records calls and returns programmed results so Launch's sequence
// is exercised with no Docker daemon. Zero value: image present, label matches
// protocol.Major, container absent — the steady "image already loaded, fresh
// box" path; tests override the fields they're probing.
//
// present is the default ImagePresent answer for any ref; presentByRef overrides
// it per-ref so a test can express "proxy image absent, brain image present"
// (the first-boot sequence loads each from its own tarball). Production's
// CLIDocker.ImagePresent already queries each ref independently — this keeps the
// fake honest to that.
type fakeDocker struct {
	present      bool
	presentByRef map[string]bool
	presentErr   error
	loadErr      error
	label        string
	labelErr     error
	exists       bool
	existsErr    error
	sandboxed    bool
	sandboxedErr error
	removeErr    error
	removeCalls  int
	lastRemove   string
	runErr       error
	runErrs      []error
	netErr       error
	loadCalls    int
	loadPaths    []string
	runCalls     int
	netCalls     int
	lastRun      RunSpec
	runSpecs     []RunSpec
	lastNet      string
	lastLabelKey string
}

func newFake() *fakeDocker {
	return &fakeDocker{present: true, label: "1"}
}

func (f *fakeDocker) ImagePresent(_ context.Context, ref string) (bool, error) {
	if v, ok := f.presentByRef[ref]; ok {
		return v, f.presentErr
	}
	return f.present, f.presentErr
}
func (f *fakeDocker) Load(_ context.Context, path string) error {
	f.loadCalls++
	f.loadPaths = append(f.loadPaths, path)
	return f.loadErr
}
func (f *fakeDocker) ImageLabel(_ context.Context, _, label string) (string, error) {
	f.lastLabelKey = label
	return f.label, f.labelErr
}
func (f *fakeDocker) ContainerExists(context.Context, string) (bool, error) {
	return f.exists, f.existsErr
}
func (f *fakeDocker) Run(_ context.Context, spec RunSpec) error {
	f.runCalls++
	f.lastRun = spec
	f.runSpecs = append(f.runSpecs, spec)
	// runErrs, when set, gives a per-call answer (so a test can fail the first
	// run and succeed on the retry); runErr is the same answer every time.
	if len(f.runErrs) > 0 {
		err := f.runErrs[0]
		f.runErrs = f.runErrs[1:]
		return err
	}
	return f.runErr
}
func (f *fakeDocker) ContainerSandboxed(context.Context, string) (bool, error) {
	return f.sandboxed, f.sandboxedErr
}
func (f *fakeDocker) Remove(_ context.Context, name string) error {
	f.removeCalls++
	f.lastRemove = name
	return f.removeErr
}
func (f *fakeDocker) NetworkCreate(_ context.Context, name string) error {
	f.netCalls++
	f.lastNet = name
	return f.netErr
}

func testConfig() Config {
	return Config{
		Image:         "moose-brain:dev",
		ImageTar:      "/var/lib/moose/brain-image.tar",
		ContainerName: "moose-brain",
		DataDir:       "/var/lib/moose",
		StateDir:      "/var/lib/moose/state",
		SocketPath:    "/var/run/moose/agent.sock",

		Network:            "moose-ingress",
		ProxyImage:         "tecnativa/docker-socket-proxy:v0.4.2",
		ProxyImageTar:      "/var/lib/moose/control-plane/images/docker-socket-proxy.tar",
		ProxyContainerName: "moose-docker-proxy",
		ControlPlaneDir:    "/var/lib/moose/control-plane",
		UIUpstream:         "moose-ui:80",
		CatalogURL:         "https://onmoose.network",
		CatalogCacheDir:    "/var/lib/moose/catalog-cache",
	}
}

func TestLaunchImageAbsentLoadsThenRuns(t *testing.T) {
	f := newFake()
	f.present = false // image not loaded yet → docker load

	if err := Launch(context.Background(), f, testConfig()); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if f.loadCalls != 1 {
		t.Errorf("load calls = %d, want 1 (absent image must be loaded)", f.loadCalls)
	}
	if f.runCalls != 1 {
		t.Errorf("run calls = %d, want 1", f.runCalls)
	}
	if f.lastLabelKey != protocol.ImageProtocolMajorLabel {
		t.Errorf("read label %q, want %q", f.lastLabelKey, protocol.ImageProtocolMajorLabel)
	}
}

func TestLaunchImagePresentSkipsLoad(t *testing.T) {
	f := newFake() // present=true

	if err := Launch(context.Background(), f, testConfig()); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if f.loadCalls != 0 {
		t.Errorf("load calls = %d, want 0 (present image must not be reloaded)", f.loadCalls)
	}
	if f.runCalls != 1 {
		t.Errorf("run calls = %d, want 1", f.runCalls)
	}
}

func TestLaunchRunSpec(t *testing.T) {
	f := newFake()
	if err := Launch(context.Background(), f, testConfig()); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	s := f.lastRun
	if s.Name != "moose-brain" || s.Image != "moose-brain:dev" {
		t.Errorf("run spec name/image = %q/%q", s.Name, s.Image)
	}
	if s.Restart != "unless-stopped" {
		t.Errorf("restart policy = %q, want unless-stopped", s.Restart)
	}
	if !hasMount(s.Mounts, "/var/run/moose", "/var/run/moose") {
		t.Errorf("missing host-agent socket-dir mount: %+v", s.Mounts)
	}
	if !hasMount(s.Mounts, "/var/lib/moose", "/var/lib/moose") {
		t.Errorf("missing data-dir mount: %+v", s.Mounts)
	}
	if v := envVal(s.Env, "MOOSE_STATE_DIR"); v != "/var/lib/moose/state" {
		t.Errorf("MOOSE_STATE_DIR = %q, want /var/lib/moose/state", v)
	}
	if v := envVal(s.Env, "MOOSE_AGENT_SOCK"); v != "/var/run/moose/agent.sock" {
		t.Errorf("MOOSE_AGENT_SOCK = %q, want the socket path", v)
	}
	// M1b: the brain joins the ingress network and is pointed at the proxy +
	// Caddy + the staged control-plane compose. It must never get the raw socket.
	if s.Network != "moose-ingress" {
		t.Errorf("brain network = %q, want moose-ingress", s.Network)
	}
	if v := envVal(s.Env, "DOCKER_HOST"); v != "tcp://docker-proxy:2375" {
		t.Errorf("DOCKER_HOST = %q, want tcp://docker-proxy:2375", v)
	}
	if v := envVal(s.Env, "MOOSE_CADDY_ADMIN"); v != "http://moose-caddy:2019" {
		t.Errorf("MOOSE_CADDY_ADMIN = %q, want http://moose-caddy:2019", v)
	}
	if v := envVal(s.Env, "MOOSE_CONTROL_PLANE_DIR"); v != "/var/lib/moose/control-plane" {
		t.Errorf("MOOSE_CONTROL_PLANE_DIR = %q", v)
	}
	if v := envVal(s.Env, "MOOSE_DASHBOARD_UI_UPSTREAM"); v != "moose-ui:80" {
		t.Errorf("MOOSE_DASHBOARD_UI_UPSTREAM = %q, want moose-ui:80", v)
	}
	// The control-plane catalog origin + asset cache dir (icons and screenshots
	// only — the snapshot is never written to disk). The cache is under DataDir, so
	// it rides the data-dir mount (no separate Mount entry).
	if v := envVal(s.Env, "MOOSE_CATALOG_URL"); v != "https://onmoose.network" {
		t.Errorf("MOOSE_CATALOG_URL = %q, want https://onmoose.network", v)
	}
	if v := envVal(s.Env, "MOOSE_CATALOG_CACHE_DIR"); v != "/var/lib/moose/catalog-cache" {
		t.Errorf("MOOSE_CATALOG_CACHE_DIR = %q, want /var/lib/moose/catalog-cache", v)
	}
	// OfflineInstall defaults off → the brain gets no MOOSE_OFFLINE_INSTALL.
	if v := envVal(s.Env, "MOOSE_OFFLINE_INSTALL"); v != "" {
		t.Errorf("MOOSE_OFFLINE_INSTALL = %q, want unset when OfflineInstall is false", v)
	}
	// CaddyImage empty (the appliance/dev default) → the brain gets no
	// MOOSE_CADDY_IMAGE, so the control-plane compose stays on stock caddy:2-alpine.
	if v := envVal(s.Env, "MOOSE_CADDY_IMAGE"); v != "" {
		t.Errorf("MOOSE_CADDY_IMAGE = %q, want unset when CaddyImage is empty", v)
	}
	if hasMount(s.Mounts, "/var/run/docker.sock", "/var/run/docker.sock") {
		t.Error("brain must NOT mount the raw Docker socket")
	}
	// An unmarked box (no ProfileMarkerPath) gets no marker mount — the brain
	// resolves appliance, the no-op default.
	if hasMount(s.Mounts, "/etc/moose/profile", "/etc/moose/profile") {
		t.Errorf("unexpected profile-marker mount when ProfileMarkerPath is empty: %+v", s.Mounts)
	}
}

// On a marked box host-agent mounts the environment-profile marker read-only at
// the same path so the containerized brain resolves the profile (appliance vs
// hosted) exactly as it would natively — otherwise it can't see /etc/moose and
// always reads appliance, leaving a hosted box's /setup gate disarmed.
func TestLaunchRunSpecProfileMarkerMount(t *testing.T) {
	f := newFake()
	cfg := testConfig()
	cfg.ProfileMarkerPath = "/etc/moose/profile"
	if err := Launch(context.Background(), f, cfg); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	s := f.lastRun
	if !hasMount(s.Mounts, "/etc/moose/profile", "/etc/moose/profile") {
		t.Errorf("missing same-path profile-marker mount: %+v", s.Mounts)
	}
	for _, m := range s.Mounts {
		if m.Source == "/etc/moose/profile" && !m.ReadOnly {
			t.Errorf("profile-marker mount must be read-only: %+v", m)
		}
	}
}

// On a baked, air-gapped box the brain is launched in offline-install mode; unset
// catalog vars leave MOOSE_CATALOG_URL / MOOSE_CATALOG_CACHE_DIR off rather than
// pointing at "" (the brain then falls back to its own defaults).
func TestLaunchRunSpecOfflineAndNoCatalog(t *testing.T) {
	f := newFake()
	cfg := testConfig()
	cfg.OfflineInstall = true
	cfg.CatalogURL = ""
	cfg.CatalogCacheDir = ""
	if err := Launch(context.Background(), f, cfg); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	s := f.lastRun
	if v := envVal(s.Env, "MOOSE_OFFLINE_INSTALL"); v != "true" {
		t.Errorf("MOOSE_OFFLINE_INSTALL = %q, want true", v)
	}
	if v := envVal(s.Env, "MOOSE_CATALOG_URL"); v != "" {
		t.Errorf("MOOSE_CATALOG_URL = %q, want unset when CatalogURL is empty", v)
	}
	if v := envVal(s.Env, "MOOSE_CATALOG_CACHE_DIR"); v != "" {
		t.Errorf("MOOSE_CATALOG_CACHE_DIR = %q, want unset when CatalogCacheDir is empty", v)
	}
	if v := envVal(s.Env, "MOOSE_CATALOG_FILE"); v != "" {
		t.Errorf("MOOSE_CATALOG_FILE = %q, want unset when CatalogFile is empty", v)
	}
}

// The air-gapped lanes stage a snapshot file and point the brain at it, because
// there is no control plane in the VM to sync from. A real box never sets it.
func TestLaunchRunSpecCatalogFile(t *testing.T) {
	f := newFake()
	cfg := testConfig()
	cfg.CatalogFile = "/var/lib/moose/catalog-seed.json"
	if err := Launch(context.Background(), f, cfg); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if v := envVal(f.lastRun.Env, "MOOSE_CATALOG_FILE"); v != "/var/lib/moose/catalog-seed.json" {
		t.Errorf("MOOSE_CATALOG_FILE = %q, want the staged snapshot path", v)
	}
}

// The hosted profile sets CaddyImage; it must reach the brain as MOOSE_CADDY_IMAGE
// so the control-plane compose substitutes the caddy-dns/acmedns build for the
// wildcard cert (os #207/C3b).
func TestLaunchRunSpecCaddyImage(t *testing.T) {
	f := newFake()
	cfg := testConfig()
	cfg.CaddyImage = "moose-caddy-acmedns:dev"
	if err := Launch(context.Background(), f, cfg); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if v := envVal(f.lastRun.Env, "MOOSE_CADDY_IMAGE"); v != "moose-caddy-acmedns:dev" {
		t.Errorf("MOOSE_CADDY_IMAGE = %q, want moose-caddy-acmedns:dev", v)
	}
}

func TestEnsureTransportSeedsNetworkAndProxy(t *testing.T) {
	f := newFake()
	f.exists = false // proxy not yet running

	if err := EnsureTransport(context.Background(), f, testConfig()); err != nil {
		t.Fatalf("EnsureTransport: %v", err)
	}
	if f.netCalls != 1 || f.lastNet != "moose-ingress" {
		t.Errorf("network create calls=%d last=%q, want 1 moose-ingress", f.netCalls, f.lastNet)
	}
	if f.runCalls != 1 {
		t.Fatalf("run calls = %d, want 1 (proxy launched)", f.runCalls)
	}
	s := f.lastRun
	if s.Name != "moose-docker-proxy" || s.Network != "moose-ingress" {
		t.Errorf("proxy spec name/network = %q/%q", s.Name, s.Network)
	}
	// The brain dials the proxy by the docker-proxy alias regardless of the
	// container's moose-prefixed name.
	if len(s.Aliases) != 1 || s.Aliases[0] != "docker-proxy" {
		t.Errorf("proxy aliases = %v, want [docker-proxy]", s.Aliases)
	}
	// The raw socket is mounted read-only into the proxy — the one place it is
	// exposed to a container.
	var sockRO bool
	for _, m := range s.Mounts {
		if m.Source == "/var/run/docker.sock" && m.Target == "/var/run/docker.sock" {
			sockRO = m.ReadOnly
		}
	}
	if !sockRO {
		t.Errorf("proxy socket mount must be read-only: %+v", s.Mounts)
	}
	// EXEC must stay denied (it is absent from the allowlist).
	if v := envVal(s.Env, "EXEC"); v != "" {
		t.Errorf("proxy EXEC = %q, want unset (denied)", v)
	}
	if envVal(s.Env, "POST") != "1" || envVal(s.Env, "CONTAINERS") != "1" {
		t.Errorf("proxy allowlist missing POST/CONTAINERS: %+v", s.Env)
	}
	// The container that holds the raw socket runs the app sandbox: every
	// capability dropped, no-new-privileges (#431). haproxy binds :2375, so it
	// needs nothing added back.
	if len(s.CapDrop) != 1 || s.CapDrop[0] != "ALL" {
		t.Errorf("proxy cap_drop = %v, want [ALL]", s.CapDrop)
	}
	if len(s.SecurityOpt) != 1 || s.SecurityOpt[0] != "no-new-privileges:true" {
		t.Errorf("proxy security_opt = %v, want [no-new-privileges:true]", s.SecurityOpt)
	}
}

func TestEnsureTransportProxyExistsIsNoOp(t *testing.T) {
	f := newFake()
	f.exists = true    // proxy already running (host-agent restart)
	f.sandboxed = true // and already created with the sandbox

	if err := EnsureTransport(context.Background(), f, testConfig()); err != nil {
		t.Fatalf("EnsureTransport: %v", err)
	}
	if f.netCalls != 1 {
		t.Errorf("network create still ensured idempotently: calls=%d, want 1", f.netCalls)
	}
	if f.runCalls != 0 {
		t.Errorf("run calls = %d, want 0 (existing proxy left to Docker)", f.runCalls)
	}
	if f.removeCalls != 0 {
		t.Errorf("remove calls = %d, want 0 (a sandboxed proxy is not recreated)", f.removeCalls)
	}
}

// A proxy created before #431 keeps Docker's full default capability set for as
// long as the container lives — a restart does not re-apply flags. Without this
// path the hardening would reach new boxes only, which is how a change ships and
// does nothing (#404). host-agent recreates it once, then converges.
func TestEnsureTransportRecreatesUnsandboxedProxy(t *testing.T) {
	f := newFake()
	f.exists = true     // proxy from before the sandbox landed
	f.sandboxed = false // …created without it

	if err := EnsureTransport(context.Background(), f, testConfig()); err != nil {
		t.Fatalf("EnsureTransport: %v", err)
	}
	if f.removeCalls != 1 || f.lastRemove != "moose-docker-proxy" {
		t.Fatalf("remove calls=%d last=%q, want 1 moose-docker-proxy", f.removeCalls, f.lastRemove)
	}
	if f.runCalls != 1 {
		t.Fatalf("run calls = %d, want 1 (proxy relaunched hardened)", f.runCalls)
	}
	if len(f.lastRun.CapDrop) != 1 || f.lastRun.CapDrop[0] != "ALL" {
		t.Errorf("relaunched proxy cap_drop = %v, want [ALL]", f.lastRun.CapDrop)
	}
}

// The recreate path removes a working container before it can start its
// replacement, so a run that fails on a busy daemon would cost the box its
// Docker transport for the rest of the boot. It retries; a run that succeeds on
// the second attempt leaves the box with a hardened proxy and no error.
func TestEnsureTransportRetriesTheRecreatedProxy(t *testing.T) {
	prev := proxyRunRetryDelay
	proxyRunRetryDelay = 0
	t.Cleanup(func() { proxyRunRetryDelay = prev })

	f := newFake()
	f.exists = true
	f.sandboxed = false
	f.runErrs = []error{errors.New("docker daemon busy")} // first attempt only

	if err := EnsureTransport(context.Background(), f, testConfig()); err != nil {
		t.Fatalf("EnsureTransport: %v, want the retry to carry it", err)
	}
	if f.runCalls != 2 {
		t.Errorf("run calls = %d, want 2 (one failure, then the retry)", f.runCalls)
	}
}

// A first launch keeps its single attempt: nothing was taken away, so there is
// no transport to lose by giving up.
func TestEnsureTransportFirstLaunchDoesNotRetry(t *testing.T) {
	f := newFake()
	f.exists = false
	f.runErr = errors.New("docker daemon busy")

	if err := EnsureTransport(context.Background(), f, testConfig()); err == nil {
		t.Fatal("EnsureTransport: want the run error to propagate")
	}
	if f.runCalls != 1 {
		t.Errorf("run calls = %d, want 1 (a first launch does not retry)", f.runCalls)
	}
}

// The sandbox check must never be the reason a box fails to boot: an unreadable
// container leaves the proxy exactly as it was, and the boot carries on.
func TestEnsureTransportSandboxCheckFailureKeepsProxy(t *testing.T) {
	f := newFake()
	f.exists = true
	f.sandboxedErr = errors.New("docker inspect: no such object")

	if err := EnsureTransport(context.Background(), f, testConfig()); err != nil {
		t.Fatalf("EnsureTransport: %v, want the boot to carry on", err)
	}
	if f.removeCalls != 0 || f.runCalls != 0 {
		t.Errorf("remove=%d run=%d, want 0/0 (an unreadable proxy is left alone)", f.removeCalls, f.runCalls)
	}
}

func TestEnsureTransportLoadsAbsentProxyImage(t *testing.T) {
	f := newFake()
	f.present = false // proxy image not loaded yet

	cfg := testConfig()
	if err := EnsureTransport(context.Background(), f, cfg); err != nil {
		t.Fatalf("EnsureTransport: %v", err)
	}
	if f.loadCalls != 1 {
		t.Fatalf("load calls = %d, want 1 (absent proxy image loaded)", f.loadCalls)
	}
	if f.loadPaths[0] != cfg.ProxyImageTar {
		t.Errorf("loaded %q, want the proxy tarball %q", f.loadPaths[0], cfg.ProxyImageTar)
	}
	// A regression that loads the image and returns early (skipping Run) must
	// fail here — loading is not the end of the absent-image path, launching is.
	if f.runCalls != 1 {
		t.Fatalf("run calls = %d, want 1 (proxy launched after load)", f.runCalls)
	}
	if f.lastRun.Name != "moose-docker-proxy" {
		t.Errorf("ran %q, want the proxy after loading its image", f.lastRun.Name)
	}
}

// TestFirstBootSequenceLoadsEachImageFromItsOwnTarball mirrors host-agent's
// first-boot order — EnsureTransport then Launch — on a fresh box where both the
// proxy and brain images are absent. It proves each is loaded from its *own*
// tarball (proxy ← ProxyImageTar, brain ← ImageTar), which a single shared
// present bool can't express: the per-ref fake distinguishes the two refs the
// way production's CLIDocker does.
func TestFirstBootSequenceLoadsEachImageFromItsOwnTarball(t *testing.T) {
	cfg := testConfig()
	f := newFake()
	f.presentByRef = map[string]bool{
		cfg.ProxyImage: false, // both absent on a fresh box → both load
		cfg.Image:      false,
	}

	if err := EnsureTransport(context.Background(), f, cfg); err != nil {
		t.Fatalf("EnsureTransport: %v", err)
	}
	if err := Launch(context.Background(), f, cfg); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	want := []string{cfg.ProxyImageTar, cfg.ImageTar}
	if len(f.loadPaths) != len(want) {
		t.Fatalf("load paths = %v, want %v", f.loadPaths, want)
	}
	for i, w := range want {
		if f.loadPaths[i] != w {
			t.Errorf("load[%d] = %q, want %q", i, f.loadPaths[i], w)
		}
	}
	// Proxy then brain — two distinct containers, in order.
	if len(f.runSpecs) != 2 || f.runSpecs[0].Name != "moose-docker-proxy" || f.runSpecs[1].Name != "moose-brain" {
		t.Errorf("ran %d containers (%+v), want [moose-docker-proxy moose-brain]", len(f.runSpecs), f.runSpecs)
	}
}

func TestEnsureTransportNetworkErrorPropagates(t *testing.T) {
	f := newFake()
	f.netErr = errors.New("daemon down")

	if err := EnsureTransport(context.Background(), f, testConfig()); err == nil {
		t.Fatal("want error when network create fails")
	}
	if f.runCalls != 0 {
		t.Errorf("run calls = %d, want 0 after a network-create failure", f.runCalls)
	}
}

func TestLaunchProtocolMismatchRefuses(t *testing.T) {
	f := newFake()
	f.label = "2" // brain speaks a major this host-agent doesn't

	err := Launch(context.Background(), f, testConfig())
	if !errors.Is(err, ErrProtocolMismatch) {
		t.Fatalf("err = %v, want ErrProtocolMismatch", err)
	}
	if f.runCalls != 0 {
		t.Errorf("run calls = %d, want 0 (mismatch must refuse launch)", f.runCalls)
	}
}

func TestLaunchMissingLabelRefuses(t *testing.T) {
	f := newFake()
	f.label = "" // image carries no protocol label → cannot verify → refuse

	err := Launch(context.Background(), f, testConfig())
	if !errors.Is(err, ErrProtocolMismatch) {
		t.Fatalf("err = %v, want ErrProtocolMismatch", err)
	}
	if f.runCalls != 0 {
		t.Errorf("run calls = %d, want 0", f.runCalls)
	}
}

func TestLaunchContainerExistsIsNoOp(t *testing.T) {
	f := newFake()
	f.exists = true // brain already running (host-agent restart)

	if err := Launch(context.Background(), f, testConfig()); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if f.runCalls != 0 {
		t.Errorf("run calls = %d, want 0 (existing brain left to Docker)", f.runCalls)
	}
}

func TestLaunchLoadErrorPropagates(t *testing.T) {
	f := newFake()
	f.present = false
	f.loadErr = errors.New("disk full")

	err := Launch(context.Background(), f, testConfig())
	if err == nil {
		t.Fatal("want error when docker load fails")
	}
	if f.runCalls != 0 {
		t.Errorf("run calls = %d, want 0 (no run after a failed load)", f.runCalls)
	}
}

func TestLaunchImagePresentErrorPropagates(t *testing.T) {
	f := newFake()
	f.presentErr = errors.New("docker daemon unreachable")

	if err := Launch(context.Background(), f, testConfig()); err == nil {
		t.Fatal("want error when the image check fails")
	}
	if f.loadCalls != 0 || f.runCalls != 0 {
		t.Errorf("load/run calls = %d/%d, want 0/0 on an image-check error", f.loadCalls, f.runCalls)
	}
}

func TestLaunchContainerCheckErrorPropagates(t *testing.T) {
	f := newFake()
	f.existsErr = errors.New("docker ps failed")

	if err := Launch(context.Background(), f, testConfig()); err == nil {
		t.Fatal("want error when the container check fails")
	}
	if f.runCalls != 0 {
		t.Errorf("run calls = %d, want 0 when the container check errors", f.runCalls)
	}
}

func TestLaunchRunErrorPropagates(t *testing.T) {
	f := newFake()
	f.runErr = errors.New("no such image")

	if err := Launch(context.Background(), f, testConfig()); err == nil {
		t.Fatal("want error when docker run fails")
	}
	if f.runCalls != 1 {
		t.Errorf("run calls = %d, want 1 (run was attempted)", f.runCalls)
	}
}

// TestProxyAllowlistIsMinimal pins the socket-proxy allowlist to the exact set
// #430 measured as load-bearing. The proxy filters by URL prefix and method
// only — it never reads request bodies — so granting CONTAINERS+POST is already
// a host-root escape for anyone who can reach :2375 (a privileged, host-bind
// POST /containers/create passes through). Widening this set makes that worse
// and can only reduce again by dropping a family the brain needs. So this is a
// guard, not a snapshot: a diff that changes it should send the author back to
// #430 and CONTROL_PLANE.md # Locked: Docker socket exposure. In particular EXEC
// must never appear here — it is denied precisely by being absent.
func TestProxyAllowlistIsMinimal(t *testing.T) {
	want := map[string]string{
		"POST": "1", "PING": "1", "VERSION": "1", "INFO": "1",
		"CONTAINERS": "1", "IMAGES": "1", "NETWORKS": "1", "VOLUMES": "1",
	}
	got := map[string]string{}
	for _, e := range proxyAllowlist() {
		if _, dup := got[e.Key]; dup {
			t.Errorf("duplicate allowlist key %q", e.Key)
		}
		got[e.Key] = e.Value
	}
	if _, banned := got["EXEC"]; banned {
		t.Error("EXEC is in the allowlist — it must stay denied (#430); managed DB runs a one-shot container, not docker exec")
	}
	if len(got) != len(want) {
		t.Fatalf("allowlist keys = %v, want %v", keysOf(got), keysOf(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("allowlist[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func keysOf(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func hasMount(ms []Mount, src, tgt string) bool {
	for _, m := range ms {
		if m.Source == src && m.Target == tgt {
			return true
		}
	}
	return false
}

func envVal(es []EnvVar, key string) string {
	for _, e := range es {
		if e.Key == key {
			return e.Value
		}
	}
	return ""
}
