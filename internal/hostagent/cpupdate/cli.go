package cpupdate

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/onmoose/os/internal/hostagent/brainlaunch"
)

// CLIDocker is the production Docker, a thin wrapper over the `docker` CLI —
// the same approach brainlaunch and internal/lifecycle already take. It is
// exercised by the QEMU lane, not by unit tests: there is no daemon in the Go
// test environment, so the transaction itself is covered against the Docker
// interface with a fake.
type CLIDocker struct{}

// NewCLIDocker returns the production Docker backed by the `docker` CLI.
func NewCLIDocker() CLIDocker { return CLIDocker{} }

func (CLIDocker) Pull(ctx context.Context, ref string) error {
	if out, err := exec.CommandContext(ctx, "docker", "pull", ref).CombinedOutput(); err != nil {
		return fmt.Errorf("docker pull %s: %w\n%s", ref, err, out)
	}
	return nil
}

// RemoveContainer force-removes by name. `docker rm -f` stops a running
// container first, and an absent container is success, not failure: the caller
// wants "there is no container by this name" and does not care whether it had
// to do anything to get there.
func (CLIDocker) RemoveContainer(ctx context.Context, name string) error {
	out, err := exec.CommandContext(ctx, "docker", "rm", "-f", name).CombinedOutput()
	if err != nil && strings.Contains(string(out), "No such container") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("docker rm -f %s: %w\n%s", name, err, out)
	}
	return nil
}

// ImageLabel delegates to brainlaunch's CLI so the lockstep check reads the
// label exactly the way the first-boot path reads it.
func (CLIDocker) ImageLabel(ctx context.Context, ref, label string) (string, error) {
	return brainlaunch.NewCLIDocker().ImageLabel(ctx, ref, label)
}

// Run delegates to brainlaunch's own CLI, so an updated brain is started by
// exactly the code that starts it at boot rather than by a second `docker run`
// builder that could drift from it.
func (CLIDocker) Run(ctx context.Context, spec brainlaunch.RunSpec) error {
	return brainlaunch.NewCLIDocker().Run(ctx, spec)
}

func (CLIDocker) ComposeUp(ctx context.Context, dir, project string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "compose", "-f", "compose.yml", "-p", project, "up", "-d")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ContainerIP reads the container's first network address. The control-plane
// containers publish no ports and sit on the ingress network, so this address
// is how anything on the host reaches them.
func (CLIDocker) ContainerIP(ctx context.Context, name string) (string, error) {
	const format = `{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}`
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", format, name).Output()
	if err != nil {
		return "", fmt.Errorf("docker inspect %s: %w", name, err)
	}
	for _, ip := range strings.Fields(string(out)) {
		if ip != "" {
			return ip, nil
		}
	}
	return "", fmt.Errorf("container %s has no network address", name)
}

// RemoveImage drops an image past its retention window. Docker refuses to
// remove an image a container still uses, and that refusal is correct — it is
// surfaced as an error and logged, never forced.
func (CLIDocker) RemoveImage(ctx context.Context, ref string) error {
	if out, err := exec.CommandContext(ctx, "docker", "rmi", ref).CombinedOutput(); err != nil {
		return fmt.Errorf("docker rmi %s: %w\n%s", ref, err, out)
	}
	return nil
}

// HTTPProber polls a URL until it answers or the context is done.
//
// **Any HTTP response counts as serving**, including a 404 or a 500. The
// question this answers is "did the new container come up and bind its port",
// not "is every route behaving" — a brain that answers 500 on some endpoint is
// still a running brain, and reverting the control plane over it would take a
// working box backwards. The brain's own probe is /healthz, which returns 200
// as soon as it is serving (internal/api/healthz.go).
type HTTPProber struct {
	// Interval between attempts. Zero uses one second, which is what the 60s
	// budget in UPDATES.md # 3 step 3d was sized against.
	Interval time.Duration
}

func (p HTTPProber) WaitServing(ctx context.Context, url string) error {
	interval := p.Interval
	if interval <= 0 {
		interval = time.Second
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
		// The probe targets a container address directly; a redirect would send
		// it somewhere that says nothing about whether this container is up.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext},
	}

	var last error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			return nil
		}
		last = err

		select {
		case <-ctx.Done():
			// Report the transport error, not "context deadline exceeded" —
			// "connection refused" tells an operator the container never bound
			// its port, which is the thing they need to know.
			return fmt.Errorf("%s not serving: %w", url, last)
		case <-time.After(interval):
		}
	}
}
