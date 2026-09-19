//go:build linux && cgo

// Package pamverifier implements PasswordVerifier using the system PAM stack.
// It is intentionally isolated so that the shared internal/hostagent package
// (imported by both the fake binary and the real binary) has no PAM dependency.
// Only cmd/host-agent-real imports this package.
package pamverifier

import (
	"errors"

	"github.com/msteinert/pam/v2"
)

// PAMVerifier implements hostagent.PasswordVerifier using the system PAM stack.
// The PAM service name selects /etc/pam.d/<Service>; use "moose" in
// production and "moose-test" in the nspawn test lane.
type PAMVerifier struct {
	Service string // e.g. "moose"
}

// Verify calls pam_authenticate(3) via the msteinert/pam binding.
//
// Return contract (mirrors BRAIN_HOST_PROTOCOL.md "never reveal why"):
//   - (true,  nil)  — authentication succeeded.
//   - (false, nil)  — authentication denied (wrong password, unknown user,
//     locked account) — the caller maps this to {valid: false}.
//   - (false, err)  — PAM config / transport error — the handler logs this
//     and still returns {valid: false} so the brain never learns why.
func (p *PAMVerifier) Verify(user, password string) (bool, error) {
	tx, err := pam.StartFunc(p.Service, user, func(s pam.Style, msg string) (string, error) {
		switch s {
		case pam.PromptEchoOff:
			// Password prompt — return the supplied password.
			return password, nil
		case pam.PromptEchoOn:
			// Username re-prompt — return the username (already set).
			return user, nil
		case pam.ErrorMsg, pam.TextInfo:
			// Informational messages — ignore.
			return "", nil
		default:
			return "", errors.New("unhandled PAM conversation style")
		}
	})
	if err != nil {
		return false, err
	}

	if err := tx.Authenticate(0); err != nil {
		// pam.ErrAuth = wrong credentials — not a transport error. Collapse
		// ErrUserUnknown into the same bucket so the brain can't distinguish
		// "unknown user" from "wrong password" by timing or response.
		if errors.Is(err, pam.ErrAuth) || errors.Is(err, pam.ErrUserUnknown) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
