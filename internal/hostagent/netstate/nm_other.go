//go:build !linux || (linux && hosted)

// Godbus-free stub for NMProvider, built for two cases: any non-Linux OS (the
// dev-on-Mac workflow), and the Linux `hosted` build. NetworkManager and the
// system DBus are Linux-only, so the real provider (nm_linux.go) is gated to
// `linux && !hosted`; the hosted cloud box has no NetworkManager and no LAN
// mDNS at all (ENVIRONMENT.md # Networking & discovery), so it takes this stub
// instead. That is what keeps godbus out of the slim cloud host-agent —
// finishing the "no DBus deps" goal of the -tags hosted split (#204/#216, #217)
// — while cmd/host-agent-real and cmd/moose-network-verify still compile under
// -tags hosted. Mirrors avahipublisher's dbus_other.go.
package netstate

import (
	"context"
	"errors"
	"time"
)

var errUnsupported = errors.New("netstate: NetworkManager provider not supported on this OS")

// NMProvider is a non-functional stub on non-Linux platforms.
type NMProvider struct {
	Debounce time.Duration
}

func (p *NMProvider) LANInterfaces() ([]LANInterface, error) { return nil, errUnsupported }

func (p *NMProvider) Watch(ctx context.Context, onChange func()) error { return errUnsupported }

func (p *NMProvider) Close() error { return nil }
