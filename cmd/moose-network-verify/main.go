// Command moose-network-verify exercises the network-state discovery slice
// end to end on a real Linux host: NetworkManager LAN enumeration, the
// avahi-daemon.conf allow-interfaces sync, per-LAN-interface announcement,
// and IP-change replay. It is the in-VM driver for the medium QEMU lane
// (dev/test-qemu/), wiring the same packages cmd/host-agent-real does minus
// PAM — CGO-free so the lane can bake it without cross-building libpam
// (mirrors cmd/moose-storage-verify). Not part of a running moose.
//
// Usage:
//
//	moose-network-verify lan
//	    Print the NM-derived LAN set as JSON to stdout and exit.
//
//	moose-network-verify serve [-slug name]
//	    Sync the avahi allowlist (restarting avahi-daemon if it changed),
//	    publish <slug>.local per LAN interface, then watch NetworkManager
//	    and replay on every change until killed. Must run as root (conf
//	    write + systemctl restart).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/onmoose/os/internal/hostagent/avahipublisher"
	"github.com/onmoose/os/internal/hostagent/netstate"
	"github.com/onmoose/os/internal/protocol"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: moose-network-verify lan | serve [-slug name]")
		os.Exit(2)
	}

	prov := &netstate.NMProvider{}
	switch os.Args[1] {
	case "lan":
		lis, err := prov.LANInterfaces()
		if err != nil {
			slog.Error("LAN interfaces", "err", err)
			os.Exit(1)
		}
		if err := json.NewEncoder(os.Stdout).Encode(lis); err != nil {
			slog.Error("encode", "err", err)
			os.Exit(1)
		}
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		slug := fs.String("slug", "moosetest", "slug to publish as <slug>.local")
		_ = fs.Parse(os.Args[2:])

		pub := &avahipublisher.DBusPublisher{HostSuffix: protocol.AppHostSuffix, LAN: prov.LANInterfaces}
		avahiSync := &avahipublisher.Sync{Publisher: pub, LAN: prov.LANInterfaces}
		if err := avahiSync.Apply(); err != nil {
			slog.Error("avahi sync at startup", "err", err)
			os.Exit(1)
		}
		name, err := pub.Publish(*slug)
		if err != nil {
			slog.Error("publish", "slug", *slug, "err", err)
			os.Exit(1)
		}
		slog.Info("published; watching NetworkManager", "slug", *slug, "host", name)
		if err := prov.Watch(context.Background(), func() {
			if err := avahiSync.Apply(); err != nil {
				slog.Error("avahi sync", "err", err)
			}
		}); err != nil {
			slog.Error("netstate watch", "err", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q (want lan or serve)\n", os.Args[1])
		os.Exit(2)
	}
}
