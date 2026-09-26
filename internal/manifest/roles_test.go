package manifest

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// rolesHead is the required top of every manifest in these tests.
const rolesHead = `id: claw-demo
manifest_version: 1
name: Claw Demo
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 80
`

// clawConfig is an openclaw-like config block: three native key slots and a
// custom OpenAI-compatible triple.
const clawConfig = `config:
  - app_env: ANTHROPIC_API_KEY
    title: Anthropic key
    description: d
    secret: true
    role: ai.anthropic.api_key
  - app_env: OPENAI_API_KEY
    title: OpenAI key
    description: d
    secret: true
    role: ai.openai.api_key
  - app_env: CUSTOM_BASE_URL
    title: Custom base URL
    description: d
    role: ai.openai_compatible.base_url
  - app_env: CUSTOM_MODEL
    title: Custom model
    description: d
    role: ai.openai_compatible.model.chat
  - app_env: CUSTOM_API_KEY
    title: Custom key
    description: d
    secret: true
    role: ai.openai_compatible.api_key
  - app_env: CUSTOM_MODELS
    title: Custom model list
    description: d
    role: ai.openai_compatible.models.embedding
    separator: ";"
  - app_env: GOOGLE_API_KEY
    title: Google key
    description: d
  - app_env: GOOGLE_CREDENTIALS
    title: Google credentials
    description: d
`

func mustParseRoles(t *testing.T, src string) *Manifest {
	t.Helper()
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return m
}

func TestParseRole(t *testing.T) {
	good := map[string]Role{
		"ai.anthropic.api_key":              {Kind: "ai", Protocol: "anthropic", Attribute: AttrAPIKey},
		"ai.openai_compatible.base_url":     {Kind: "ai", Protocol: "openai_compatible", Attribute: AttrBaseURL},
		"ai.openai.model.embedding":         {Kind: "ai", Protocol: "openai", Attribute: AttrModel, ModelType: "embedding"},
		"ai.openai.models.speech_to_text":   {Kind: "ai", Protocol: "openai", Attribute: AttrModels, ModelType: "speech_to_text"},
		"ai.some_new_protocol2.api_key":     {Kind: "ai", Protocol: "some_new_protocol2", Attribute: AttrAPIKey},
		"ai.openai_compatible.models.chat":  {Kind: "ai", Protocol: "openai_compatible", Attribute: AttrModels, ModelType: "chat"},
		"ai.anthropic.model.text_to_speech": {Kind: "ai", Protocol: "anthropic", Attribute: AttrModel, ModelType: "text_to_speech"},
	}
	for in, want := range good {
		got, err := ParseRole(in)
		if err != nil || got != want {
			t.Errorf("ParseRole(%q) = %+v, %v; want %+v", in, got, err, want)
		}
		if got.String() != in {
			t.Errorf("Role.String() = %q; want %q", got.String(), in)
		}
	}
	bad := map[string]string{
		"":                        "must be",
		"ai.anthropic":            "must be",
		"AI.anthropic.api_key":    "must be",
		"ai.anthropic.api-key":    "must be",
		"ai..api_key":             "must be",
		"ai.a.b.c.d":              "must be",
		"email.smtp.api_key":      "unknown kind",
		"ai.anthropic.token":      "unknown attribute",
		"ai.anthropic.api_key.x":  "unknown attribute",
		"ai.anthropic.model":      "needs a model type",
		"ai.anthropic.model.poem": "unknown model type",
	}
	for in, want := range bad {
		if _, err := ParseRole(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseRole(%q) err = %v; want it to mention %q", in, err, want)
		}
	}
}

// Parse must accept every bad role, separator and requires shape: the brain
// runs it on every catalog and installed manifest, so none of these may make a
// box refuse an app.
func TestParse_LenientOnRolesAndRequires(t *testing.T) {
	cases := map[string]string{
		"malformed role":       "config:\n  - {app_env: A, title: t, description: d, role: Not.A.Role}\n",
		"role is a list":       "config:\n  - {app_env: A, title: t, description: d, role: [a, b]}\n",
		"role is a mapping":    "config:\n  - {app_env: A, title: t, description: d, role: {x: 1}}\n",
		"unknown kind":         "config:\n  - {app_env: A, title: t, description: d, role: email.smtp.api_key}\n",
		"separator is a list":  "config:\n  - {app_env: A, title: t, description: d, role: ai.x.models.chat, separator: [a]}\n",
		"bad separator":        "config:\n  - {app_env: A, title: t, description: d, role: ai.x.models.chat, separator: \"a=b\"}\n",
		"requires is a string": "requires: ai\n",
		"requires is a map":    "requires: {one_of: [ai]}\n",
		"entry is a string":    "requires: [ai]\n",
		"entry has no one_of":  "requires:\n  - any_of: [ai]\n",
		"one_of is a string":   "requires:\n  - one_of: ai\n",
		"one_of is empty":      "requires:\n  - one_of: []\n",
		"member is a list":     "requires:\n  - one_of: [[ai]]\n",
		"member matches none":  "requires:\n  - one_of: [NOPE, ai.nothing]\n",
		"requires is null":     "requires:\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			m, err := Parse([]byte(rolesHead + body))
			if err != nil {
				t.Fatalf("Parse refused a manifest over %s: %v", name, err)
			}
			if len(m.FillableRoles()) != 0 {
				t.Errorf("FillableRoles = %v; want none", m.FillableRoles())
			}
			if g := m.EffectiveRequires(); len(g) != 0 {
				t.Errorf("EffectiveRequires = %v; want none", g)
			}
		})
	}
}

func TestFillableRoles(t *testing.T) {
	m := mustParseRoles(t, rolesHead+clawConfig+`  - app_env: BAD_ROLE
    title: t
    description: d
    role: ai.anthropic.token
  - app_env: DUP_KEY
    title: t
    description: d
    secret: true
    role: ai.anthropic.api_key
  - app_env: NOT_SECRET_KEY
    title: t
    description: d
    role: ai.groq.api_key
  - app_env: MISPLACED_SEP
    title: t
    description: d
    role: ai.groq.base_url
    separator: ","
  - app_env: BAD_SEP
    title: t
    description: d
    role: ai.groq.models.chat
    separator: "a\"b"
  - app_env: ENUM_ROLE
    title: t
    description: d
    type: enum
    options: [a]
    role: ai.groq.model.chat
  - app_env: FUTURE_KIND
    title: t
    description: d
    role: email.smtp.api_key
`)
	got := m.FillableRoles()
	want := map[string]string{
		"ANTHROPIC_API_KEY": "ai.anthropic.api_key",
		"OPENAI_API_KEY":    "ai.openai.api_key",
		"CUSTOM_BASE_URL":   "ai.openai_compatible.base_url",
		"CUSTOM_MODEL":      "ai.openai_compatible.model.chat",
		"CUSTOM_API_KEY":    "ai.openai_compatible.api_key",
		"CUSTOM_MODELS":     "ai.openai_compatible.models.embedding",
	}
	gotStr := map[string]string{}
	for env, r := range got {
		gotStr[env] = r.String()
	}
	if !reflect.DeepEqual(gotStr, want) {
		t.Errorf("FillableRoles =\n %v\nwant\n %v", gotStr, want)
	}

	drops := strings.Join(m.RoleDrops(), "\n")
	for _, env := range []string{"BAD_ROLE", "DUP_KEY", "NOT_SECRET_KEY", "MISPLACED_SEP", "BAD_SEP", "ENUM_ROLE", "FUTURE_KIND"} {
		if !strings.Contains(drops, "config["+env+"]") {
			t.Errorf("RoleDrops does not mention %s:\n%s", env, drops)
		}
	}
	if strings.Contains(drops, "CUSTOM_MODELS") {
		t.Errorf("RoleDrops mentions a fillable field:\n%s", drops)
	}
}

func TestEffectiveSeparator(t *testing.T) {
	semi, bad := ";", "a=b"
	cases := []struct {
		sep  *string
		want string
	}{{nil, ","}, {&semi, ";"}, {&bad, ","}}
	for _, c := range cases {
		f := ConfigField{Separator: c.sep}
		if got := f.EffectiveSeparator(); got != c.want {
			t.Errorf("EffectiveSeparator(%v) = %q; want %q", c.sep, got, c.want)
		}
	}
}

func TestCheckSeparator(t *testing.T) {
	for _, ok := range []string{",", ";", ", ", "|", " | ", "::", "\u00b7"} {
		if err := checkSeparator(ok); err != nil {
			t.Errorf("checkSeparator(%q) = %v; want ok", ok, err)
		}
	}
	for _, bad := range []string{"", "12345", "\n", "a\tb", "=", "\"", "'", "`"} {
		if err := checkSeparator(bad); err == nil {
			t.Errorf("checkSeparator(%q) = nil; want an error", bad)
		}
	}
}

func TestEffectiveRequires(t *testing.T) {
	m := mustParseRoles(t, rolesHead+clawConfig+`requires:
  - one_of: [ai]
  - one_of: [GOOGLE_API_KEY, GOOGLE_CREDENTIALS, GOOGLE_API_KEY]
  - one_of: [ai.anthropic, ai.gemini]
  - one_of: [ai.anthropic.api_key]
  - one_of: [NOPE]
  - one_of: []
  - any_of: [ai]
`)
	got := m.EffectiveRequires()
	want := [][]string{{"ai"}, {"GOOGLE_API_KEY", "GOOGLE_CREDENTIALS"}, {"ai.anthropic"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("EffectiveRequires = %v; want %v", got, want)
	}
	drops := strings.Join(m.RoleDrops(), "\n")
	for _, want := range []string{`"ai.gemini" matches no field`, `requires[3]: no member left`, `requires[4]`, `requires[5]: one_of is empty`, `requires[6]: unknown key "any_of"`} {
		if !strings.Contains(drops, want) {
			t.Errorf("RoleDrops missing %q:\n%s", want, drops)
		}
	}
}

func TestGroupSatisfied(t *testing.T) {
	m := mustParseRoles(t, rolesHead+clawConfig+`  - app_env: FUTURE_KEY
    title: t
    description: d
    secret: true
    role: email.smtp.api_key
  - app_env: BROKEN
    title: t
    description: d
    role: ai.Broken
`)
	cases := []struct {
		name   string
		group  []string
		values map[string]string
		want   bool
	}{
		{"kind met by a native key", []string{"ai"}, map[string]string{"ANTHROPIC_API_KEY": "k"}, true},
		{"kind met by a base url", []string{"ai"}, map[string]string{"CUSTOM_BASE_URL": "https://x"}, true},
		{"model fields do not count", []string{"ai"}, map[string]string{"CUSTOM_MODEL": "m", "CUSTOM_MODELS": "a;b"}, false},
		{"empty value does not count", []string{"ai"}, map[string]string{"ANTHROPIC_API_KEY": ""}, false},
		{"nothing set", []string{"ai"}, nil, false},
		{"slot met", []string{"ai.openai_compatible"}, map[string]string{"CUSTOM_API_KEY": "k"}, true},
		{"slot not met by another slot", []string{"ai.openai"}, map[string]string{"ANTHROPIC_API_KEY": "k"}, false},
		{"slot prefix is exact", []string{"ai.openai"}, map[string]string{"CUSTOM_API_KEY": "k"}, false},
		{"plain field met", []string{"GOOGLE_API_KEY", "GOOGLE_CREDENTIALS"}, map[string]string{"GOOGLE_CREDENTIALS": "c"}, true},
		{"plain field not met", []string{"GOOGLE_API_KEY"}, map[string]string{"GOOGLE_CREDENTIALS": "c"}, false},
		{"unknown kind still matches its role", []string{"email"}, map[string]string{"FUTURE_KEY": "k"}, true},
		{"malformed role matches by app_env only", []string{"BROKEN"}, map[string]string{"BROKEN": "x"}, true},
		{"three-segment member matches nothing", []string{"ai.anthropic.api_key"}, map[string]string{"ANTHROPIC_API_KEY": "k"}, false},
	}
	for _, c := range cases {
		if got := m.GroupSatisfied(c.group, c.values); got != c.want {
			t.Errorf("%s: GroupSatisfied(%v, %v) = %v; want %v", c.name, c.group, c.values, got, c.want)
		}
	}
}

// The installer writes the parsed manifest back to the instance dir with
// yaml.Marshal and parses it again on every later read, so the new keys must
// survive that round trip.
func TestRolesRoundTrip(t *testing.T) {
	m := mustParseRoles(t, rolesHead+clawConfig+"requires:\n  - one_of: [ai]\n")
	out, err := yaml.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	again := mustParseRoles(t, string(out))
	if !reflect.DeepEqual(again.FillableRoles(), m.FillableRoles()) {
		t.Errorf("roles changed on round trip:\n%v\n%v", again.FillableRoles(), m.FillableRoles())
	}
	if !reflect.DeepEqual(again.EffectiveRequires(), m.EffectiveRequires()) {
		t.Errorf("requires changed on round trip: %v", again.EffectiveRequires())
	}
	if f := again.Config[5]; f.EffectiveSeparator() != ";" {
		t.Errorf("separator changed on round trip: %q", f.EffectiveSeparator())
	}
}
