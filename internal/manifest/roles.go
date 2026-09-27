package manifest

// roles.go holds the role vocabulary on config fields and the `requires`
// groups (INSTALL_SETUP.md # 1 and # 3, APP_MANIFEST.md # D4).
//
// The box reads these keys leniently. Parse never fails over `role`,
// `separator` or `requires`, because Parse also reads every catalog manifest and
// every installed app's stored manifest, and a store manifest can be tagged
// before the whole fleet understands a new role. A field whose role the box
// cannot fill is shown as a plain field, and a `requires` member that matches
// no field is dropped. The strict rules live in Lint, which only the
// `moose manifest` CLI runs.

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// roleKinds is the closed list of kinds this box can fill. A kind is added when
// moose can fill it (email and Google OAuth are the likely next ones).
var roleKinds = map[string]bool{"ai": true}

// ProtocolOpenAICompatible is the reserved protocol of the generic slot: any
// provider with an OpenAI-compatible base URL can fill it. Every other protocol
// is a native one, matched against a provider's native_protocol.
const ProtocolOpenAICompatible = "openai_compatible"

// Role attributes. Model and Models carry a model type as a fourth segment.
const (
	AttrAPIKey  = "api_key"
	AttrBaseURL = "base_url"
	AttrModel   = "model"
	AttrModels  = "models"
)

// modelTypeList is the closed list of model types, in the order messages list
// them. The provider data reader (internal/catalog) uses the same list, so a
// role and a provider's models always speak the same types.
var modelTypeList = []string{"chat", "embedding", "image", "speech_to_text", "text_to_speech", "rerank"}

// IsModelType reports whether t is a model type this box knows.
func IsModelType(t string) bool { return slices.Contains(modelTypeList, t) }

// DefaultSeparator joins a models.<type> list when the field declares none.
const DefaultSeparator = ","

// roleSegment is one lowercase segment of a role or a requires member.
var roleSegment = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Role is a parsed role: <kind>.<protocol>.<attribute>, where the attribute is
// api_key, base_url, model.<type> or models.<type>.
type Role struct {
	Kind      string
	Protocol  string
	Attribute string // AttrAPIKey, AttrBaseURL, AttrModel or AttrModels
	ModelType string // set only for AttrModel and AttrModels
}

// Slot is the kind.protocol the role belongs to. Fields that share it form a
// slot, which the setup page draws as provider tiles.
func (r Role) Slot() string { return r.Kind + "." + r.Protocol }

// String is the role as written in a manifest.
func (r Role) String() string {
	s := r.Slot() + "." + r.Attribute
	if r.ModelType != "" {
		s += "." + r.ModelType
	}
	return s
}

// IsModel reports whether the role names one model or a list of models.
func (r Role) IsModel() bool { return r.Attribute == AttrModel || r.Attribute == AttrModels }

// splitRole splits a role into its segments when it is well formed: three or
// four lowercase segments. It knows nothing about the vocabulary, so a role
// with an unknown kind is still well formed and still counts for `requires`.
func splitRole(s string) ([]string, bool) {
	segs := strings.Split(s, ".")
	if len(segs) < 3 || len(segs) > 4 {
		return nil, false
	}
	for _, seg := range segs {
		if !roleSegment.MatchString(seg) {
			return nil, false
		}
	}
	return segs, true
}

// ParseRole parses a role against the vocabulary. The error says what is wrong
// in plain words, so Lint can print it as is.
func ParseRole(s string) (Role, error) {
	segs, ok := splitRole(s)
	if !ok {
		return Role{}, fmt.Errorf("role %q must be <kind>.<protocol>.<attribute> in lowercase segments, e.g. ai.anthropic.api_key", s)
	}
	if !roleKinds[segs[0]] {
		return Role{}, fmt.Errorf("role %q has an unknown kind %q (allowed: ai)", s, segs[0])
	}
	r := Role{Kind: segs[0], Protocol: segs[1], Attribute: segs[2]}
	switch segs[2] {
	case AttrAPIKey, AttrBaseURL:
		if len(segs) == 3 {
			return r, nil
		}
	case AttrModel, AttrModels:
		if len(segs) == 3 {
			return Role{}, fmt.Errorf("role %q needs a model type, e.g. %s.chat (allowed: %s)", s, s, strings.Join(modelTypeList, ", "))
		}
		if !IsModelType(segs[3]) {
			return Role{}, fmt.Errorf("role %q has an unknown model type %q (allowed: %s)", s, segs[3], strings.Join(modelTypeList, ", "))
		}
		r.ModelType = segs[3]
		return r, nil
	}
	return Role{}, fmt.Errorf("role %q has an unknown attribute %q (allowed: api_key, base_url, model.<type>, models.<type>)", s, strings.Join(segs[2:], "."))
}

// checkSeparator reports why a separator value is not valid: 1 to 4 printable
// characters, with no "=", no quote and no newline or other control character.
func checkSeparator(s string) error {
	if n := utf8.RuneCountInString(s); n < 1 || n > 4 {
		return fmt.Errorf("separator %q must be 1 to 4 characters", s)
	}
	for _, r := range s {
		if !unicode.IsPrint(r) || strings.ContainsRune("=\"'`", r) {
			return fmt.Errorf("separator %q may use only printable characters, with no \"=\" and no quote", s)
		}
	}
	return nil
}

// fillRole reports the field's role when the box can fill it, or why not. It
// can when the role parses, a separator is absent or valid on a models field,
// the field is type text, and a key field is secret. The last two are lint
// errors too; the box checks them so a filled key is never shown back to the
// user and a filled value never fails an enum or bool check.
func (f *ConfigField) fillRole() (Role, error) {
	if len(f.unreadable) > 0 {
		return Role{}, fmt.Errorf("%s must be a single value", strings.Join(f.unreadable, " and "))
	}
	r, err := ParseRole(f.Role)
	if err != nil {
		return Role{}, err
	}
	if f.Separator != nil {
		if r.Attribute != AttrModels {
			return Role{}, fmt.Errorf("separator is only valid on a models.<type> role")
		}
		if err := checkSeparator(*f.Separator); err != nil {
			return Role{}, err
		}
	}
	if f.Type != "" && f.Type != "text" {
		return Role{}, fmt.Errorf("a field with a role must be type text")
	}
	if r.Attribute == AttrAPIKey && !f.Secret {
		return Role{}, fmt.Errorf("a key field must be secret: true")
	}
	return r, nil
}

// EffectiveSeparator is the separator that joins a models.<type> list: the
// declared one, or DefaultSeparator. It is meaningful only on a field whose
// role FillableRoles returns with AttrModels.
func (f *ConfigField) EffectiveSeparator() string {
	if f.Separator != nil && checkSeparator(*f.Separator) == nil {
		return *f.Separator
	}
	return DefaultSeparator
}

// FillableRoles maps app_env to the role the box can fill for that field. A
// field that is not in the map is a plain field. When two fields carry the same
// role, the first one keeps it.
func (m *Manifest) FillableRoles() map[string]Role {
	out := map[string]Role{}
	seen := map[string]bool{}
	for i := range m.Config {
		f := &m.Config[i]
		if f.Role == "" {
			continue
		}
		r, err := f.fillRole()
		if err != nil || seen[r.String()] {
			continue
		}
		seen[r.String()] = true
		out[f.AppEnv] = r
	}
	return out
}

// counts reports whether a field can satisfy a requires member, and which one.
// A kind or slot member matches a field whose role starts with member + ".",
// and an app_env member matches that field. A model field never counts, and a
// malformed role matches only by its app_env. The check knows nothing about the
// vocabulary: a well-formed role with an unknown kind still counts.
func (f *ConfigField) counts(member string) bool {
	segs, wellFormed := splitRole(f.Role)
	if f.Role != "" && !wellFormed {
		return member == f.AppEnv
	}
	if wellFormed && (segs[2] == AttrModel || segs[2] == AttrModels) {
		return false
	}
	if member == f.AppEnv {
		return true
	}
	return wellFormed && isKindOrSlot(member) && strings.HasPrefix(f.Role, member+".")
}

// isKindOrSlot reports whether a requires member names a kind (one lowercase
// segment) or a slot (two). Anything else is an app_env or malformed.
func isKindOrSlot(member string) bool {
	segs := strings.Split(member, ".")
	if len(segs) > 2 {
		return false
	}
	for _, s := range segs {
		if !roleSegment.MatchString(s) {
			return false
		}
	}
	return true
}

// GroupFields returns the fields that can satisfy a requires group, in
// manifest order.
func (m *Manifest) GroupFields(group []string) []ConfigField {
	var out []ConfigField
	for _, f := range m.Config {
		for _, member := range group {
			if f.counts(member) {
				out = append(out, f)
				break
			}
		}
	}
	return out
}

// GroupSatisfied reports whether at least one field that can satisfy the group
// has a non-empty value. values maps app_env to the value the brain resolved.
func (m *Manifest) GroupSatisfied(group []string, values map[string]string) bool {
	for _, f := range m.GroupFields(group) {
		if values[f.AppEnv] != "" {
			return true
		}
	}
	return false
}

// EffectiveRequires returns the requires groups the box checks: members that
// match no field are dropped, then groups left empty, and entries the box
// cannot read.
func (m *Manifest) EffectiveRequires() [][]string {
	groups, _ := m.effectiveRequires()
	return groups
}

func (m *Manifest) effectiveRequires() ([][]string, []string) {
	var (
		out     [][]string
		dropped []string
	)
	for i, req := range m.Requires {
		if req.problem != "" {
			dropped = append(dropped, fmt.Sprintf("requires[%d]: %s", i, req.problem))
			continue
		}
		var group []string
		for _, member := range req.OneOf {
			if len(m.GroupFields([]string{member})) == 0 {
				dropped = append(dropped, fmt.Sprintf("requires[%d]: %q matches no field", i, member))
				continue
			}
			if !slices.Contains(group, member) {
				group = append(group, member)
			}
		}
		if len(group) == 0 {
			dropped = append(dropped, fmt.Sprintf("requires[%d]: no member left", i))
			continue
		}
		out = append(out, group)
	}
	return out, dropped
}

// RoleDrops lists, in short plain notes, what the box ignores in this
// manifest's role, separator and requires keys. Empty for a clean manifest. The
// catalog logs it when it loads a manifest.
func (m *Manifest) RoleDrops() []string {
	var dropped []string
	fillable := m.FillableRoles()
	for i := range m.Config {
		f := &m.Config[i]
		if f.Role == "" && f.Separator == nil && len(f.unreadable) == 0 {
			continue
		}
		if _, ok := fillable[f.AppEnv]; ok {
			continue
		}
		reason := "role repeats an earlier field"
		if _, err := f.fillRole(); err != nil {
			reason = err.Error()
		}
		dropped = append(dropped, fmt.Sprintf("config[%s]: plain field: %s", f.AppEnv, reason))
	}
	_, reqDrops := m.effectiveRequires()
	return append(dropped, reqDrops...)
}

// Requirement is one `requires` entry: the group is satisfied when at least
// one member is filled (INSTALL_SETUP.md # 3). A member is a kind (`ai`), a
// slot (`ai.anthropic`) or a plain field by its app_env.
type Requirement struct {
	OneOf []string `yaml:"one_of"`

	// problem is why the box cannot read this entry, set by the lenient
	// decoder. Lint reports it; EffectiveRequires drops the entry.
	problem string
}

// Requirements is the `requires` list. It decodes leniently: a shape the box
// cannot read becomes an entry with a problem, never a Parse error.
type Requirements []Requirement

// UnmarshalYAML never fails. Parse must not refuse a manifest over requires.
func (rs *Requirements) UnmarshalYAML(node *yaml.Node) error {
	*rs = nil
	if node.Kind != yaml.SequenceNode {
		if node.Tag != "!!null" {
			*rs = Requirements{{problem: "requires must be a list of one_of groups"}}
		}
		return nil
	}
	for _, item := range node.Content {
		*rs = append(*rs, readRequirement(item))
	}
	return nil
}

func readRequirement(node *yaml.Node) Requirement {
	if node.Kind != yaml.MappingNode {
		return Requirement{problem: "entry must be a mapping with a one_of list"}
	}
	var (
		req   Requirement
		found bool
	)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i].Value, node.Content[i+1]
		if key != "one_of" {
			return Requirement{problem: fmt.Sprintf("unknown key %q (only one_of is supported)", key)}
		}
		found = true
		if val.Kind != yaml.SequenceNode {
			return Requirement{problem: "one_of must be a list"}
		}
		for _, m := range val.Content {
			if m.Kind != yaml.ScalarNode {
				return Requirement{problem: "each one_of member must be a single name"}
			}
			req.OneOf = append(req.OneOf, m.Value)
		}
	}
	if !found {
		return Requirement{problem: "entry has no one_of list"}
	}
	if len(req.OneOf) == 0 {
		return Requirement{problem: "one_of is empty"}
	}
	return req
}

// UnmarshalYAML decodes a config field and never fails over role or separator:
// a value that is not a single scalar is recorded for Lint and read as absent,
// so the field stays a plain field.
func (f *ConfigField) UnmarshalYAML(node *yaml.Node) error {
	var unreadable []string
	if node.Kind == yaml.MappingNode {
		cp := *node
		cp.Content = slices.Clone(node.Content)
		for i := 0; i+1 < len(cp.Content); i += 2 {
			key, val := cp.Content[i].Value, cp.Content[i+1]
			if (key == "role" || key == "separator") && val.Kind != yaml.ScalarNode {
				unreadable = append(unreadable, key)
				cp.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}
			}
		}
		node = &cp
	}
	type plain ConfigField // shed the method set to avoid recursing into this func
	var p plain
	if err := node.Decode(&p); err != nil {
		return err
	}
	*f = ConfigField(p)
	f.unreadable = unreadable
	return nil
}
