package mailpreset

import "testing"

// The table is data, so the tests guard its invariants rather than restate
// every constant: a wrong hostname is caught by the live test-send, but a
// preset that is internally inconsistent (a fixed username with no value, a
// region option with no host) would ship a form the admin cannot complete.
func TestPresetsAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range List() {
		if seen[p.ID] {
			t.Errorf("duplicate preset id %q", p.ID)
		}
		seen[p.ID] = true

		if p.Label == "" || p.CredentialLabel == "" || p.Help == "" {
			t.Errorf("%s: label, credential_label and help are all required", p.ID)
		}
		if p.Port < 1 || p.Port > 65535 {
			t.Errorf("%s: port %d out of range", p.ID, p.Port)
		}
		// Hosted reaches 587 and 2525 only; a preset defaulting to 25 or 465
		// would ship a form whose test-send times out there.
		if p.Port != 587 && p.Port != 2525 {
			t.Errorf("%s: port %d is not reachable from a hosted box", p.ID, p.Port)
		}
		if p.Encryption != "starttls" {
			t.Errorf("%s: encryption %q, want starttls", p.ID, p.Encryption)
		}

		switch p.UsernameMode {
		case UsernameFixed:
			if p.UsernameFixed == "" {
				t.Errorf("%s: username_mode fixed needs a username_fixed value", p.ID)
			}
		case UsernameUser, UsernameSameAsPassword:
			if p.UsernameFixed != "" {
				t.Errorf("%s: username_fixed set on a non-fixed username_mode", p.ID)
			}
		default:
			t.Errorf("%s: unknown username_mode %q", p.ID, p.UsernameMode)
		}

		// Host and region are exclusive: exactly one of them tells the form
		// which server to dial.
		if p.ID == Custom {
			if p.Host != "" || p.Region != nil {
				t.Errorf("custom must carry neither a host nor a region")
			}
			continue
		}
		if (p.Host == "") == (p.Region == nil) {
			t.Errorf("%s: want exactly one of host and region", p.ID)
		}
		if p.Region == nil {
			continue
		}
		if len(p.Region.Options) == 0 {
			t.Errorf("%s: region with no options", p.ID)
		}
		var hasDefault bool
		for _, o := range p.Region.Options {
			if o.Value == "" || o.Label == "" || o.Host == "" {
				t.Errorf("%s: region option %q is incomplete", p.ID, o.Value)
			}
			if o.Value == p.Region.Default {
				hasDefault = true
			}
		}
		if !hasDefault {
			t.Errorf("%s: region default %q names no option", p.ID, p.Region.Default)
		}
	}
}

func TestCustomIsPresent(t *testing.T) {
	if !Valid(Custom) {
		t.Fatal("custom must be a valid provider_type — it is what pre-preset rows migrate to")
	}
}

func TestGetAndValid(t *testing.T) {
	p, ok := Get("sendgrid")
	if !ok {
		t.Fatal("sendgrid preset missing")
	}
	if p.UsernameFixed != "apikey" {
		t.Errorf("sendgrid username_fixed = %q, want apikey", p.UsernameFixed)
	}
	if Valid("nope") {
		t.Error("unknown id reported valid")
	}
	if _, ok := Get("nope"); ok {
		t.Error("unknown id returned a preset")
	}
}

func TestLabelForFallsBackToID(t *testing.T) {
	if got := LabelFor("ses"); got != "Amazon SES" {
		t.Errorf("LabelFor(ses) = %q", got)
	}
	// A withdrawn preset must still render as something.
	if got := LabelFor("gone"); got != "gone" {
		t.Errorf("LabelFor(gone) = %q, want the id back", got)
	}
}

// List hands out a copy; a caller mutating it must not corrupt the table.
func TestListIsACopy(t *testing.T) {
	l := List()
	l[0].Label = "mutated"
	if List()[0].Label == "mutated" {
		t.Error("List returned the backing array")
	}
}

// iCloud sends over smtp.mail.me.com on 587 with STARTTLS, and the username
// is the full iCloud address (INSTALL_STEPS.md # 4).
func TestICloudPreset(t *testing.T) {
	p, ok := Get("icloud")
	if !ok {
		t.Fatal("icloud preset missing")
	}
	if p.Host != "smtp.mail.me.com" || p.Port != 587 || p.Encryption != "starttls" {
		t.Errorf("icloud = %s:%d %s, want smtp.mail.me.com:587 starttls", p.Host, p.Port, p.Encryption)
	}
	if p.UsernameMode != UsernameUser || !p.Personal {
		t.Errorf("icloud: username_mode %q personal %v, want user and personal", p.UsernameMode, p.Personal)
	}
	if p.Label != "iCloud" || p.DefaultAccountName() != "iCloud" {
		t.Errorf("icloud label %q, account name %q", p.Label, p.DefaultAccountName())
	}
}

// The Gmail preset keeps the id google_workspace, because saved accounts and
// the logo use it, and its label covers personal Gmail too.
func TestGmailPreset(t *testing.T) {
	p, ok := Get("google_workspace")
	if !ok {
		t.Fatal("google_workspace preset missing")
	}
	if p.Label != "Gmail or Google Workspace" || p.DefaultAccountName() != "Gmail" {
		t.Errorf("label %q, account name %q", p.Label, p.DefaultAccountName())
	}
	if p.Host != "smtp.gmail.com" || p.Port != 587 || !p.Personal {
		t.Errorf("gmail = %s:%d personal %v", p.Host, p.Port, p.Personal)
	}
}

// The personal accounts come first, custom last, and a personal preset has
// the steps and the page for making its app password. Its username is the
// address the user types, so it cannot be fixed or shared with the password.
func TestPresetOrderAndPersonal(t *testing.T) {
	l := List()
	if l[0].ID != "google_workspace" || l[1].ID != "icloud" || l[len(l)-1].ID != Custom {
		t.Fatalf("order = %s, %s … %s", l[0].ID, l[1].ID, l[len(l)-1].ID)
	}
	seenService := false
	for _, p := range l {
		if !p.Personal {
			seenService = true
			if len(p.Steps) > 0 {
				t.Errorf("%s: steps on a sending service", p.ID)
			}
			continue
		}
		if seenService {
			t.Errorf("%s: a personal preset after a sending service", p.ID)
		}
		if len(p.Steps) == 0 || p.SetupURL == "" || p.UsernameMode != UsernameUser {
			t.Errorf("%s: a personal preset needs steps, a setup URL and username_mode user", p.ID)
		}
	}
}
