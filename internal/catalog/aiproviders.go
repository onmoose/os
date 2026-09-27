package catalog

import (
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/onmoose/os/internal/manifest"
)

// aiproviders.go reads the AI provider data on the browse payload and projects
// it for the install setup page (INSTALL_SETUP.md # 4). The store adds to this
// data more often than to anything else on the payload (a new model, a new
// model type), so the reading is lenient on purpose: whatever the box does not
// understand is dropped, never the snapshot. A box on a catalog without the
// field has an empty list, and the setup page then shows the app's AI fields as
// plain fields.

// aiModelFlags is the closed list of model flags this box knows, and
// manifest.IsModelType the closed list of model types, shared with the role
// vocabulary on manifest fields. A value outside them is dropped when the
// snapshot is loaded, so the store can publish a new type before the fleet
// understands it.
var aiModelFlags = map[string]bool{"vision": true, "tools": true, "reasoning": true}

// maxLoggedDrops caps the drop list in the one log line per load. A new model
// type in the store can touch every model at once, and the line should stay
// short.
const maxLoggedDrops = 10

// AIProvider is one AI provider as the box serves it to the UI, in the order
// the store authored. Logo URLs point at the box's own routes, never at the
// asset origin, the same way an app's icon does; each is absent when the
// provider has no such logo.
type AIProvider struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	LogoURL     string `json:"logo_url,omitempty"`
	LogoDarkURL string `json:"logo_dark_url,omitempty"`
	// KeyURL is the provider's page where the user makes a key.
	KeyURL string `json:"key_url,omitempty"`
	Help   string `json:"help,omitempty"`
	// KeyPrefix is a hint: a key that does not start with it gets a warning,
	// never a block.
	KeyPrefix string `json:"key_prefix,omitempty"`
	// NativeProtocol is the API an app's native slot speaks (anthropic,
	// openai, ...). Empty means the provider is reachable only through an
	// OpenAI-compatible slot.
	NativeProtocol string `json:"native_protocol,omitempty"`
	OpenAIBaseURL  string `json:"openai_base_url,omitempty"`
	// Checked is the date the store last checked this entry against the
	// provider's own docs.
	Checked string `json:"checked,omitempty"`
	// Defaults maps a model type to the model to suggest first. Every entry
	// names a model in Models that has that type.
	Defaults map[string]string `json:"defaults,omitempty"`
	Models   []AIModel         `json:"models"`
}

// AIModel is one model of an AI provider. Types and Flags hold only values the
// box knows, and Types is never empty.
type AIModel struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Types []string `json:"types" enum:"chat,embedding,image,speech_to_text,text_to_speech,rerank"`
	Flags []string `json:"flags,omitempty" enum:"vision,tools,reasoning"`
}

// aiProviderLogoURL is the box route that serves a provider's logo, or its
// dark variant.
func aiProviderLogoURL(id string, dark bool) string {
	u := "/api/v1/ai-providers/" + url.PathEscape(id) + "/logo"
	if dark {
		u += "-dark"
	}
	return u
}

// readAIProviders decodes the raw ai_providers value leniently and returns the
// providers the box can use, in published order, plus a short note for every
// part it dropped. It never fails: a value it cannot read at all is an empty
// list.
//
// The rules: a provider with no id or no name is dropped, and a repeated id
// keeps the first. A model with no id is dropped, a repeated model id keeps the
// first, an unknown type or flag is dropped, and a model left with no known
// type is dropped. A model with no name shows its id. A defaults entry is
// dropped when its type is unknown, or its model is missing or lacks that type.
func readAIProviders(raw json.RawMessage) ([]wireAIProvider, []string) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, []string{"ai_providers is not a list"}
	}
	var (
		out     []wireAIProvider
		dropped []string
		seen    = map[string]bool{}
	)
	for i, e := range entries {
		var p wireAIProvider
		if err := json.Unmarshal(e, &p); err != nil {
			dropped = append(dropped, fmt.Sprintf("provider %d: unreadable", i))
			continue
		}
		if p.ID == "" || p.Name == "" {
			dropped = append(dropped, fmt.Sprintf("provider %d: no id or name", i))
			continue
		}
		if seen[p.ID] {
			dropped = append(dropped, fmt.Sprintf("provider %q: repeated id", p.ID))
			continue
		}
		seen[p.ID] = true
		p.Models, dropped = cleanAIModels(p.ID, p.Models, dropped)
		p.Defaults, dropped = cleanAIDefaults(p.ID, p.Defaults, p.Models, dropped)
		out = append(out, p)
	}
	return out, dropped
}

func cleanAIModels(provider string, in []wireAIModel, dropped []string) ([]wireAIModel, []string) {
	var out []wireAIModel
	seen := map[string]bool{}
	for _, m := range in {
		if m.ID == "" {
			dropped = append(dropped, fmt.Sprintf("provider %q: model with no id", provider))
			continue
		}
		if seen[m.ID] {
			dropped = append(dropped, fmt.Sprintf("provider %q: repeated model %q", provider, m.ID))
			continue
		}
		seen[m.ID] = true
		var types, flags []string
		for _, t := range m.Types {
			if manifest.IsModelType(t) {
				types = append(types, t)
			} else {
				dropped = append(dropped, fmt.Sprintf("provider %q: model %q: unknown type %q", provider, m.ID, t))
			}
		}
		if len(types) == 0 {
			dropped = append(dropped, fmt.Sprintf("provider %q: model %q: no known type", provider, m.ID))
			continue
		}
		for _, f := range m.Flags {
			if aiModelFlags[f] {
				flags = append(flags, f)
			} else {
				dropped = append(dropped, fmt.Sprintf("provider %q: model %q: unknown flag %q", provider, m.ID, f))
			}
		}
		m.Types, m.Flags = types, flags
		if m.Name == "" {
			m.Name = m.ID
		}
		out = append(out, m)
	}
	return out, dropped
}

func cleanAIDefaults(provider string, in map[string]string, models []wireAIModel, dropped []string) (map[string]string, []string) {
	if len(in) == 0 {
		return nil, dropped
	}
	out := map[string]string{}
	for typ, id := range in {
		if !manifest.IsModelType(typ) {
			dropped = append(dropped, fmt.Sprintf("provider %q: default for unknown type %q", provider, typ))
			continue
		}
		if !modelHasType(models, id, typ) {
			dropped = append(dropped, fmt.Sprintf("provider %q: default %s model %q is not in its %s models", provider, typ, id, typ))
			continue
		}
		out[typ] = id
	}
	if len(out) == 0 {
		return nil, dropped
	}
	return out, dropped
}

func modelHasType(models []wireAIModel, id, typ string) bool {
	for _, m := range models {
		if m.ID != id {
			continue
		}
		for _, t := range m.Types {
			if t == typ {
				return true
			}
		}
		return false
	}
	return false
}

// aiProviderOf projects a cleaned wire provider into the shape the UI reads.
func aiProviderOf(p *wireAIProvider) AIProvider {
	out := AIProvider{
		ID:             p.ID,
		Name:           p.Name,
		KeyURL:         p.KeyURL,
		Help:           p.Help,
		KeyPrefix:      p.KeyPrefix,
		NativeProtocol: p.NativeProtocol,
		OpenAIBaseURL:  p.OpenAIBaseURL,
		Checked:        p.Checked,
		Defaults:       p.Defaults,
		Models:         make([]AIModel, 0, len(p.Models)),
	}
	if p.LogoURL != "" {
		out.LogoURL = aiProviderLogoURL(p.ID, false)
	}
	if p.LogoDarkURL != "" {
		out.LogoDarkURL = aiProviderLogoURL(p.ID, true)
	}
	for _, m := range p.Models {
		out.Models = append(out.Models, AIModel{ID: m.ID, Name: m.Name, Types: m.Types, Flags: m.Flags})
	}
	return out
}

// AIProviders returns the AI provider data in the order the store authored it.
// A box with no catalog yet, or a catalog without the data, returns an empty
// list and no error.
func (c *Catalog) AIProviders() ([]AIProvider, error) { return c.src.aiProviders() }

// AIProviderLogoPath returns a local file path to a provider's logo, or its
// dark variant, proxied and cached like an app icon. ErrNotFound when the
// provider is unknown or has no such logo.
func (c *Catalog) AIProviderLogoPath(id string, dark bool) (string, error) {
	return c.src.aiProviderLogoPath(id, dark)
}
