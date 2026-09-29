package usermgr

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// remapAccount is the system account whose subordinate id range Docker's
// daemon-wide userns-remap uses ("userns-remap": "moose-remap" in daemon.json,
// BUILD.md # User-namespace remap).
const remapAccount = "moose-remap"

// minRemapCount is the smallest moose-remap range RemapBase accepts: the whole
// 16-bit id space a container expects, 0 to 65535. The brain computes host
// owners as base+uid for in-container ids up to 65535 (a service_user id from
// 2100, an image's own user such as 999 or 65534), and an id outside the range
// does not map into the container at all. The images write exactly this size.
const minRemapCount = 65536

// Default paths of the subordinate id files. LinuxUserManager.SubUIDPath and
// SubGIDPath override them in tests.
const (
	defaultSubUIDPath = "/etc/subuid"
	defaultSubGIDPath = "/etc/subgid"
)

// subIDRange is one "name:start:count" line of a subuid or subgid file.
type subIDRange struct{ start, count int }

// RemapBase implements hostagent.UserManager. It reads the moose-remap lines of
// /etc/subuid and /etc/subgid and returns the first host id of the range, the
// remap base (BRAIN_HOST_PROTOCOL.md # User info endpoints).
//
//   - Both files name the same range: ok is true and base is its start.
//   - Neither file has a moose-remap line (or a file is missing): ok is false.
//     That means the box runs no remap.
//   - Only one file has the line, the two ranges differ, a range is smaller
//     than minRemapCount, or a line is malformed: an error. host-agent answers
//     an error rather than a guess, because the brain gives bind dirs to owners
//     computed from this number.
func (m *LinuxUserManager) RemapBase() (base int, ok bool, err error) {
	uids, uidOK, err := readSubIDRange(pathOr(m.SubUIDPath, defaultSubUIDPath), remapAccount)
	if err != nil {
		return 0, false, err
	}
	gids, gidOK, err := readSubIDRange(pathOr(m.SubGIDPath, defaultSubGIDPath), remapAccount)
	if err != nil {
		return 0, false, err
	}
	switch {
	case !uidOK && !gidOK:
		return 0, false, nil
	case uidOK != gidOK:
		return 0, false, fmt.Errorf("usermgr: %s has a subordinate range in only one of the subuid and subgid files", remapAccount)
	case uids != gids:
		return 0, false, fmt.Errorf("usermgr: %s subuid range %d:%d and subgid range %d:%d differ", remapAccount, uids.start, uids.count, gids.start, gids.count)
	case uids.count < minRemapCount:
		return 0, false, fmt.Errorf("usermgr: %s range has %d ids, want at least %d", remapAccount, uids.count, minRemapCount)
	}
	return uids.start, true, nil
}

func pathOr(p, def string) string {
	if p == "" {
		return def
	}
	return p
}

// readSubIDRange reads one subordinate id file. A missing file is the same as
// a file with no line for name.
func readSubIDRange(path, name string) (r subIDRange, found bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return subIDRange{}, false, nil
	}
	if err != nil {
		return subIDRange{}, false, fmt.Errorf("usermgr: read %s: %w", path, err)
	}
	r, found, err = parseSubIDRange(b, name)
	if err != nil {
		return subIDRange{}, false, fmt.Errorf("usermgr: %s: %w", path, err)
	}
	return r, found, nil
}

// parseSubIDRange finds the line for name in the content of a subuid or subgid
// file (subuid(5): "name:start:count", one per line). Lines for other accounts
// are skipped. A line for name that is not three fields of a name and two
// whole numbers above zero is an error. A start of 0 would map a container's
// root to real root. A second line for name is an error too: Docker would read
// the ranges together, and one base could not describe them.
func parseSubIDRange(content []byte, name string) (r subIDRange, found bool, err error) {
	sc := bufio.NewScanner(bytes.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if fields[0] != name {
			continue
		}
		if found {
			return subIDRange{}, false, fmt.Errorf("more than one line for %s", name)
		}
		if len(fields) != 3 {
			return subIDRange{}, false, fmt.Errorf("malformed line for %s: %q", name, line)
		}
		s, errS := strconv.Atoi(fields[1])
		c, errC := strconv.Atoi(fields[2])
		if errS != nil || errC != nil || s <= 0 || c <= 0 {
			return subIDRange{}, false, fmt.Errorf("malformed line for %s: %q", name, line)
		}
		r, found = subIDRange{start: s, count: c}, true
	}
	if err := sc.Err(); err != nil {
		return subIDRange{}, false, err
	}
	return r, found, nil
}
