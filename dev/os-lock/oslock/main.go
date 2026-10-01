// Command oslock reads and writes the OS package lock in dev/os-lock/
// (BUILD.md # 1b # The OS package lock, #560).
//
// The lock has three files. debian-snapshot holds the snapshot.debian.org
// timestamp the image installs from. third-party.lock pins each package from a
// repo that is not in that snapshot (Docker's) to an exact version. And
// cloud-packages.lock is the package list the hosted image resolved to, one
// "name version architecture" line per package, made from mkosi's JSON
// manifest. The build compares a fresh manifest with that file and fails when
// they differ; the scheduled bump workflow (.github/workflows/os-lock-bump.yml)
// writes a new one and names the changes in its PR.
//
// Subcommands:
//
//	oslock normalize MANIFEST.json              print the lock file for a manifest
//	oslock check MANIFEST.json LOCK             exit 1, with the changes, if they differ
//	oslock title [-security P] OLD NEW          one-line PR title naming the changes
//	oslock diff [-security P] OLD NEW           Markdown table of the changes
//	oslock newest-pins LOCK DOCKER_PACKAGES     third-party.lock moved to the newest
//	                                            version within each pin's major
//
// P is a Debian Packages index of trixie-security at the new snapshot. A changed
// package whose new version is in it is marked as a security update.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		var mismatch errMismatch
		if errors.As(err, &mismatch) {
			fmt.Fprint(os.Stderr, string(mismatch))
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "oslock:", err)
		os.Exit(2)
	}
}

// errMismatch is the one error a caller acts on: the build resolved to a
// different package list than the committed lock. Its text is the report.
type errMismatch string

func (e errMismatch) Error() string { return string(e) }

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: oslock normalize|check|title|diff|newest-pins ...")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "normalize":
		if len(rest) != 1 {
			return errors.New("usage: oslock normalize MANIFEST.json")
		}
		pkgs, err := readManifest(rest[0])
		if err != nil {
			return err
		}
		_, err = io.WriteString(out, formatLock(pkgs))
		return err
	case "check":
		if len(rest) != 2 {
			return errors.New("usage: oslock check MANIFEST.json LOCK")
		}
		got, err := readManifest(rest[0])
		if err != nil {
			return err
		}
		want, err := readLockFile(rest[1])
		if err != nil {
			return err
		}
		changes := compare(want, got)
		if len(changes) == 0 {
			fmt.Fprintf(out, "package lock check passed: %d packages match %s\n", len(got), rest[1])
			return nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "PACKAGE LOCK CHECK FAILED: the build resolved to a different package list than %s (%d changes).\n", rest[1], len(changes))
		for _, c := range changes {
			fmt.Fprintf(&b, "  %s\n", c.String())
		}
		b.WriteString("\nThe image installs from the snapshot in dev/os-lock/debian-snapshot and the pins in\n" +
			"dev/os-lock/third-party.lock, so this list must not move by itself. If you changed the\n" +
			"package list, the snapshot or a pin on purpose, update the lock with the resolved list\n" +
			"this build wrote to .dev/cloud/cloud-packages.lock (CI uploads it as an artifact).\n")
		return errMismatch(b.String())
	case "title", "diff":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		security := fs.String("security", "", "Debian Packages index of trixie-security at the new snapshot")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.NArg() != 2 {
			return fmt.Errorf("usage: oslock %s [-security PACKAGES] OLD NEW", cmd)
		}
		old, err := readLockFile(fs.Arg(0))
		if err != nil {
			return err
		}
		cur, err := readLockFile(fs.Arg(1))
		if err != nil {
			return err
		}
		sec := map[string]map[string]bool{}
		if *security != "" {
			f, err := os.Open(*security)
			if err != nil {
				return err
			}
			defer f.Close()
			if sec, err = parsePackagesIndex(f); err != nil {
				return err
			}
		}
		changes := compare(old, cur)
		markSecurity(changes, sec)
		if cmd == "title" {
			_, err = fmt.Fprintln(out, title(changes))
		} else {
			_, err = io.WriteString(out, markdown(changes))
		}
		return err
	case "newest-pins":
		if len(rest) != 2 {
			return errors.New("usage: oslock newest-pins LOCK DOCKER_PACKAGES")
		}
		lock, err := os.ReadFile(rest[0])
		if err != nil {
			return err
		}
		f, err := os.Open(rest[1])
		if err != nil {
			return err
		}
		defer f.Close()
		index, err := parsePackagesIndex(f)
		if err != nil {
			return err
		}
		next, err := newestPins(string(lock), index)
		if err != nil {
			return err
		}
		_, err = io.WriteString(out, next)
		return err
	}
	return fmt.Errorf("unknown subcommand %q", cmd)
}

// pkg is one installed package. The key is name plus architecture, because a
// multi-arch package can be installed once per architecture.
type pkg struct {
	Name, Version, Arch string
}

func (p pkg) key() string { return p.Name + ":" + p.Arch }

func readManifest(path string) ([]pkg, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m struct {
		Packages []struct {
			Name         string `json:"name"`
			Version      string `json:"version"`
			Architecture string `json:"architecture"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(m.Packages) == 0 {
		return nil, fmt.Errorf("%s: the manifest lists no packages", path)
	}
	var pkgs []pkg
	for _, p := range m.Packages {
		if p.Name == "" || p.Version == "" || p.Architecture == "" {
			return nil, fmt.Errorf("%s: a package entry is missing its name, version or architecture", path)
		}
		pkgs = append(pkgs, pkg{p.Name, p.Version, p.Architecture})
	}
	return pkgs, nil
}

const lockHeader = `# The package list the hosted cloud image resolved to (dev/cloud/mkosi.conf),
# installed from the snapshot in debian-snapshot and the pins in third-party.lock.
# Generated from mkosi's JSON manifest by dev/os-lock/oslock; do not edit by hand.
# The build fails when it resolves to a different list (BUILD.md # 1b # The OS
# package lock). One package per line: name version architecture.
`

func formatLock(pkgs []pkg) string {
	sorted := append([]pkg(nil), pkgs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].key() < sorted[j].key() })
	var b strings.Builder
	b.WriteString(lockHeader)
	for _, p := range sorted {
		fmt.Fprintf(&b, "%s %s %s\n", p.Name, p.Version, p.Arch)
	}
	return b.String()
}

func readLockFile(path string) ([]pkg, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	pkgs, err := parseLock(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return pkgs, nil
}

// parseLock reads a lock file. Comments and blank lines are ignored, so a
// change to the header is never a package change.
func parseLock(r io.Reader) ([]pkg, error) {
	var pkgs []pkg
	sc := bufio.NewScanner(r)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			return nil, fmt.Errorf("line %d: want \"name version architecture\", got %q", n, line)
		}
		pkgs = append(pkgs, pkg{f[0], f[1], f[2]})
	}
	return pkgs, sc.Err()
}

type change struct {
	Name, Arch string
	From, To   string // empty From: added; empty To: removed
	Security   bool
}

func (c change) String() string {
	switch {
	case c.From == "":
		return fmt.Sprintf("added   %s %s", c.Name, c.To)
	case c.To == "":
		return fmt.Sprintf("removed %s %s", c.Name, c.From)
	}
	return fmt.Sprintf("changed %s %s to %s", c.Name, c.From, c.To)
}

// compare lists what moved from old to cur, sorted by package name.
func compare(old, cur []pkg) []change {
	before := map[string]pkg{}
	for _, p := range old {
		before[p.key()] = p
	}
	after := map[string]pkg{}
	for _, p := range cur {
		after[p.key()] = p
	}
	var changes []change
	for k, p := range after {
		o, ok := before[k]
		switch {
		case !ok:
			changes = append(changes, change{Name: p.Name, Arch: p.Arch, To: p.Version})
		case o.Version != p.Version:
			changes = append(changes, change{Name: p.Name, Arch: p.Arch, From: o.Version, To: p.Version})
		}
	}
	for k, o := range before {
		if _, ok := after[k]; !ok {
			changes = append(changes, change{Name: o.Name, Arch: o.Arch, From: o.Version})
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Name != changes[j].Name {
			return changes[i].Name < changes[j].Name
		}
		return changes[i].Arch < changes[j].Arch
	})
	return changes
}

func markSecurity(changes []change, sec map[string]map[string]bool) {
	for i := range changes {
		if changes[i].To != "" && sec[changes[i].Name][changes[i].To] {
			changes[i].Security = true
		}
	}
}

const maxTitle = 120

// title names the changes, security updates first, and stops at about
// maxTitle characters with a count of the rest.
func title(changes []change) string {
	if len(changes) == 0 {
		return "OS lock: no package changes"
	}
	ordered := append([]change(nil), changes...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Security && !ordered[j].Security })
	prefix := "OS lock: "
	if ordered[0].Security {
		prefix = "OS lock (security): "
	}
	t := prefix
	for i, c := range ordered {
		var part string
		switch {
		case c.From == "":
			part = fmt.Sprintf("add %s %s", c.Name, c.To)
		case c.To == "":
			part = fmt.Sprintf("remove %s", c.Name)
		default:
			part = fmt.Sprintf("%s %s to %s", c.Name, c.From, c.To)
		}
		sep := ""
		if i > 0 {
			sep = ", "
		}
		more := fmt.Sprintf(" and %d more", len(ordered)-i)
		if i > 0 && len(t)+len(sep)+len(part) > maxTitle {
			return t + more
		}
		t += sep + part
	}
	return t
}

func markdown(changes []change) string {
	var b strings.Builder
	if len(changes) == 0 {
		b.WriteString("No package changed.\n")
		return b.String()
	}
	nsec, nadd := 0, 0
	for _, c := range changes {
		if c.Security {
			nsec++
		}
		if c.From == "" || c.To == "" {
			nadd++
		}
	}
	if nsec > 0 {
		fmt.Fprintf(&b, "**Security:** %d of the %d changed packages come from `trixie-security`. "+
			"Release rule: cut an OS patch release with this change within 7 days "+
			"(`docs/dev/contributing.md` # Release model).\n\n", nsec, len(changes))
	} else {
		b.WriteString("No change comes from `trixie-security`, so this ships with the next normal OS release.\n\n")
	}
	b.WriteString("| Package | From | To | Source |\n|---|---|---|---|\n")
	for _, c := range changes {
		from, to, src := c.From, c.To, ""
		if from == "" {
			from = "(new)"
		}
		if to == "" {
			to = "(removed)"
		}
		if c.Security {
			src = "trixie-security"
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | %s |\n", c.Name, from, to, src)
	}
	if nadd > 0 {
		b.WriteString("\nA package was added or removed. The lean check fails until " +
			"`dev/cloud/expected-packages.txt` lists the new set, so a person must review it.\n")
	}
	return b.String()
}

// parsePackagesIndex reads a Debian Packages index into name -> versions.
func parsePackagesIndex(r io.Reader) (map[string]map[string]bool, error) {
	idx := map[string]map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	name := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			name = ""
		case strings.HasPrefix(line, "Package:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "Package:"))
		case strings.HasPrefix(line, "Version:") && name != "":
			v := strings.TrimSpace(strings.TrimPrefix(line, "Version:"))
			if idx[name] == nil {
				idx[name] = map[string]bool{}
			}
			idx[name][v] = true
		}
	}
	return idx, sc.Err()
}

// newestPins rewrites a third-party.lock ("package=version" lines, comments
// kept as they are) so each pin names the newest version in index with the
// same epoch and the same major version. A new major is a manual edit.
func newestPins(lock string, index map[string]map[string]bool) (string, error) {
	var b strings.Builder
	lines := strings.SplitAfter(lock, "\n")
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			b.WriteString(raw)
			continue
		}
		name, ver, ok := strings.Cut(line, "=")
		if !ok || name == "" || ver == "" {
			return "", fmt.Errorf("third-party pin %q is not package=version", line)
		}
		if len(index[name]) == 0 {
			return "", fmt.Errorf("package %s is not in the third-party index", name)
		}
		best := ver
		for cand := range index[name] {
			if sameMajor(cand, ver) && compareVersions(cand, best) > 0 {
				best = cand
			}
		}
		b.WriteString(name + "=" + best + "\n")
	}
	return b.String(), nil
}

// sameMajor reports whether two Debian versions share an epoch and the first
// number of the upstream version ("5:29.8.2-1~debian.13~trixie" is 5 and 29).
func sameMajor(a, b string) bool {
	ea, ua, _ := splitVersion(a)
	eb, ub, _ := splitVersion(b)
	return ea == eb && leadingNumber(ua) == leadingNumber(ub) && leadingNumber(ua) != ""
}

func leadingNumber(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}

// splitVersion splits a Debian version into epoch, upstream and revision.
func splitVersion(v string) (epoch, upstream, revision string) {
	epoch = "0"
	if e, rest, ok := strings.Cut(v, ":"); ok {
		epoch, v = e, rest
	}
	if i := strings.LastIndex(v, "-"); i >= 0 {
		return epoch, v[:i], v[i+1:]
	}
	return epoch, v, ""
}

// compareVersions compares two Debian versions the way dpkg does: epoch as a
// number, then upstream and revision by dpkg's string-and-number rule, where
// "~" sorts before everything, even the end of the string.
func compareVersions(a, b string) int {
	ea, ua, ra := splitVersion(a)
	eb, ub, rb := splitVersion(b)
	if c := compareNumber(ea, eb); c != 0 {
		return c
	}
	if c := comparePart(ua, ub); c != 0 {
		return c
	}
	return comparePart(ra, rb)
}

func compareNumber(a, b string) int {
	a = strings.TrimLeft(a, "0")
	b = strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func order(c byte) int {
	switch {
	case c == '~':
		return -1
	case isDigit(c):
		return 0
	case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
		return int(c)
	}
	return int(c) + 256
}

func comparePart(a, b string) int {
	for a != "" || b != "" {
		// The non-digit run.
		for (a != "" && !isDigit(a[0])) || (b != "" && !isDigit(b[0])) {
			ac, bc := 0, 0
			if a != "" {
				ac = order(a[0])
			}
			if b != "" {
				bc = order(b[0])
			}
			if ac != bc {
				if ac < bc {
					return -1
				}
				return 1
			}
			if a != "" {
				a = a[1:]
			}
			if b != "" {
				b = b[1:]
			}
		}
		// The digit run.
		i := 0
		for i < len(a) && isDigit(a[i]) {
			i++
		}
		j := 0
		for j < len(b) && isDigit(b[j]) {
			j++
		}
		if c := compareNumber(a[:i], b[:j]); c != 0 {
			return c
		}
		a, b = a[i:], b[j:]
	}
	return 0
}
