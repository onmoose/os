package manifest

import (
	"strings"
	"testing"
)

// clawRequires gates the openclaw-like fixture on any AI provider.
const clawRequires = "requires:\n  - one_of: [ai]\n  - one_of: [GOOGLE_API_KEY, GOOGLE_CREDENTIALS]\n"

func lintOf(t *testing.T, body string, opts LintOptions) (errs, warns []string) {
	t.Helper()
	return Lint(mustParseRoles(t, rolesHead+body), opts)
}

func TestLint_CleanManifest(t *testing.T) {
	errs, warns := lintOf(t, clawConfig+clawRequires, LintOptions{})
	if len(errs) > 0 || len(warns) > 0 {
		t.Fatalf("clean manifest: errs=%v warns=%v", errs, warns)
	}
	// A manifest with no roles and no requires is clean too.
	errs, warns = lintOf(t, "config:\n  - {app_env: A, title: t, description: d, required: true}\n", LintOptions{})
	if len(errs) > 0 || len(warns) > 0 {
		t.Fatalf("role-free manifest: errs=%v warns=%v", errs, warns)
	}
}

// field is one config entry in flow style, for the error table below.
func field(env, extra string) string {
	return "  - {app_env: " + env + ", title: " + env + " title, description: d" + extra + "}\n"
}

func TestLint_Errors(t *testing.T) {
	key := func(env, slot string) string { return field(env, ", secret: true, role: "+slot+".api_key") }
	cases := []struct {
		name string
		body string
		want string
	}{
		{"malformed role", "config:\n" + field("A", ", role: AI.x.api_key"), "config[A]: role \"AI.x.api_key\" must be"},
		{"role not a single value", "config:\n" + field("A", ", role: [ai]"), "config[A]: role must be a single text value"},
		{"unknown kind", "config:\n" + field("A", ", secret: true, role: mail.x.api_key"), "unknown kind \"mail\""},
		{"unknown attribute", "config:\n" + key("A", "ai.x") + field("B", ", role: ai.x.token"), "config[B]: role \"ai.x.token\" has an unknown attribute"},
		{"unknown model type", "config:\n" + key("A", "ai.x") + field("B", ", role: ai.x.model.poem"), "unknown model type \"poem\""},
		{"duplicate role", "config:\n" + key("A", "ai.x") + key("B", "ai.x"), "config[B]: role \"ai.x.api_key\" is also on config[A]"},
		{"key not secret", "config:\n" + field("A", ", role: ai.x.api_key"), "config[A]: role \"ai.x.api_key\" is a key; add secret: true"},
		{"role on enum", "config:\n" + key("A", "ai.x") + field("B", ", type: enum, options: [a], role: ai.x.model.chat"), "config[B]: a field with a role must be type text"},
		{"role with options", "config:\n" + key("A", "ai.x") + field("B", ", type: enum, options: [a], role: ai.x.model.chat"), "config[B]: a field with a role may not have options"},
		{"role with default", "config:\n" + key("A", "ai.x") + field("B", ", default: m, role: ai.x.model.chat"), "config[B]: a field with a role may not have a default"},
		{"role with required", "config:\n" + field("A", ", secret: true, required: true, role: ai.x.api_key"), "config[A]: a field with a role may not be required: true"},
		{"separator on a plain field", "config:\n" + field("A", ", separator: \",\""), "config[A]: separator is only valid on a field with a models.<type> role"},
		{"separator on a single model", "config:\n" + key("A", "ai.x") + field("B", ", separator: \",\", role: ai.x.model.chat"), "config[B]: separator is only valid on a models.<type> role"},
		{"separator too long", "config:\n" + key("A", "ai.x") + field("B", ", separator: \"12345\", role: ai.x.models.chat"), "config[B]: separator \"12345\" must be 1 to 4 characters"},
		{"separator with equals", "config:\n" + key("A", "ai.x") + field("B", ", separator: \"=\", role: ai.x.models.chat"), "config[B]: separator \"=\" may use only printable characters"},
		{"separator empty", "config:\n" + key("A", "ai.x") + field("B", ", separator: \"\", role: ai.x.models.chat"), "must be 1 to 4 characters"},
		{"separator with newline", "config:\n" + key("A", "ai.x") + field("B", ", separator: \"\\n\", role: ai.x.models.chat"), "may use only printable characters"},
		{"model-only slot", "config:\n" + field("A", ", role: ai.x.model.chat"), "config[A]: slot \"ai.x\" has only model fields"},
		{"compatible slot without base_url", "config:\n" + key("A", "ai.openai_compatible"), "config[A]: slot \"ai.openai_compatible\" has no base_url field"},
		{"member matches no field", "config:\n" + key("A", "ai.x") + "requires:\n  - one_of: [ai.y]\n", "requires[0]: \"ai.y\" matches no field"},
		{"member is a role", "config:\n" + key("A", "ai.x") + "requires:\n  - one_of: [ai.x.api_key]\n", "requires[0]: \"ai.x.api_key\" is a role, not a slot; name its slot (ai.x)"},
		{"member is malformed", "config:\n" + key("A", "ai.x") + "requires:\n  - one_of: [Ai-X]\n", "requires[0]: \"Ai-X\" is not a kind (ai), a slot (ai.anthropic) or a field's app_env"},
		{"duplicate member", "config:\n" + key("A", "ai.x") + "requires:\n  - one_of: [ai, ai]\n", "requires[0]: \"ai\" is listed twice"},
		{"empty group", "config:\n" + key("A", "ai.x") + "requires:\n  - one_of: []\n", "requires[0]: one_of is empty"},
		{"entry with no one_of", "config:\n" + key("A", "ai.x") + "requires:\n  - {}\n", "requires[0]: entry has no one_of list"},
		{"requires not a list", "config:\n" + key("A", "ai.x") + "requires: ai\n", "requires[0]: requires must be a list of one_of groups"},
		{"required plain member", "config:\n" + field("A", ", required: true") + field("B", "") + "requires:\n  - one_of: [A, B]\n", "config[A]: required: true conflicts with requires[0]"},
		{"role field named by app_env", "config:\n" + key("A", "ai.x") + field("B", "") + "requires:\n  - one_of: [A, B]\n", "requires[0]: \"A\" has a role; name its slot or kind"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs, _ := lintOf(t, c.body, LintOptions{})
			all := strings.Join(errs, "\n")
			if !strings.Contains(all, c.want) {
				t.Fatalf("errors do not contain %q:\n%s", c.want, all)
			}
		})
	}
}

func TestLint_Warnings(t *testing.T) {
	t.Run("kind group over slots with different model types", func(t *testing.T) {
		body := "config:\n" +
			field("A", ", secret: true, role: ai.x.api_key") +
			field("B", ", role: ai.x.model.chat") +
			field("C", ", secret: true, role: ai.y.api_key") +
			field("D", ", role: ai.y.model.embedding") +
			field("E", ", secret: true, role: ai.z.api_key") + // no model field: left out of the comparison
			"requires:\n  - one_of: [ai]\n"
		errs, warns := lintOf(t, body, LintOptions{})
		if len(errs) > 0 {
			t.Fatalf("errs = %v", errs)
		}
		if len(warns) != 1 || !strings.Contains(warns[0], "(ai.x: chat; ai.y: embedding)") {
			t.Fatalf("warns = %v; want one about differing model types", warns)
		}
	})
	t.Run("same model types do not warn", func(t *testing.T) {
		body := "config:\n" +
			field("A", ", secret: true, role: ai.x.api_key") +
			field("B", ", role: ai.x.model.chat") +
			field("C", ", secret: true, role: ai.y.api_key") +
			field("D", ", role: ai.y.models.chat") +
			"requires:\n  - one_of: [ai]\n"
		if errs, warns := lintOf(t, body, LintOptions{}); len(errs)+len(warns) > 0 {
			t.Fatalf("errs=%v warns=%v; want none", errs, warns)
		}
	})
	t.Run("single plain member", func(t *testing.T) {
		errs, warns := lintOf(t, "config:\n"+field("A", "")+"requires:\n  - one_of: [A]\n", LintOptions{})
		if len(errs) > 0 || len(warns) != 1 || !strings.Contains(warns[0], "set required: true on that field instead") {
			t.Fatalf("errs=%v warns=%v", errs, warns)
		}
	})
	t.Run("native protocol no provider offers", func(t *testing.T) {
		body := clawConfig + field("GROKK_KEY", ", secret: true, role: ai.grokk.api_key")
		opts := LintOptions{NativeProtocols: map[string]bool{"anthropic": true, "openai": true}}
		errs, warns := lintOf(t, body, opts)
		if len(errs) > 0 {
			t.Fatalf("errs = %v", errs)
		}
		if len(warns) != 1 || !strings.Contains(warns[0], `config[GROKK_KEY]: no AI provider offers the "grokk" protocol`) {
			t.Fatalf("warns = %v; want one about grokk only", warns)
		}
		// Without provider data the check is skipped.
		if _, warns := lintOf(t, body, LintOptions{}); len(warns) != 0 {
			t.Fatalf("warns without provider data = %v; want none", warns)
		}
	})
}
