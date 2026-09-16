package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onmoose/moose/internal/hostagent/updatetarget"
)

// seedFile writes a seed at a fresh path and points MOOSE_SEED_PATH at it, the
// way the first-boot materializer lands one before host-agent starts.
func seedFile(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seed.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	t.Setenv(envSeedPath, path)
}

// seededTarget is a seed of the shape a provisioned box gets, with the update
// target set to url. An empty url leaves the field out entirely, which is what a
// box that was never steered receives.
func seededTarget(url string) string {
	const head = `{"box_id":"cindy-fox","assertion_verification_key":"a2V5"`
	if url == "" {
		return head + `}`
	}
	return head + `,"update_target_url":"` + url + `"}`
}

// noSeed is the appliance case, and a hosted box provisioned without a seed: the
// path exists as a name and nothing was ever written there.
func noSeed(t *testing.T) {
	t.Helper()
	t.Setenv(envSeedPath, filepath.Join(t.TempDir(), "seed.json"))
}

func TestSeedUpdateFacts(t *testing.T) {
	// No seed at all must not be an error. This is every appliance box, and a
	// hosted box provisioned without one.
	t.Run("absent seed is not an error", func(t *testing.T) {
		noSeed(t)
		target, boxID, err := seedUpdateFacts()
		if err != nil || target != "" || boxID != "" {
			t.Fatalf("got (%q, %q, %v), want (\"\", \"\", nil)", target, boxID, err)
		}
	})

	t.Run("seed without the field falls through", func(t *testing.T) {
		seedFile(t, seededTarget(""))
		target, _, err := seedUpdateFacts()
		if err != nil || target != "" {
			t.Fatalf("got (%q, %v), want (\"\", nil)", target, err)
		}
	})

	// An empty field carries no instruction, so it is the absent case rather than
	// an unusable one.
	t.Run("empty field falls through", func(t *testing.T) {
		seedFile(t, seededTarget("  "))
		target, _, err := seedUpdateFacts()
		if err != nil || target != "" {
			t.Fatalf("got (%q, %v), want (\"\", nil)", target, err)
		}
	})

	t.Run("field present, surrounding whitespace trimmed", func(t *testing.T) {
		seedFile(t, seededTarget(" https://example.test/target\\n "))
		target, _, err := seedUpdateFacts()
		if err != nil {
			t.Fatalf("seedUpdateFacts: %v", err)
		}
		if target != "https://example.test/target" {
			t.Fatalf("target = %q, want %q", target, "https://example.test/target")
		}
	})

	// A seed that will not parse might have carried a target and we cannot tell,
	// so it is a hard error rather than the absent case.
	t.Run("malformed seed is an error", func(t *testing.T) {
		seedFile(t, `{"box_id": `)
		target, boxID, err := seedUpdateFacts()
		if err == nil {
			t.Fatalf("got (%q, %q, nil), want an error", target, boxID)
		}
	})

	// Present but unreadable is an error for the same reason. A directory stands
	// in for the unreadable file: mode 0o000 would be ignored by root, so the
	// assertion would quietly evaporate whenever the suite runs as root, while
	// os.ReadFile on a directory fails for every user.
	t.Run("unreadable seed is an error", func(t *testing.T) {
		t.Setenv(envSeedPath, t.TempDir())
		target, boxID, err := seedUpdateFacts()
		if err == nil {
			t.Fatalf("got (%q, %q, nil), want an error", target, boxID)
		}
	})
}

func TestUpdateTarget(t *testing.T) {
	const seeded = "https://candidate.example.test/api/updates/target"
	const env = "http://127.0.0.1:9"

	t.Run("seed beats the env var", func(t *testing.T) {
		seedFile(t, seededTarget(seeded))
		t.Setenv(envUpdateTargetURL, env)
		target, from, _, err := updateTarget()
		if err != nil {
			t.Fatalf("updateTarget: %v", err)
		}
		if target != seeded || from != fromSeed {
			t.Fatalf("got (%q, %q), want (%q, %q)", target, from, seeded, fromSeed)
		}
	})

	t.Run("env var used when the seed names no target", func(t *testing.T) {
		seedFile(t, seededTarget(""))
		t.Setenv(envUpdateTargetURL, env)
		target, from, _, err := updateTarget()
		if err != nil {
			t.Fatalf("updateTarget: %v", err)
		}
		if target != env || from != fromEnv {
			t.Fatalf("got (%q, %q), want (%q, %q)", target, from, env, fromEnv)
		}
	})

	// The appliance path: no seed file exists at all, and the box must resolve
	// exactly as it did before, not error.
	t.Run("env var used when there is no seed at all", func(t *testing.T) {
		noSeed(t)
		t.Setenv(envUpdateTargetURL, env)
		target, from, _, err := updateTarget()
		if err != nil {
			t.Fatalf("updateTarget: %v", err)
		}
		if target != env || from != fromEnv {
			t.Fatalf("got (%q, %q), want (%q, %q)", target, from, env, fromEnv)
		}
	})

	// The regression that matters most: a box provisioned the way every box is
	// provisioned today must still read the fleet endpoint.
	t.Run("neither leaves the fleet default untouched", func(t *testing.T) {
		noSeed(t)
		t.Setenv(envUpdateTargetURL, "")
		target, from, _, err := updateTarget()
		if err != nil {
			t.Fatalf("updateTarget: %v", err)
		}
		if target != "" || from != fromDefault {
			t.Fatalf("got (%q, %q), want (\"\", %q)", target, from, fromDefault)
		}
		// Empty is what HTTPSource reads as "the fleet endpoint".
		if (updatetarget.HTTPSource{URL: target}).URL != "" {
			t.Fatal("an absent seed target must leave HTTPSource on its default URL")
		}
	})

	// An unusable seeded target is refused. It must not fall through to the env
	// var or to the fleet default: a box meant to be pinned must not join stable.
	for _, tc := range []struct {
		name string
		url  string
	}{
		{"no scheme", "candidate.example.test/api/updates/target"},
		{"wrong scheme", "ftp://candidate.example.test/target"},
		{"no host", "https:///api/updates/target"},
		{"not a url", "https://exa mple.test/\\u007f"},
	} {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			seedFile(t, seededTarget(tc.url))
			t.Setenv(envUpdateTargetURL, env)
			target, from, _, err := updateTarget()
			if err == nil {
				t.Fatalf("got (%q, %q, nil), want an error", target, from)
			}
			if target != "" {
				t.Fatalf("a refused target resolved to %q; it must resolve to nothing", target)
			}
		})
	}

	// The identity the box sends. It comes off the same seed read as the target,
	// and it does not depend on where the target came from: a box whose URL is a
	// local hand-edit is still that box.
	t.Run("the box id comes from the seed", func(t *testing.T) {
		seedFile(t, seededTarget(seeded))
		_, _, boxID, err := updateTarget()
		if err != nil {
			t.Fatalf("updateTarget: %v", err)
		}
		if boxID != "cindy-fox" {
			t.Fatalf("box_id = %q, want %q", boxID, "cindy-fox")
		}
	})

	t.Run("the box id survives an env-var target", func(t *testing.T) {
		seedFile(t, seededTarget(""))
		t.Setenv(envUpdateTargetURL, env)
		_, from, boxID, err := updateTarget()
		if err != nil {
			t.Fatalf("updateTarget: %v", err)
		}
		if from != fromEnv || boxID != "cindy-fox" {
			t.Fatalf("got (%q, %q), want (%q, %q)", from, boxID, fromEnv, "cindy-fox")
		}
	})

	// No seed, no identity. The appliance box and the unseeded hosted box both ask
	// anonymously, which is what HTTPSource turns into "send no parameter".
	t.Run("no seed means no box id", func(t *testing.T) {
		noSeed(t)
		_, _, boxID, err := updateTarget()
		if err != nil {
			t.Fatalf("updateTarget: %v", err)
		}
		if boxID != "" {
			t.Fatalf("box_id = %q, want empty on a box with no seed", boxID)
		}
	})

	// A malformed seed is refused for the same reason an unusable URL is: it may
	// have pinned this box and we cannot read it, so falling through to the fleet
	// endpoint could move a pinned box onto stable.
	t.Run("refuses a malformed seed", func(t *testing.T) {
		seedFile(t, "not json at all")
		t.Setenv(envUpdateTargetURL, env)
		target, _, _, err := updateTarget()
		if err == nil {
			t.Fatal("a malformed seed must be refused, not resolved")
		}
		if target != "" {
			t.Fatalf("a malformed seed fell back to %q; it must fall back to nothing", target)
		}
	})
}

func TestUpdateWindow(t *testing.T) {
	envWindow := updatetarget.Window{Start: 5 * time.Hour, End: 6 * time.Hour}

	t.Run("env var wins over the default", func(t *testing.T) {
		t.Setenv(envUpdateWindow, "05:00-06:00")
		w, from := updateWindow()
		if w != envWindow || from != fromEnv {
			t.Fatalf("got (%v, %q), want (%v, %q)", w, from, envWindow, fromEnv)
		}
	})

	t.Run("no setting leaves the default window untouched", func(t *testing.T) {
		t.Setenv(envUpdateWindow, "")
		w, from := updateWindow()
		if w != updatetarget.DefaultWindow || from != fromDefault {
			t.Fatalf("got (%v, %q), want (%v, %q)", w, from, updatetarget.DefaultWindow, fromDefault)
		}
	})

	// Unlike the target URL, a mistyped window falls back rather than costing the
	// box its updates: a wrong hour cannot send it to a wrong version.
	t.Run("unparseable setting falls back to the default", func(t *testing.T) {
		t.Setenv(envUpdateWindow, "half past three")
		w, from := updateWindow()
		if w != updatetarget.DefaultWindow || from != fromDefault {
			t.Fatalf("got (%v, %q), want (%v, %q)", w, from, updatetarget.DefaultWindow, fromDefault)
		}
	})

	// The window has no seed source, so a seed carrying an update target must not
	// change the window this box applies in.
	t.Run("a seeded target does not touch the window", func(t *testing.T) {
		seedFile(t, seededTarget("https://candidate.example.test/target"))
		t.Setenv(envUpdateWindow, "")
		w, from := updateWindow()
		if w != updatetarget.DefaultWindow || from != fromDefault {
			t.Fatalf("got (%v, %q), want (%v, %q)", w, from, updatetarget.DefaultWindow, fromDefault)
		}
	})
}

// A seeded update-target URL that will not parse becomes the `detail` of the
// disabled state on GET /v1/system/update-target (#443). The parser's own error
// carries the raw URL — password, query and all — so this is the one message on
// that path that has to be checked, not assumed.
func TestCheckTargetURL_RefusalCarriesNoSecret(t *testing.T) {
	// Malformed (a control character), with a secret in each hiding place.
	err := checkTargetURL("http://user:pw-secret@moose.example\x7f/target?token=query-secret")
	if err == nil {
		t.Fatal("a URL with a control character was accepted")
	}
	for _, leak := range []string{"pw-secret", "query-secret"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("the refusal leaks %q: %q", leak, err)
		}
	}
	if !strings.Contains(err.Error(), "seed update_target_url") {
		t.Errorf("the refusal = %q, want it to still name the setting at fault", err)
	}
}

// The two well-formed refusals name the offending value, redacted.
func TestCheckTargetURL_WrongSchemeCarriesNoSecret(t *testing.T) {
	err := checkTargetURL("ftp://user:pw-secret@moose.example/target?token=query-secret")
	if err == nil {
		t.Fatal("an ftp URL was accepted")
	}
	for _, leak := range []string{"pw-secret", "query-secret"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("the refusal leaks %q: %q", leak, err)
		}
	}
}
