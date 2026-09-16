//go:build !linux

// Package avahipublisher provides a no-op stub for non-Linux builds.
// DBusPublisher is only functional on Linux (where Avahi and the system DBus
// exist). This file satisfies the compiler on macOS and other platforms so
// that go build ./... works in the dev-on-Mac workflow.
//
// cmd/host-agent-real is Linux-only and is the only binary that instantiates
// DBusPublisher. cmd/host-agent (the fake binary) uses FakePublisher and never
// imports this type at runtime.
package avahipublisher

import (
	"errors"

	"github.com/onmoose/os/internal/hostagent/netstate"
)

// ErrCollision is the name-collision sentinel — defined here so callers can
// use errors.Is on both platforms without a build-tag check.
var ErrCollision = errors.New("avahipublisher: name collision")

// DBusPublisher is a non-functional stub on non-Linux platforms.
// All methods return an "not supported on this OS" error.
type DBusPublisher struct {
	HostSuffix string
	LAN        func() ([]netstate.LANInterface, error)
}

func (p *DBusPublisher) Publish(slug string) (string, error) {
	return "", errors.New("avahipublisher: DBus publisher not supported on this OS")
}

func (p *DBusPublisher) Unpublish(slug string) error {
	return errors.New("avahipublisher: DBus publisher not supported on this OS")
}

func (p *DBusPublisher) RepublishAll() error {
	return errors.New("avahipublisher: DBus publisher not supported on this OS")
}

func (p *DBusPublisher) Close() error {
	return nil
}
