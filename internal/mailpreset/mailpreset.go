// Package mailpreset holds the built-in outgoing-mail provider presets
// (SERVICE_PROVISIONING.md # BYO outgoing mail). Adding an email account used
// to mean typing seven fields the admin had to look up; for every provider
// worth presetting the host, port and encryption are fixed constants, and
// often the username is too, so the preset fills them in and the admin
// supplies only the credential, the from address, and sometimes a region.
//
// The table is hardcoded here rather than served by the catalog: the catalog
// is an app-distribution channel, these constants change roughly never, and
// the credential broker's per-provider logic has to be Go anyway
// (DECISIONS.md 2026-08-27 D1, NEXT.md # On-box credential broker).
//
// A leaf package with no moose dependencies. Two consumers justify it over an
// inline constant in the API: the API serves and validates against it, and the
// broker will later need the same provider identity server-side.
package mailpreset

// Custom is the escape hatch: not a preset at all, but the persisted
// provider_type of a provider whose fields the admin typed by hand. Rows that
// predate presets migrate to this value, which is exactly right — they were
// typed by hand.
const Custom = "custom"

// Username modes. The preset says where the SMTP username comes from, because
// it differs per provider: SendGrid wants the literal string "apikey",
// Postmark wants the same server token in both fields, and SES wants a value
// only the admin has.
const (
	// UsernameUser: the admin supplies it. UsernamePrefill may seed the field.
	UsernameUser = "user"
	// UsernameFixed: the provider mandates a constant, in UsernameFixed.
	UsernameFixed = "fixed"
	// UsernameSameAsPassword: the credential goes in both fields.
	UsernameSameAsPassword = "same_as_password"
)

// Region is the one variable a preset may carry. SES and Mailgun are the only
// providers here whose region changes the SMTP host, so instead of a general
// template language each option names the host it resolves to. Mailgun is why:
// its EU host is smtp.eu.mailgun.org, a prefix rather than a region code, so
// substituting a code into one template would not reach it.
type Region struct {
	Label   string // field label, e.g. "Region"
	Default string // pre-selected option value
	Options []RegionOption
}

// RegionOption is one choice in the region select. Host is the full SMTP
// hostname that choice resolves to.
type RegionOption struct {
	Value string
	Label string
	Host  string
}

// Preset is one built-in provider. Host is empty when Region is set — the
// chosen region option carries the host instead.
type Preset struct {
	ID              string
	Label           string
	Host            string
	Port            int
	Encryption      string
	UsernameMode    string
	UsernameFixed   string
	UsernamePrefill string
	CredentialLabel string
	Help            string
	DocsURL         string
	Region          *Region
}

// sesRegions is a common subset, not every SES region. SES SMTP exists in ~19
// regions; a select that long is worse for the non-technical admin than a
// short one plus the advanced host field, which anyone outside these can use.
func sesRegions() *Region {
	return &Region{
		Label:   "Region",
		Default: "us-east-1",
		Options: []RegionOption{
			{Value: "us-east-1", Label: "US East (N. Virginia)", Host: "email-smtp.us-east-1.amazonaws.com"},
			{Value: "us-west-2", Label: "US West (Oregon)", Host: "email-smtp.us-west-2.amazonaws.com"},
			{Value: "ca-central-1", Label: "Canada (Central)", Host: "email-smtp.ca-central-1.amazonaws.com"},
			{Value: "eu-west-1", Label: "Europe (Ireland)", Host: "email-smtp.eu-west-1.amazonaws.com"},
			{Value: "eu-central-1", Label: "Europe (Frankfurt)", Host: "email-smtp.eu-central-1.amazonaws.com"},
			{Value: "ap-southeast-1", Label: "Asia Pacific (Singapore)", Host: "email-smtp.ap-southeast-1.amazonaws.com"},
			{Value: "ap-southeast-2", Label: "Asia Pacific (Sydney)", Host: "email-smtp.ap-southeast-2.amazonaws.com"},
			{Value: "ap-northeast-1", Label: "Asia Pacific (Tokyo)", Host: "email-smtp.ap-northeast-1.amazonaws.com"},
		},
	}
}

// presets is the shipped table. Every entry is 587 / STARTTLS except SMTP2GO,
// which documents 2525 as the port open in the most places. Hosted boxes reach
// 587 and 2525 but not 25 or 465 (SERVICE_PROVISIONING.md # BYO outgoing
// mail), so no preset can default to implicit TLS.
//
// Hosts, ports and username rules verified against each provider's own docs on
// 2026-08-27. Re-check them when this table is next touched — they are exactly
// the kind of fact that goes stale.
var presets = []Preset{
	{
		ID:              "ses",
		Label:           "Amazon SES",
		Port:            587,
		Encryption:      "starttls",
		UsernameMode:    UsernameUser,
		CredentialLabel: "SMTP password",
		Help:            "Create SMTP credentials in the SES console (Account dashboard → SMTP settings). They are not your AWS access keys. Pick the region your SES identity lives in.",
		DocsURL:         "https://docs.aws.amazon.com/ses/latest/dg/smtp-credentials.html",
		Region:          sesRegions(),
	},
	{
		ID:              "sendgrid",
		Label:           "SendGrid",
		Host:            "smtp.sendgrid.net",
		Port:            587,
		Encryption:      "starttls",
		UsernameMode:    UsernameFixed,
		UsernameFixed:   "apikey",
		CredentialLabel: "API key",
		Help:            "Create an API key with Mail Send permission in Settings → API Keys. The username is always the literal word apikey.",
		DocsURL:         "https://www.twilio.com/docs/sendgrid/for-developers/sending-email/getting-started-smtp",
	},
	{
		ID:              "mailgun",
		Label:           "Mailgun",
		Port:            587,
		Encryption:      "starttls",
		UsernameMode:    UsernameUser,
		UsernamePrefill: "postmaster@",
		CredentialLabel: "SMTP password",
		Help:            "Find the SMTP login and password under Sending → Domain settings → SMTP credentials. The login looks like postmaster@your-domain.com. Pick the region your domain was created in.",
		DocsURL:         "https://documentation.mailgun.com/docs/mailgun/user-manual/sending-messages/send-smtp",
		Region: &Region{
			Label:   "Region",
			Default: "us",
			Options: []RegionOption{
				{Value: "us", Label: "US", Host: "smtp.mailgun.org"},
				{Value: "eu", Label: "EU", Host: "smtp.eu.mailgun.org"},
			},
		},
	},
	{
		ID:              "postmark",
		Label:           "Postmark",
		Host:            "smtp.postmarkapp.com",
		Port:            587,
		Encryption:      "starttls",
		UsernameMode:    UsernameSameAsPassword,
		CredentialLabel: "Server API token",
		Help:            "Copy the Server API token from your Postmark server's API Tokens tab. Postmark uses it as both the username and the password.",
		DocsURL:         "https://postmarkapp.com/developer/user-guide/send-email-with-smtp",
	},
	{
		ID:              "brevo",
		Label:           "Brevo",
		Host:            "smtp-relay.brevo.com",
		Port:            587,
		Encryption:      "starttls",
		UsernameMode:    UsernameUser,
		CredentialLabel: "SMTP key",
		Help:            "Go to SMTP & API in Brevo. The login shown there looks like 12ab34@smtp-brevo.com. That is the username, not your account email. The password is an SMTP key, not an API key.",
		DocsURL:         "https://help.brevo.com/hc/en-us/articles/7924908994450-Send-transactional-emails-using-Brevo-SMTP",
	},
	{
		ID:              "resend",
		Label:           "Resend",
		Host:            "smtp.resend.com",
		Port:            587,
		Encryption:      "starttls",
		UsernameMode:    UsernameFixed,
		UsernameFixed:   "resend",
		CredentialLabel: "API key",
		Help:            "Create an API key with sending access in the Resend dashboard. The username is always the literal word resend.",
		DocsURL:         "https://resend.com/docs/send-with-smtp",
	},
	{
		ID:              "smtp2go",
		Label:           "SMTP2GO",
		Host:            "mail.smtp2go.com",
		Port:            2525,
		Encryption:      "starttls",
		UsernameMode:    UsernameUser,
		CredentialLabel: "SMTP password",
		Help:            "Create an SMTP user under Sending → SMTP Users and use its username and password. Port 2525 is the one SMTP2GO recommends because it is open in the most places.",
		DocsURL:         "https://support.smtp2go.com/hc/en-gb/articles/223087627-SMTP-Settings",
	},
	{
		ID:              "google_workspace",
		Label:           "Google Workspace",
		Host:            "smtp.gmail.com",
		Port:            587,
		Encryption:      "starttls",
		UsernameMode:    UsernameUser,
		CredentialLabel: "App password",
		Help:            "The username is the full Google Workspace address. The password is a 16-character app password, which needs 2-Step Verification turned on for that account. Your normal Google password will not work.",
		DocsURL:         "https://knowledge.workspace.google.com/admin/gmail/send-email-from-a-printer-scanner-or-app",
	},
	{
		ID:              "custom",
		Label:           "Custom SMTP server",
		Port:            587,
		Encryption:      "starttls",
		UsernameMode:    UsernameUser,
		CredentialLabel: "Password",
		Help:            "Enter the SMTP details your provider gives you.",
		DocsURL:         "",
	},
}

// List returns every preset, including custom, in display order.
func List() []Preset {
	out := make([]Preset, len(presets))
	copy(out, presets)
	return out
}

// Get returns the preset with the given id. The second result is false for an
// unknown id, which the API turns into a 422.
func Get(id string) (Preset, bool) {
	for _, p := range presets {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}

// Valid reports whether id names a shipped preset (custom included). It is the
// check the API runs before persisting a provider_type.
func Valid(id string) bool {
	_, ok := Get(id)
	return ok
}

// LabelFor returns the display label for a provider_type, or the id itself
// when it names no shipped preset — a provider registered against a preset we
// later withdraw must still render as something.
func LabelFor(id string) string {
	if p, ok := Get(id); ok {
		return p.Label
	}
	return id
}
