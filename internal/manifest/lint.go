package manifest

import (
	"fmt"
	"slices"
	"strings"
)

// lint.go holds the strict authoring rules for roles, separators and requires
// (INSTALL_SETUP.md # 1 to # 3). Only the `moose manifest` CLI runs them. The
// box never does: it reads the same keys leniently (roles.go), so a store
// manifest that breaks a rule here still installs on every box.

// LintOptions tunes Lint.
type LintOptions struct {
	// NativeProtocols is the set of native protocols some AI provider offers.
	// nil skips the check that warns about a protocol no provider offers.
	NativeProtocols map[string]bool
}

// Lint checks the role, separator and requires keys strictly. Errors break a
// rule and should fail the lint. Warnings point at a likely mistake and leave
// the choice to the author. Both are plain sentences that say the fix.
func Lint(m *Manifest, opts LintOptions) (errs, warns []string) {
	e := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }
	w := func(format string, a ...any) { warns = append(warns, fmt.Sprintf(format, a...)) }

	byRole := map[string]string{}    // role -> first app_env that carries it
	slots := map[string][]Role{}     // slot -> its parsed roles
	slotField := map[string]string{} // slot -> first app_env in it, for messages
	var slotOrder []string
	for i := range m.Config {
		f := &m.Config[i]
		for _, key := range f.unreadable {
			e("config[%s]: %s must be a single text value", f.AppEnv, key)
		}
		if f.Separator != nil {
			if err := checkSeparator(*f.Separator); err != nil {
				e("config[%s]: %v, e.g. \",\" or \";\"", f.AppEnv, err)
			}
		}
		if f.Role == "" {
			if f.Separator != nil {
				e("config[%s]: separator is only valid on a field with a models.<type> role; remove it", f.AppEnv)
			}
			continue
		}
		r, err := ParseRole(f.Role)
		if err != nil {
			e("config[%s]: %v", f.AppEnv, err)
			continue
		}
		if first, dup := byRole[f.Role]; dup {
			e("config[%s]: role %q is also on config[%s]; each role may appear once", f.AppEnv, f.Role, first)
		} else {
			byRole[f.Role] = f.AppEnv
		}
		if r.Attribute == AttrAPIKey && !f.Secret {
			e("config[%s]: role %q is a key; add secret: true", f.AppEnv, f.Role)
		}
		if f.Type != "" && f.Type != "text" {
			e("config[%s]: a field with a role must be type text, because moose fills the value; remove type: %s", f.AppEnv, f.Type)
		}
		if len(f.Options) > 0 {
			e("config[%s]: a field with a role may not have options; moose fills the value", f.AppEnv)
		}
		if f.Default != "" {
			e("config[%s]: a field with a role may not have a default; moose fills the value", f.AppEnv)
		}
		if f.Required {
			e("config[%s]: a field with a role may not be required: true; use requires with its slot or kind instead", f.AppEnv)
		}
		if f.Separator != nil && r.Attribute != AttrModels {
			e("config[%s]: separator is only valid on a models.<type> role; remove it", f.AppEnv)
		}
		if _, ok := slots[r.Slot()]; !ok {
			slotOrder = append(slotOrder, r.Slot())
			slotField[r.Slot()] = f.AppEnv
		}
		slots[r.Slot()] = append(slots[r.Slot()], r)
	}

	for _, slot := range slotOrder {
		roles := slots[slot]
		hasAttr := func(attr string) bool {
			return slices.ContainsFunc(roles, func(r Role) bool { return r.Attribute == attr })
		}
		if !hasAttr(AttrAPIKey) && !hasAttr(AttrBaseURL) {
			e("config[%s]: slot %q has only model fields; add its api_key or base_url field", slotField[slot], slot)
		}
		protocol := roles[0].Protocol
		if protocol == ProtocolOpenAICompatible && !hasAttr(AttrBaseURL) {
			e("config[%s]: slot %q has no base_url field; add one, so moose can tell the app which server to use", slotField[slot], slot)
		}
		if protocol != ProtocolOpenAICompatible && opts.NativeProtocols != nil && !opts.NativeProtocols[protocol] {
			w("config[%s]: no AI provider offers the %q protocol, so the box shows slot %q as plain fields; check the spelling, or add the provider data first", slotField[slot], protocol, slot)
		}
	}

	lintRequires(m, slotOrder, slots, e, w)
	return errs, warns
}

func lintRequires(m *Manifest, slotOrder []string, slots map[string][]Role, e, w func(string, ...any)) {
	byEnv := make(map[string]*ConfigField, len(m.Config))
	for i := range m.Config {
		byEnv[m.Config[i].AppEnv] = &m.Config[i]
	}
	for i, req := range m.Requires {
		if req.problem != "" {
			e("requires[%d]: %s; write it as - one_of: [ai]", i, req.problem)
			continue
		}
		seen := map[string]bool{}
		for _, member := range req.OneOf {
			if seen[member] {
				e("requires[%d]: %q is listed twice; remove one", i, member)
				continue
			}
			seen[member] = true
			if segs, isRole := splitRole(member); isRole {
				e("requires[%d]: %q is a role, not a slot; name its slot (%s) or its kind", i, member, segs[0]+"."+segs[1])
				continue
			}
			f, isField := byEnv[member]
			if !isField && !isKindOrSlot(member) {
				e("requires[%d]: %q is not a kind (ai), a slot (ai.anthropic) or a field's app_env", i, member)
				continue
			}
			if isField && f.Role != "" {
				e("requires[%d]: %q has a role; name its slot or kind instead of its app_env", i, member)
				continue
			}
			if len(m.GroupFields([]string{member})) == 0 {
				e("requires[%d]: %q matches no field that can be filled (model fields do not count); fix the name or add the field", i, member)
				continue
			}
			if isField && f.Required {
				e("config[%s]: required: true conflicts with requires[%d], which lists the field; remove one of the two", member, i)
			}
		}
		if len(req.OneOf) == 1 {
			if f, ok := byEnv[req.OneOf[0]]; ok && f.Role == "" {
				w("requires[%d]: the group has only %q; set required: true on that field instead", i, f.AppEnv)
			}
		}
		for _, member := range req.OneOf {
			if !roleKinds[member] {
				continue
			}
			if differ := slotModelTypes(member, slotOrder, slots); differ != "" {
				w("requires[%d]: %q covers slots that need different model types (%s); if the app needs each of them, list the slots in separate groups", i, member, differ)
			}
		}
	}
}

// slotModelTypes describes the model types of each slot of a kind, when they
// differ between slots. It returns "" when they are all the same. A slot with no
// model field is left out: the app picks its model itself there, so it does
// not narrow which providers fit.
func slotModelTypes(kind string, slotOrder []string, slots map[string][]Role) string {
	var (
		parts []string
		first string
		same  = true
	)
	for _, slot := range slotOrder {
		if !strings.HasPrefix(slot, kind+".") {
			continue
		}
		var types []string
		for _, r := range slots[slot] {
			if r.IsModel() && !slices.Contains(types, r.ModelType) {
				types = append(types, r.ModelType)
			}
		}
		if len(types) == 0 {
			continue
		}
		slices.Sort(types)
		desc := strings.Join(types, ", ")
		if len(parts) == 0 {
			first = desc
		} else if desc != first {
			same = false
		}
		parts = append(parts, slot+": "+desc)
	}
	if same {
		return ""
	}
	return strings.Join(parts, "; ")
}
