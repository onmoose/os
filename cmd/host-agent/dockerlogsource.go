package main

// dockerLogSource implements hostagent.LogSource via `docker logs -f
// --timestamps`, giving the fake host-agent real container output in the dev
// inner loop without needing journald or Docker's journald log driver.
//
// Docker compose v2 appends a replica number to container names (e.g.
// moose-<id>-<service>-1), but the brain resolves the unsuffixed stem from the
// manifest. We probe the replica form first with `docker inspect` and fall back
// to the bare stem so both standalone and compose containers work.

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"

	"github.com/onmoose/os/internal/protocol"
)

type dockerLogSource struct{}

func (d *dockerLogSource) Follow(ctx context.Context, container string) (<-chan protocol.JournalLine, error) {
	name := container + "-1"
	if !dockerContainerExists(ctx, name) {
		name = container
	}

	cmd := exec.CommandContext(ctx, "docker", "logs", "-f", "--timestamps", name)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	ch := make(chan protocol.JournalLine)
	var wg sync.WaitGroup

	scan := func(r io.Reader, stream string) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			ts, line := splitDockerTimestamp(sc.Text())
			select {
			case ch <- protocol.JournalLine{Ts: ts, Stream: stream, Line: line}:
			case <-ctx.Done():
				return
			}
		}
		if err := sc.Err(); err != nil && ctx.Err() == nil {
			slog.Warn("dockerlogsource: scan error", "container", name, "stream", stream, "err", err)
		}
	}

	wg.Add(2)
	go scan(stdout, "stdout")
	go scan(stderr, "stderr")

	go func() {
		wg.Wait()
		close(ch)
		_ = cmd.Wait()
	}()

	return ch, nil
}

// dockerContainerExists reports whether a container with the given name is
// known to Docker (running or stopped).
func dockerContainerExists(ctx context.Context, name string) bool {
	return exec.CommandContext(ctx, "docker", "inspect", "--format", ".", name).Run() == nil
}

// splitDockerTimestamp splits a `docker logs --timestamps` line at the first
// space into an RFC3339Nano timestamp and the log text. Returns an empty
// timestamp if the prefix is absent.
func splitDockerTimestamp(s string) (ts, line string) {
	i := strings.IndexByte(s, ' ')
	if i < 0 {
		return "", s
	}
	return s[:i], s[i+1:]
}
