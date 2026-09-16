package usermgr

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/onmoose/os/internal/protocol"
)

func TestUseraddArgs_Defaults(t *testing.T) {
	m := &LinuxUserManager{}
	got := m.useraddArgs("cindy")
	want := []string{"--create-home", "--shell", "/bin/bash", "--gid", "moose", "cindy"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("useraddArgs default: got %v, want %v", got, want)
	}
}

func TestUseraddArgs_Overrides(t *testing.T) {
	m := &LinuxUserManager{Shell: "/usr/bin/zsh", PrimaryGroup: "users"}
	got := m.useraddArgs("dave")
	want := []string{"--create-home", "--shell", "/usr/bin/zsh", "--gid", "users", "dave"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("useraddArgs overrides: got %v, want %v", got, want)
	}
}

func TestUpsertPassword_EmptySlug(t *testing.T) {
	m := &LinuxUserManager{}
	if err := m.UpsertPassword("", "pw"); err == nil {
		t.Error("want error for empty slug")
	}
}

func TestDeleteUser_EmptySlug(t *testing.T) {
	m := &LinuxUserManager{}
	if err := m.DeleteUser(""); err == nil {
		t.Error("want error for empty slug")
	}
}

func TestSetRole_EmptySlug(t *testing.T) {
	m := &LinuxUserManager{}
	if err := m.SetRole("", "admin"); err == nil {
		t.Error("want error for empty slug")
	}
}

func TestSetRole_BadRole(t *testing.T) {
	m := &LinuxUserManager{}
	if err := m.SetRole("cindy", "superuser"); err == nil {
		t.Error("want error for bad role")
	}
}

func TestParseGroupMembership(t *testing.T) {
	const content = `# comment line ignored
root:x:0:
sudo:x:27:alice,bob,cindy
moose:x:3000:
nogroup:x:65534:
`
	cases := []struct {
		slug, group string
		want        bool
	}{
		{"alice", "sudo", true},
		{"bob", "sudo", true},
		{"cindy", "sudo", true},
		{"dave", "sudo", false},         // not listed
		{"alice", "moose", false},       // listed in sudo, not moose
		{"alice", "nonexistent", false}, // group not present
		{"", "sudo", false},
	}
	for _, c := range cases {
		got := parseGroupMembership([]byte(content), c.slug, c.group)
		if got != c.want {
			t.Errorf("parseGroupMembership(%q,%q) = %v, want %v", c.slug, c.group, got, c.want)
		}
	}
}

func TestFirstFreeAppServiceID(t *testing.T) {
	min := protocol.AppServiceUIDMin
	t.Run("empty files start at band min", func(t *testing.T) {
		n, err := firstFreeAppServiceID(nil, nil)
		if err != nil || n != min {
			t.Fatalf("got %d, %v; want %d, nil", n, err, min)
		}
	})
	t.Run("UIDs outside the band are ignored", func(t *testing.T) {
		passwd := []byte("root:x:0:0:root:/root:/bin/bash\nmoose-app:x:2000:2000::/nonexistent:/usr/sbin/nologin\nalice:x:3000:3000::/home/alice:/bin/bash\n")
		n, err := firstFreeAppServiceID(passwd, nil)
		if err != nil || n != min {
			t.Fatalf("got %d, %v; want %d, nil", n, err, min)
		}
	})
	t.Run("taken UIDs are skipped", func(t *testing.T) {
		passwd := []byte("moose-svc-2100:x:2100:2100:gecos:/nonexistent:/usr/sbin/nologin\nmoose-svc-2101:x:2101:2101:gecos:/nonexistent:/usr/sbin/nologin\n")
		n, err := firstFreeAppServiceID(passwd, nil)
		if err != nil || n != min+2 {
			t.Fatalf("got %d, %v; want %d, nil", n, err, min+2)
		}
	})
	t.Run("a GID alone reserves the number", func(t *testing.T) {
		// A group squatting on a band number (even with no matching user) makes
		// the pair unusable — groupadd --gid would fail. Skip it.
		group := []byte("stray:x:2100:\n")
		n, err := firstFreeAppServiceID(nil, group)
		if err != nil || n != min+1 {
			t.Fatalf("got %d, %v; want %d, nil", n, err, min+1)
		}
	})
	t.Run("exhausted band errors", func(t *testing.T) {
		var passwd []byte
		for n := protocol.AppServiceUIDMin; n <= protocol.AppServiceUIDMax; n++ {
			passwd = append(passwd, []byte(fmt.Sprintf("moose-svc-%d:x:%d:%d:g:/nonexistent:/usr/sbin/nologin\n", n, n, n))...)
		}
		if _, err := firstFreeAppServiceID(passwd, nil); err == nil {
			t.Fatal("want exhaustion error, got nil")
		}
	})
}

func TestFindAppServiceByGecos(t *testing.T) {
	passwd := []byte(`root:x:0:0:root:/root:/bin/bash
moose-svc-2100:x:2100:2100:moose app-service for inst_aaa:/nonexistent:/usr/sbin/nologin
moose-svc-2101:x:2101:2101:moose app-service for inst_bbb:/nonexistent:/usr/sbin/nologin
imposter:x:2102:2102:moose app-service for inst_ccc:/home/imposter:/bin/bash
`)
	if got := findAppServiceByGecos(passwd, svcGecos("inst_aaa")); got != 2100 {
		t.Errorf("inst_aaa: got %d, want 2100", got)
	}
	if got := findAppServiceByGecos(passwd, svcGecos("inst_bbb")); got != 2101 {
		t.Errorf("inst_bbb: got %d, want 2101", got)
	}
	if got := findAppServiceByGecos(passwd, svcGecos("inst_unknown")); got != 0 {
		t.Errorf("unknown instance: got %d, want 0", got)
	}
	// A matching GECOS on a non-moose-svc account must not count: only
	// accounts we created are reservations.
	if got := findAppServiceByGecos(passwd, svcGecos("inst_ccc")); got != 0 {
		t.Errorf("imposter account matched: got %d, want 0", got)
	}
	// A colon in the instance ID must still match — the GECOS field is
	// reassembled across multiple split fields.
	passwdColon := []byte("moose-svc-2103:x:2103:2103:moose app-service for inst:colon:/nonexistent:/usr/sbin/nologin\n")
	if got := findAppServiceByGecos(passwdColon, svcGecos("inst:colon")); got != 2103 {
		t.Errorf("colon in instance id: got %d, want 2103", got)
	}
}

func TestAllocateAppService_EmptyInstanceID(t *testing.T) {
	m := &LinuxUserManager{}
	if _, _, err := m.AllocateAppService(""); err == nil {
		t.Error("want error for empty instance id")
	}
}

func TestReleaseAppService_RejectsOutOfBandUID(t *testing.T) {
	m := &LinuxUserManager{}
	// Must error before any lookup/shell-out — this guard is what keeps the
	// endpoint from ever deleting an arbitrary account.
	for _, uid := range []int{0, 1000, protocol.AppServiceUIDMin - 1, protocol.AppServiceUIDMax + 1} {
		if err := m.ReleaseAppService(uid); err == nil {
			t.Errorf("ReleaseAppService(%d): want out-of-band error, got nil", uid)
		}
	}
}

// Trailing-whitespace / empty-member-field corner cases observed in practice.
func TestParseGroupMembership_EdgeCases(t *testing.T) {
	const content = "sudo:x:27: alice , bob \nempty:x:99:\n"
	if !parseGroupMembership([]byte(content), "alice", "sudo") {
		t.Error("alice with leading-space should match")
	}
	if !parseGroupMembership([]byte(content), "bob", "sudo") {
		t.Error("bob with surrounding whitespace should match")
	}
	if parseGroupMembership([]byte(content), "anyone", "empty") {
		t.Error("empty member list should not match")
	}
	// Regression: strings.Split("",",") returns [""], so without the empty-slug
	// guard a "" lookup against the empty member list of "empty:" would match.
	if parseGroupMembership([]byte(content), "", "empty") {
		t.Error("empty slug must never match, even against an empty member list")
	}
}
