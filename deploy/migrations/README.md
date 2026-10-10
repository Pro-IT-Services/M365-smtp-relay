# Host migrations

Most of an update needs no migration: `install.sh` replaces the binary, the systemd units and the
helper scripts every time, and the relay migrates its own database when it starts.

A **host migration** is for the rest: a change to something on the server that an update does not
overwrite by itself. Examples: the firewall ruleset in `/etc/nftables.conf` (written once by
`harden-host.sh`), a key that must be added to or renamed in `config.yaml`, a file or directory
that has to move.

## How they run

`update.sh` runs every script in this directory that the server has not run yet, in name order,
after stopping the relay and backing everything up, and before installing the new files. The names
already applied are recorded in `/opt/graphrelay/host-migrations.applied`. If a migration fails,
the update is rolled back. A fresh installation (`setup.sh`) records all of them as applied without
running them, because it starts from the current state.

## Writing one

- Name it `NNNN-short-description.sh` with the next number. Never renumber, rename or edit one
  that has been released: servers have already run it.
- Make it **idempotent**: running it twice, or on a server that already has the change, must do
  nothing and succeed.
- Make it **conservative**: change only what Graph Relay created, check before overwriting, keep
  a copy of any file you replace, and validate before activating (for example `nft -c -f`).
- Exit non-zero only if the server would be left unable to run the new release.
- It runs as root with the relay stopped. The old release's files are still in place; the new
  release's files are in the directory the script lives in (`$(dirname "$0")/..`).

## When one is needed

The build fails when a file listed in `GUARD` changes, because those are the files whose changes
servers do not pick up automatically. That failure is the prompt to decide: does an existing server
need a migration for this change? Add one if so, then record the decision with:

```bash
bash deploy/migrations/guard.sh --update
```

Changes to the database schema are guarded separately by the tests in `internal/store`
(`schema_test.go`), which require a new entry in the migrations list in
`internal/store/migrations.go`.
