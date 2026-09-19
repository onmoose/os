// Package journalsource is host-agent-real's per-app log tail
// (LOGGING.md # Per-app logs, BRAIN_HOST_PROTOCOL.md # Pattern C). It backs
// GET /v1/journal/follow by shelling out to
// `journalctl CONTAINER_NAME=<container> -f -o json`, parsing each JSON entry
// into a protocol.JournalLine, and streaming them over a channel until the
// follow context is cancelled (the brain disconnected) or journalctl exits.
//
// It relies on Docker's journald log driver, which tags every container line
// with CONTAINER_NAME and encodes the std stream in PRIORITY (stdout = 6,
// stderr = 3). The brain passes the container name it computes from the
// compose project + main service (moose-<id>-<service>) — the override pins
// the running container to that exact name, so the exact match is reliable.
package journalsource

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"os/exec"
	"strconv"
	"time"

	"github.com/onmoose/os/internal/protocol"
)

// Reader implements hostagent.LogSource over real journalctl.
type Reader struct{}

// New returns a Reader backed by `journalctl -f`.
func New() *Reader { return &Reader{} }

// tailLines is how many existing lines journalctl emits before it starts
// following. It gives a fresh Logs-tab open immediate history (the brain hub's
// rolling buffer is empty on its first subscriber) without unbounded backfill.
const tailLines = 100

// entry is the subset of journalctl's `-o json` fields we read. MESSAGE is a
// RawMessage because journald serialises it as a JSON string for UTF-8 text and
// as an array of byte values for anything non-UTF-8; we handle both.
type entry struct {
	Realtime string          `json:"__REALTIME_TIMESTAMP"`
	Priority string          `json:"PRIORITY"`
	Message  json.RawMessage `json:"MESSAGE"`
}

// Follow starts `journalctl CONTAINER_NAME=<container> -f -o json` and streams
// parsed lines until ctx is cancelled or journalctl exits. The command is bound
// to ctx (exec.CommandContext), so a brain disconnect kills the process; the
// returned channel is closed when the stream ends.
func (r *Reader) Follow(ctx context.Context, container string) (<-chan protocol.JournalLine, error) {
	cmd := exec.CommandContext(ctx, "journalctl",
		"CONTAINER_NAME="+container,
		"-f", "-o", "json",
		"-n", strconv.Itoa(tailLines),
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	ch := make(chan protocol.JournalLine)
	go func() {
		defer close(ch)
		// Reap the process so cancellation doesn't leave a zombie. ctx-kill
		// surfaces here as a non-nil Wait error, which is expected and ignored.
		defer func() { _ = cmd.Wait() }()

		sc := bufio.NewScanner(stdout)
		// journald entries can exceed bufio's 64 KiB default line cap; raise it
		// so a long log line isn't silently truncated mid-stream.
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line, ok := parseEntry(sc.Bytes())
			if !ok {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case ch <- line:
			}
		}
		if err := sc.Err(); err != nil && ctx.Err() == nil {
			slog.Error("journalsource: scan error", "container", container, "err", err)
		}
	}()
	return ch, nil
}

// parseEntry turns one journalctl JSON line into a protocol.JournalLine. It
// returns ok=false for an unparseable line (skip it rather than abort the
// stream). The std stream is inferred from PRIORITY: Docker's journald driver
// writes stderr at priority 3 and stdout at 6.
func parseEntry(b []byte) (protocol.JournalLine, bool) {
	var e entry
	if err := json.Unmarshal(b, &e); err != nil {
		return protocol.JournalLine{}, false
	}
	stream := "stdout"
	if e.Priority == "3" {
		stream = "stderr"
	}
	return protocol.JournalLine{
		Ts:     realtimeToRFC3339(e.Realtime),
		Stream: stream,
		Line:   decodeMessage(e.Message),
	}, true
}

// realtimeToRFC3339 converts journald's __REALTIME_TIMESTAMP (microseconds since
// the Unix epoch, as a string) to RFC3339 UTC. An unparseable value yields an
// empty timestamp rather than a bogus time.
func realtimeToRFC3339(us string) string {
	n, err := strconv.ParseInt(us, 10, 64)
	if err != nil {
		return ""
	}
	return time.UnixMicro(n).UTC().Format(time.RFC3339)
}

// decodeMessage reads journald's MESSAGE field, which is a JSON string for
// UTF-8 text and a JSON array of byte values for non-UTF-8 payloads. Anything
// else yields an empty line.
func decodeMessage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var bytesArr []byte
	if err := json.Unmarshal(raw, &bytesArr); err == nil {
		return string(bytesArr)
	}
	return ""
}
