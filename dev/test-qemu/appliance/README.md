# Appliance overlay

Static, checked-in files that belong to the **appliance** image itself, copied into the generated `mkosi.extra/` tree by `bootstrap.sh` at their in-image paths.

They are here, and not in `mkosi.extra/`, because that directory is generated on every build and gitignored. These files are product configuration, not build output: on a real appliance they are shipped by the moose `.deb` (`BUILD.md` # SSH). The medium lane is the only place that builds an appliance image today, so this is where they live until that package exists.

Nothing test-specific goes in here. The harness's own files (the root key, the harness sshd drop-in, the assertion script) stay in `bootstrap.sh`, so a reader can tell the shipped posture from the scaffolding around it.

- `etc/nftables.d/moose-ssh.conf` — scopes `:22` to the LAN and the mesh (`BUILD.md` # SSH).
- `etc/ssh/sshd_config.d/moose-hardening.conf` — the two static sshd settings that hold whether or not any account has SSH on.
- `etc/systemd/system/moose-ssh-firewall.service` — loads the nftables rule at boot.
