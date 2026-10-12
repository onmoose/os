package updatetarget

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LocaltimePath is the file that names the box's time zone. `timedatectl
// set-timezone` points it at the zone's file under /usr/share/zoneinfo
// (TIME.md # Locked: timezone).
const LocaltimePath = "/etc/localtime"

// SystemZone reads the box's time zone as it is now, for Loop.Zone.
//
// Go reads /etc/localtime once, at start, into time.Local, and nothing
// refreshes it. So a host-agent started before first-run set the zone would
// keep reading the window in the old zone until the next reboot, and
// time.Local cannot even say its own name ("Local"). Reading the link on every
// tick fixes both: the window follows the zone the owner chose, and the
// dashboard can name it (#488).
//
// When the link cannot be read or names no zone the box knows, it falls back
// to time.Local with no name, which is what the loop did before.
func SystemZone() (*time.Location, string) {
	return zoneFromLink(LocaltimePath)
}

func zoneFromLink(path string) (*time.Location, string) {
	if tz := os.Getenv("TZ"); tz != "" {
		// TZ wins over /etc/localtime for Go and for every other program, so
		// it wins here too.
		if loc, err := time.LoadLocation(strings.TrimPrefix(tz, ":")); err == nil {
			return loc, loc.String()
		}
		return time.Local, ""
	}
	target, err := os.Readlink(path)
	if err != nil {
		return time.Local, ""
	}
	name := zoneName(target)
	if name == "" {
		return time.Local, ""
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.Local, ""
	}
	return loc, name
}

// zoneName is the IANA name in a link target such as
// "../usr/share/zoneinfo/Europe/Stockholm", or "" when it has none.
func zoneName(target string) string {
	target = filepath.ToSlash(filepath.Clean(target))
	_, name, ok := strings.Cut(target, "zoneinfo/")
	if !ok {
		return ""
	}
	return name
}
