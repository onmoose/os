package lifecycle

// The image tier's user (APP_ISOLATION.md # User-namespace tiers). An app that
// declares image_user: true runs as the user its image sets, with no user:
// pin, so the brain must know that user's ids to give each bind dir the right
// owner: base+uid:base+gid on the host.
//
// The user comes from the pulled image's Config.User (the Dockerfile USER).
// A number is used as it is. A name is looked up in the image's own
// /etc/passwd and /etc/group, the same files Docker reads when it starts the
// container. The brain reads them without starting the image: it creates a
// container, copies the two files out and removes it, so no code from the
// image runs.
//
// Why reading the image's files is safe here: whatever they say, the result
// is only used as an offset inside the remap range, and it must be below
// remapIDCount. So a name can only ever give what a numeric USER could
// already give, and the image tier exists only on a remapped daemon. The
// image cannot name a real host account this way.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// imageIDs is one service's image user as in-container ids.
type imageIDs struct{ uid, gid int }

// resolveImageUsers reads the image user of every service and turns it into
// in-container ids. It runs after resolveImages pulled every image, and it
// refuses the install with a plain message when a user cannot be resolved.
func (m *Manager) resolveImageUsers(ctx context.Context, instanceID string, pins []servicePin) (map[string]imageIDs, error) {
	out := make(map[string]imageIDs, len(pins))
	for _, p := range pins {
		ref := p.PinnedRef()
		spec, err := m.docker.ImageUser(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("read the image user of service %q: %w", p.Service, err)
		}
		files := func() ([]byte, []byte, error) {
			return m.docker.ImageUserFiles(ctx, instanceID, ref)
		}
		ids, err := resolveImageUser(p.Service, spec, files)
		if err != nil {
			return nil, err
		}
		slog.Info("image user resolved",
			"instance_id", instanceID, "service", p.Service, "image", ref, "image_user", spec, "uid", ids.uid, "gid", ids.gid)
		out[p.Service] = ids
	}
	return out, nil
}

// resolveImageUser turns one image's Config.User into in-container ids, the
// way Docker does when it starts the container:
//
//   - "" is root, 0:0.
//   - "uid:gid" with two numbers is used as it is. No file is read.
//   - A user name is looked up in /etc/passwd, and gives its uid and its gid.
//   - A numeric uid with no group takes the gid of its /etc/passwd entry, or
//     0 when there is none.
//   - A group name is looked up in /etc/group.
//
// files is called only when a file is needed. A name that is not in the file,
// a file that is missing, or an id at or above remapIDCount refuses the
// install.
func resolveImageUser(service, spec string, files func() (passwd, group []byte, err error)) (imageIDs, error) {
	userPart, groupPart, _ := strings.Cut(spec, ":")
	// "1001:" is "1001", as for Docker.
	hasGroup := groupPart != ""
	uid, uidNum := parseID(userPart)
	if userPart == "" {
		uid, uidNum = 0, true
	}
	gid, gidNum := parseID(groupPart)
	if uidNum && (!hasGroup && userPart == "" || hasGroup && gidNum) {
		if !hasGroup {
			gid = 0
		}
		return checkImageIDs(service, spec, imageIDs{uid, gid})
	}

	passwd, group, err := files()
	if err != nil {
		return imageIDs{}, fmt.Errorf("read the user list of the image of service %q: %w", service, err)
	}
	defaultGID := 0
	if uidNum {
		if e, ok := findPasswd(passwd, func(e passwdEntry) bool { return e.uid == uid }); ok {
			defaultGID = e.gid
		}
	} else {
		e, ok := findPasswd(passwd, func(e passwdEntry) bool { return e.name == userPart })
		if !ok {
			return imageIDs{}, fmt.Errorf("the image of service %q runs as the user %q, and that user is not in the image's own user list, so moose cannot tell who should own its data. This app cannot be installed", service, userPart)
		}
		uid, defaultGID = e.uid, e.gid
	}
	switch {
	case !hasGroup:
		gid = defaultGID
	case gidNum:
	default:
		g, ok := findGroup(group, groupPart)
		if !ok {
			return imageIDs{}, fmt.Errorf("the image of service %q runs with the group %q, and that group is not in the image's own group list, so moose cannot tell who should own its data. This app cannot be installed", service, groupPart)
		}
		gid = g
	}
	return checkImageIDs(service, spec, imageIDs{uid, gid})
}

// checkImageIDs refuses ids the remap range cannot hold.
func checkImageIDs(service, spec string, ids imageIDs) (imageIDs, error) {
	if ids.uid < 0 || ids.gid < 0 || ids.uid >= remapIDCount || ids.gid >= remapIDCount {
		return imageIDs{}, fmt.Errorf("the image of service %q runs as %q, which is user id %d and group id %d. moose gives apps ids below %d only, so this app cannot be installed", service, spec, ids.uid, ids.gid, remapIDCount)
	}
	return ids, nil
}

// parseID reads a user or group id: digits only, no sign.
func parseID(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

type passwdEntry struct {
	name     string
	uid, gid int
}

// findPasswd returns the first /etc/passwd line that matches, like Docker's
// own lookup. A line that is too short, or has an id that is not a number, is
// skipped.
func findPasswd(passwd []byte, match func(passwdEntry) bool) (passwdEntry, bool) {
	sc := bufio.NewScanner(bytes.NewReader(passwd))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ":")
		if len(f) < 4 {
			continue
		}
		uid, ok1 := parseID(f[2])
		gid, ok2 := parseID(f[3])
		if !ok1 || !ok2 {
			continue
		}
		if e := (passwdEntry{name: f[0], uid: uid, gid: gid}); match(e) {
			return e, true
		}
	}
	return passwdEntry{}, false
}

// findGroup returns the gid of a group name in /etc/group.
func findGroup(group []byte, name string) (int, bool) {
	sc := bufio.NewScanner(bytes.NewReader(group))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ":")
		if len(f) < 3 || f[0] != name {
			continue
		}
		if gid, ok := parseID(f[2]); ok {
			return gid, true
		}
	}
	return 0, false
}
