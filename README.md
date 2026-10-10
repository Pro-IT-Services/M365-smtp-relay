# Graph Relay

An SMTP relay written in Go that delivers mail through **Microsoft Graph** (`/users/{mailbox}/sendMail`)
instead of SMTP AUTH. It is meant for printers, scanners, backup jobs, line-of-business applications and
anything else that can only send email over SMTP, in a Microsoft 365 tenant where basic SMTP authentication
is disabled or unwanted.

- **SMTP listeners** on 25 (STARTTLS offered), 465 (implicit TLS) and 587 (submission, STARTTLS required).
- **Host rules**: only clients matching an enabled rule (IP, CIDR or DNS name) may relay. Each rule sends as
  one Microsoft 365 mailbox, optionally rewriting the From header, and can optionally require an SMTP login.
- **Allowed sender mailboxes**: an admin-managed list of the only mailboxes rules may send as.
- **Messages up to 150 MB**: small ones as raw MIME, large ones as a Graph draft with upload sessions.
- **Synchronous delivery, no queue**: the client gets `250` only after Graph accepted the message.
- **TLS certificates**, two ways: automatic from Let's Encrypt, renewed by the relay, which **requires your
  domain's DNS zone to be hosted at Cloudflare** (DNS-01 challenge through the Cloudflare API); or **your own
  certificate and key** from files (private CA or any other source), reloaded when they change, with no
  Cloudflare needed. An RSA certificate can be served next to the ECDSA one for older devices.
- **HTTPS management portal**: hosts, allowed mailboxes, message log, test send, settings; sign-in with
  Microsoft (Entra ID app role) and an optional break-glass local account.
- **Firewall integration** (optional, Linux/nftables): the SMTP ports open only for the hosts in the portal.
- A single static binary (no CGO); SQLite storage is embedded.

## Requirements

- A Microsoft 365 tenant with Exchange Online, and rights to register an app and scope its mail permissions.
- A Linux server (the deploy scripts target Debian with systemd; nftables for the optional firewall integration).
- For the TLS certificate, one of:
  - the DNS zone of your domain hosted at **Cloudflare** (automatic Let's Encrypt certificate), or
  - your **own certificate and key** for the relay's name.

## Documentation

| Document | Content |
|---|---|
| [docs/architecture.md](docs/architecture.md) | How the code works: packages, message flow, security model, data stored |
| [docs/setup.md](docs/setup.md) | Step-by-step setup: Entra ID, Exchange Online, TLS certificate (Cloudflare or your own), server, first start, operation |
| [config.example.yaml](config.example.yaml) | Every configuration option with comments |

## Quick start (local test, no Microsoft or Cloudflare account)

```bash
cp config.example.yaml config.yaml
# in config.yaml: tls.mode: selfsigned, ports 2525/4465/5587, portal 8443,
#                 portal.microsoft_login.enabled: false, any dummy graph.* values
go run ./cmd/graphrelay -config config.yaml
```

The portal is then at `https://localhost:8443` (self-signed certificate). The first start prints a generated
admin password. Sending fails at the Graph step with a clear error, which is expected without a real tenant.

## Install

Download the archive for your server from the repository's **Releases** page, check it against
`SHA256SUMS`, extract it, copy `config.example.yaml` to `config.yaml` and edit it, then run `bash install.sh`
as root. The archive contains the binary, the installer, the systemd units and the documentation; the server
needs neither Go nor this repository. [docs/setup.md](docs/setup.md) has every step, including the
Microsoft 365 side.

## Build and test

```bash
go test ./...
go vet ./... && gofmt -l .
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-s -w" -o graphrelay ./cmd/graphrelay
```

Releases are built by `.github/workflows/build.yml`: pushing a version tag (`v1.2.3`) tests and builds
inside a throwaway container with the newest stable Go and publishes the archives. A `Dockerfile` is
included as well.
