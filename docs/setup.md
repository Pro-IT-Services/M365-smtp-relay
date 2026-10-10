# Setup guide

From nothing to a working relay. Examples use the relay name `relay.example.com`, the server address
`10.0.0.25`, the domain `example.com` and the sender mailbox `notifications@example.com`; replace them with
your own. For how the code works, see [architecture.md](architecture.md).

| Step | What | Where |
|---|---|---|
| 1 | Server and DNS name | your network |
| 2 | App registration, client secret | Microsoft Entra admin center |
| 3 | Mail permissions limited to the relay mailboxes | Exchange Online |
| 4 | Portal admin role | Microsoft Entra admin center |
| 5 | TLS certificate: automatic via Cloudflare, or your own | Cloudflare, or your CA |
| 6 | Download the release, configure, install, start | server |
| 7 | First sign-in, mailboxes, host rules, test | portal |
| 8 | Optional SMTP login per host | portal + device |
| 9–11 | Break-glass login, maintenance, troubleshooting | |

You need: Application Administrator (or Global Administrator) in Entra ID, Exchange Administrator, root on
a Linux server, and for the TLS certificate **either** your domain's DNS zone in Cloudflare (automatic
Let's Encrypt certificate) **or** your own certificate and key (section 5). Plan about an hour, plus up to 2 hours for
Exchange permission changes to take effect.

---

## 1. Server and DNS name

- A Linux server or container (the deploy scripts target **Debian 12/13** with systemd and nftables) with
  outbound HTTPS to `login.microsoftonline.com` and `graph.microsoft.com`, plus `api.cloudflare.com` and
  Let's Encrypt if the certificate is obtained automatically. No inbound access from the internet is needed.
- A DNS name for the relay, e.g. `relay.example.com` → `10.0.0.25`. Clients and admins use it. For a private
  address, prefer an internal DNS record; if you put it into public DNS at Cloudflare, use **DNS only**
  (grey cloud), never proxied.
- A TLS certificate for that name. For the automatic one, the domain's DNS zone must be hosted at
  **Cloudflare**; otherwise bring your own certificate. Section 5 covers both.

Optional: harden a fresh Debian host (updates, automatic security updates, key-only SSH, default-drop
firewall). The script `harden-host.sh` is in the release archive (step 6a). Read it first and adjust
`TRUSTED_V4` (networks allowed to reach SSH and the portal), then run it on the server as root:

```bash
bash harden-host.sh
```

The firewall step rolls itself back after 120 seconds. Within that time, open a **new** SSH session to the
server, which proves you can still log in, and confirm there:

```bash
systemctl stop nft-rollback.timer
```

## 2. App registration and client secret (Entra ID)

1. Open <https://entra.microsoft.com> → **Entra ID → App registrations → + New registration**.
2. Fill in:
   - **Name:** e.g. `Graph Relay`
   - **Supported account types:** *Accounts in this organizational directory only (Single tenant)*
   - **Redirect URI:** platform **Web**, `https://relay.example.com:8443/auth/callback`
3. Click **Register**. On the **Overview** page note:
   - **Application (client) ID** → `GRAPH_CLIENT_ID`
   - **Directory (tenant) ID** → `GRAPH_TENANT_ID` (the GUID, not the domain name)
4. **Certificates & secrets → Client secrets → + New client secret**. Pick an expiry (max. 24 months), click
   **Add**, and **immediately copy the `Value` column** → `GRAPH_CLIENT_SECRET`. It is shown only once. Do not
   copy the *Secret ID*: that is a GUID and gives `AADSTS7000215: Invalid client secret`.
5. Put a reminder before the secret expires (see section 10).

**Do not add `Mail.Send` or `Mail.ReadWrite` under API permissions.** Granted there, with admin consent, the
app could send as and read **every mailbox in the tenant**, and that grant also bypasses the Exchange scope in
step 3 (the two are additive). If they were added already: on the **API permissions** page click **⋯** on each
row → **Revoke admin consent**, then **⋯** → **Remove permission**. Only the delegated `User.Read` should
remain. Check under **Enterprise applications → (app) → Permissions → Admin consent** that no `Mail.*`
application permission is left.

## 3. Mail permissions limited to the relay mailboxes (Exchange Online)

The relay sends through Graph as specific mailboxes. It needs **Mail.Send**, plus **Mail.ReadWrite** for
messages over ~3 MB (sent as drafts with attachments). Exchange **RBAC for Applications** grants them for a
defined set of mailboxes only.

The sender mailboxes can be user mailboxes or **shared mailboxes** (no licence needed), e.g.
`notifications@example.com`, `scanner@example.com`.

### 3a. Tag the mailboxes

Set **custom attribute 15** to `GraphRelay` on every mailbox the relay may send as. Either:

- **Exchange admin center** (<https://admin.exchange.microsoft.com>): **Recipients → Mailboxes** → click the
  mailbox → **Others** tab → **Custom attributes** → field **15** = `GraphRelay` → **Save**, or
- **PowerShell** (also needed for 3c):

```powershell
Install-Module ExchangeOnlineManagement -Scope CurrentUser    # once
Connect-ExchangeOnline -UserPrincipalName admin@example.com
Set-Mailbox notifications@example.com -CustomAttribute15 "GraphRelay"
```

If attribute 15 is already used in your tenant, pick another free one and change the filter in 3c.

### 3b. Find the service principal's Object ID

Exchange needs the Object ID of the **Enterprise application** (service principal). The portal shows three
similar GUIDs and only one is right:

| Where | Field | Use it? |
|---|---|---|
| App registration → Overview | Directory (tenant) ID | no, that's the tenant |
| App registration → Overview | Object ID | no, that's the app registration |
| Enterprise application → Properties | Object ID | **yes** |

The reliable way is to look it up from the client ID:

```powershell
Install-Module Microsoft.Graph.Applications -Scope CurrentUser    # once
Connect-MgGraph -Scopes Application.Read.All -NoWelcome
$appId    = "<Application (client) ID>"
$objectId = (Get-MgServicePrincipal -Filter "appId eq '$appId'").Id
$objectId   # must differ from both IDs on the App registration overview
```

If it comes back empty, the Enterprise application doesn't exist yet: create it with
`New-MgServicePrincipal -AppId $appId` (needs `Connect-MgGraph -Scopes Application.ReadWrite.All`).

### 3c. Create the scoped role assignments

In the same PowerShell window:

```powershell
New-ServicePrincipal -AppId $appId -ObjectId $objectId -DisplayName "Graph Relay"
Get-ServicePrincipal -Identity $appId | Format-List DisplayName,AppId,ObjectId    # check

New-ManagementScope -Name "Graph Relay mailboxes" `
  -RecipientRestrictionFilter "CustomAttribute15 -eq 'GraphRelay'"

New-ManagementRoleAssignment -App $appId -Role "Application Mail.Send" `
  -CustomResourceScope "Graph Relay mailboxes"
New-ManagementRoleAssignment -App $appId -Role "Application Mail.ReadWrite" `
  -CustomResourceScope "Graph Relay mailboxes"
```

### 3d. Verify

```powershell
Test-ServicePrincipalAuthorization -Identity $appId -Resource notifications@example.com   # InScope = True
Test-ServicePrincipalAuthorization -Identity $appId -Resource some.user@example.com       # InScope = False
Disconnect-ExchangeOnline -Confirm:$false; Disconnect-MgGraph
```

Changes take **30 minutes to 2 hours** to reach Graph; until then sends fail with `403 ErrorAccessDenied`.

### 3e. Message size (optional)

Exchange limits each mailbox to 35 MB per sent message by default (about 25 MB of attachments after encoding).
The relay accepts up to `smtp.max_message_bytes` (the example config uses 35 MB, the maximum is 150 MB). To
send larger mail, raise both:

```powershell
Set-Mailbox notifications@example.com -MaxSendSize 150MB
```

Internal recipients also have a `MaxReceiveSize` (default 36 MB); external servers apply their own limits.

## 4. Portal admin role (Entra ID)

Only users holding the app role **Relay.Admin** can sign in to the portal.

1. **Entra ID → App registrations →** your app **→ App roles → + Create app role**:
   - **Display name:** `Relay Admin`
   - **Allowed member types:** **Users/Groups** (with *Applications* only, it can't be assigned to people)
   - **Value:** `Relay.Admin` (exactly)
   - **Description:** `Can manage Graph Relay`; tick **Enable this app role** → **Apply**.
2. **Entra ID → Enterprise applications →** your app **→ Users and groups → + Add user/group** → select
   users or a group → role **Relay Admin** → **Assign**. Group assignment needs Entra ID P1, and roles are
   **not inherited through nested groups**: users must be direct members of the assigned group.
3. Recommended: **Enterprise applications →** your app **→ Properties → Assignment required? = Yes**.
4. Conditional Access and MFA policies apply to this sign-in like to any other app.

## 5. TLS certificate: automatic (Cloudflare) or your own

The relay needs a certificate for its name (`relay.example.com`). Choose one of two ways.

| | A. Automatic (Let's Encrypt) | B. Your own certificate |
|---|---|---|
| Needs | **The domain's DNS zone hosted at Cloudflare**, and an API token for it | A certificate and key for the relay's name, from your own CA or any other source |
| Renewal | Automatic, by the relay | You replace the files; the relay reloads them |
| Trusted by devices | Yes, publicly trusted (old devices may need the root imported, see section 8) | Only by devices that have your CA's root certificate |
| `tls.mode` | `acme` | `files` |

**Cloudflare is only required for option A.** The relay proves control of the name to Let's Encrypt by
creating a temporary DNS record through the Cloudflare API (DNS-01 challenge), so the zone of your domain
(`example.com`) must be managed in Cloudflare. If your DNS is hosted elsewhere and you don't want to move it,
use option B. The relay's own DNS record does not have to be public in either case.

### 5a. Option A: Cloudflare API token

1. <https://dash.cloudflare.com> → profile → **My Profile → API Tokens → Create Token** → template
   **Edit zone DNS** → **Use template**.
2. **Permissions:** `Zone · DNS · Edit` and (**+ Add more**) `Zone · Zone · Read`.
3. **Zone Resources:** `Include · Specific zone · example.com`.
4. Optional: **Client IP Address Filtering** with your server's public egress IP; an expiry date.
5. **Create Token** and copy it → `CLOUDFLARE_API_TOKEN` (shown only once). To check it:

```bash
curl -s -H "Authorization: Bearer <token>" https://api.cloudflare.com/client/v4/user/tokens/verify
```

In `config.yaml` (step 6b) keep `tls.mode: acme` and set `tls.acme_email`.

### 5b. Option B: your own certificate and key

No Cloudflare account, token or Let's Encrypt is involved. Put the files on the server and point the relay
at them in `config.yaml` (step 6b):

```yaml
tls:
  mode: files
  cert_file: /etc/graphrelay/tls/fullchain.pem   # server certificate first, then the intermediates
  key_file: /etc/graphrelay/tls/privkey.pem
  # optional, for devices that can't use the first certificate (e.g. RSA next to ECDSA):
  # fallback_cert_file: /etc/graphrelay/tls/fullchain-rsa.pem
  # fallback_key_file: /etc/graphrelay/tls/privkey-rsa.pem
```

- The certificate must list the relay's name (`hostname`) as a DNS name; the relay warns at startup if it
  doesn't, or if the certificate is expired or about to expire. A key that doesn't match stops it.
- The files must be readable by the `graphrelay` user and nobody else who shouldn't have the key:
  `install -d -o root -g graphrelay -m 0750 /etc/graphrelay/tls`, files `root:graphrelay`, mode `0640`.
  That user is created by the installer, so put the files in place after step 6b and before starting the
  relay (6e).
- To renew, replace the files (write the new ones next to them and `mv` them into place). The relay picks
  them up within about a minute, without a restart, and logs "TLS certificate reloaded".
- Devices need your CA's root certificate in their trust store; distribute it from your CA. The portal's
  certificate download card is not shown in this mode.

## 6. Install, configure, start

The relay is installed from a **release archive**. The server needs neither Go nor this repository. Two
kinds of installation are supported; pick one:

- **Direct install** (6a–6e): a systemd service on a Debian-style host. Supports the nftables firewall
  integration. This is the recommended form.
- **Docker** (6f): a container managed with Docker Compose.

### 6a. Download the release

Releases are built by the repository's GitHub Actions workflow and published on the repository's
**Releases** page. Each release has one archive per CPU architecture and a `SHA256SUMS` file. An archive
contains the `graphrelay` binary, the setup and update scripts, the systemd units, the firewall helpers, the
hardening script, the host migrations, the Docker files, `config.example.yaml` and this documentation.

On the server, as root (replace `OWNER/REPO` and the version; use `arm64` instead of `amd64` on ARM):

```bash
VERSION=v1.1.0
BASE=https://github.com/OWNER/REPO/releases/download/$VERSION
cd /root
curl -fLO "$BASE/graphrelay_${VERSION}_linux_amd64.tar.gz"
curl -fLO "$BASE/SHA256SUMS"
sha256sum -c --ignore-missing SHA256SUMS      # must print: OK
tar -xzf "graphrelay_${VERSION}_linux_amd64.tar.gz"
cd "graphrelay_${VERSION}_linux_amd64"
```

If the repository is private, those URLs need a login: download the archive and `SHA256SUMS` from the
Releases page in your browser, copy them to the server with `scp`, then continue from the `sha256sum` line.

### 6b. Install

In the extracted directory:

```bash
bash setup.sh
```

`setup.sh` is for the first installation only (it refuses to run over an existing one). It

- creates the `graphrelay` user and `/opt/graphrelay`, and installs the binary, the systemd units and the
  firewall helpers,
- creates `/opt/graphrelay/config.yaml` from the example and an empty `/etc/graphrelay.env` (mode 0600),
- installs the update tooling: `graphrelay-update`, plus `harden-host.sh` in `/opt/graphrelay/bin`,
- writes `/etc/graphrelay/update.conf` with the repository the archive was built from, so later updates know
  where to look. For a private repository add `--token-file /etc/graphrelay/update.token` (a root-only file
  holding a GitHub token that can read the repository),
- does **not** start the relay.

With `tls.mode: files`, put your certificate and key in place now (step 5b).

### 6c. Configuration file

```bash
nano /opt/graphrelay/config.yaml
```

Set at least:

| Setting | Value |
|---|---|
| `hostname` | `relay.example.com` |
| `data_dir` | `/opt/graphrelay/data` (already set by `setup.sh`) |
| `tls.mode` | `acme` (automatic, needs Cloudflare) or `files` (your own certificate), see section 5 |
| `tls.acme_email` | `acme` mode: an address for Let's Encrypt expiry notices |
| `tls.skip_dns_propagation_check` | `acme` mode: `true` only if your network redirects all outbound DNS to an internal resolver (see section 11) |
| `tls.rsa_fallback` | `acme` mode: `true` (default) also keeps an RSA certificate for devices that can't use ECDSA |
| `smtp.max_message_bytes` | up to `157286400` (150 MB) |
| `portal.allowed_cidrs` | networks allowed to open the portal |
| `portal.microsoft_login.enabled` | `true`, `required_roles: ["Relay.Admin"]` |
| `portal.allow_local_login` | `${GRAPHRELAY_ALLOW_LOCAL_LOGIN}` (break-glass switch, section 9) |
| `firewall.allowlist_file` | `/opt/graphrelay/data/firewall-allowlist.txt` if you use the nftables integration |
| `firewall.blocked_dir` | `/run/graphrelay-fw` to show firewall-blocked addresses on the Activity page (needs the ruleset from `harden-host.sh`) |

Keep secrets out of the file: `graph.*` and `tls.cloudflare_api_token` already reference `${...}` variables
that are read from `/etc/graphrelay.env` (step 6d).

### 6d. Secrets

```bash
nano /etc/graphrelay.env       # root:root, mode 0600
```

```
CLOUDFLARE_API_TOKEN=...          # only with tls.mode: acme
GRAPH_TENANT_ID=xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
GRAPH_CLIENT_ID=xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
GRAPH_CLIENT_SECRET=...
```

No spaces around `=`, no quotes.

### 6e. Check and start

```bash
set -a; . /etc/graphrelay.env; set +a
/opt/graphrelay/graphrelay -check-config -config /opt/graphrelay/config.yaml   # must say: is valid
systemctl start graphrelay
journalctl -u graphrelay -f
```

| Log line | Meaning |
|---|---|
| `created portal admin account ... password=...` | Local break-glass password, printed once (needed only if section 9 is ever used) |
| `certificate obtained successfully` | `acme` mode: Cloudflare token and DNS-01 work (first start takes 1–2 minutes) |
| `SMTP listening` ×3, `management portal listening` | Relay is up |
| `Microsoft Graph authentication OK` | Tenant ID, client ID and secret are correct |
| `firewall allowlist updated ... entries=0` | No host rules yet, so the SMTP ports are still closed |

Optionally harden the host (OS updates, key-only SSH, default-drop firewall that opens the SMTP ports only
for the portal's host rules). Read the script first; it changes SSH and firewall settings:

```bash
bash /opt/graphrelay/bin/harden-host.sh
```

**Limiting outbound traffic (optional).** With `--restrict-outbound` the firewall also limits what the
server itself may connect to, so a compromised relay can't reach the rest of your network or the internet
at large:

```bash
bash /opt/graphrelay/bin/harden-host.sh --restrict-outbound
```

| Allowed outbound | Why |
|---|---|
| DNS (port 53) to the resolvers in `/etc/resolv.conf` only | Name resolution |
| Ports 80/443 to Microsoft (`login.microsoftonline.com`, `graph.microsoft.com`, `outlook.office.com`, `outlook.office365.com`) | Sending mail, portal sign-in |
| Ports 80/443 to Let's Encrypt and `api.cloudflare.com` | Certificate (`acme` mode) |
| Ports 80/443 to GitHub (`api.github.com`, `github.com`, release downloads) | `graphrelay-update` |
| Ports 80/443 to the package mirrors in `/etc/apt/sources.list*` | OS updates |
| NTP, DHCP, ICMP, replies to inbound connections | Basics |

Everything else is refused, including SSH and SMTP from the relay to other machines. Things to know:

- A firewall filters addresses, not names, and these services change their addresses often. The helper
  `graphrelay-egress-sync` resolves the names every 30 seconds and allows each address for a day after it
  was last seen. Rarely, a service hands out an address a few seconds before the helper has seen it; that
  connection is refused, the sending device gets a temporary error (`451`) and retries.
- Your own destinations (a monitoring or log server, an internal package mirror, a proxy) go into
  `/etc/graphrelay/egress-hosts.conf`, one host name, address or network per line.
- If something stops working, `nft list set inet filter egress_blocked4` shows the destinations that were
  refused, with packet counters.
- In `acme` mode the certificate check asks the domain's own name servers directly, which the DNS limit
  refuses. Set `tls.skip_dns_propagation_check: true` when you use this option.
- `--open-outbound` removes the limit again. Running the script without either option keeps what is set.

### 6f. Alternative: Docker

Needs Docker with the compose plugin. Download and extract the release as in 6a, then:

```bash
bash docker/setup.sh                      # --dir DIR to install somewhere other than /opt/graphrelay-docker
cd /opt/graphrelay-docker
nano config.yaml                          # as in 6c, but keep data_dir and the 2525/4465/5587 ports
nano graphrelay.env                       # as in 6d
docker compose run --rm graphrelay -check-config -config /etc/graphrelay/config.yaml
docker compose up -d && docker compose logs -f
```

`docker/setup.sh` builds the image `graphrelay:<version>` locally from the binary in the archive (nothing
is pulled from a registry except the small base image) and creates the directory with `compose.yaml`,
`config.yaml`, `graphrelay.env`, `data/` and the update tool `graphrelay-docker-update`. The container runs
as an unprivileged user with a read-only root filesystem and no capabilities; `compose.yaml` maps the host
ports 25, 465, 587 and 8443 to it.

Differences from a direct install:

- Leave `firewall.allowlist_file` and `firewall.blocked_dir` empty. The nftables integration is for direct
  installs; restrict the published ports with your host or network firewall.
- With `tls.mode: files`, mount the certificate directory into the container (add a read-only volume in
  `compose.yaml`) and make the files readable by uid 10001.
- Where this guide says `systemctl restart graphrelay`, use `docker compose restart`; logs are
  `docker compose logs`; the env file is `/opt/graphrelay-docker/graphrelay.env`.

## 7. First sign-in, mailboxes, host rules, test

1. Open `https://relay.example.com:8443` and click **Sign in with Microsoft**.
2. **Settings → Allowed sender mailboxes:** enter the tagged mailboxes from 3a, one per line → **Save
   mailboxes**. Only these can be chosen for host rules and test messages; the relay also rejects mail for any
   rule whose mailbox is not on the list.
3. **Hosts → Add host:**
   - **Name:** e.g. `backup-server`
   - **Address / network:** an IP (`10.0.0.50`), a CIDR (`10.0.1.0/24`) or a DNS name
   - **Sends as mailbox:** choose from the list
   - **Rewrite the From header:** on (unless the mailbox has *Send As* rights for the device's own From address)
   - **Enabled:** on → **Save**
4. With the firewall integration, the SMTP ports open for that address within seconds:
   `nft list set inet filter smtp4`.
5. **Test:** choose the mailbox, send to yourself. A `403 ErrorAccessDenied` means the Exchange scope (step 3)
   is not active yet.
6. Point the device at `relay.example.com`, port 25 (or 587 with STARTTLS). Check **Log** for `sent` entries.
7. If a device doesn't get through, open **Activity**. It lists every connection attempt with the reason it
   failed (TLS too old, no common cipher, certificate not trusted, wrong login, no host rule) and, with the
   firewall integration, the addresses that were blocked before reaching the relay.

For each new sender mailbox: tag it (3a), add it under **Settings → Allowed sender mailboxes**, then pick it in
a host rule.

## 8. Optional SMTP login per host

In the host form, **SMTP login (optional)** takes a username and a password (**Generate** creates a random
one; copy it now, it is stored only as a hash). With a login set, a client must come from the rule's address
**and** log in; without a login it gets `530 Authentication required`. Leave both fields empty for devices
that can't authenticate.

Device settings: server `relay.example.com`, port **587 with STARTTLS** (or 25 with STARTTLS, or **465 with
SSL/TLS**), authentication on, mechanism PLAIN or LOGIN. AUTH is only offered over TLS.

**If the device doesn't trust the relay's certificate:** many printers and appliances ship without current
root certificates. Under **Settings → TLS certificates** download the **Root CA** (PEM, or DER/.cer for
devices that want a binary file) and import it into the device's trusted CA store. If it still refuses,
import the intermediate CA certificates too, or the CA bundle. Don't import the server certificate itself:
it is renewed about every 60 days. Five failed logins from
one IP lock it out for 15 minutes; failures appear in **Log**.

## 9. Break-glass password login

Normally only Microsoft sign-in is possible. If Entra ID sign-in breaks:

```bash
echo 'GRAPHRELAY_ALLOW_LOCAL_LOGIN=true' >> /etc/graphrelay.env
systemctl restart graphrelay
```

Sign in as `admin`. To set a new password (reads the env file, runs as the service user so the database keeps
its owner):

```bash
set -a; . /etc/graphrelay.env; set +a
cd /opt/graphrelay && setpriv --reuid=graphrelay --regid=graphrelay --init-groups \
  ./graphrelay -config config.yaml -reset-admin-password
```

Turn it off again:

```bash
sed -i '/^GRAPHRELAY_ALLOW_LOCAL_LOGIN=/d' /etc/graphrelay.env
systemctl restart graphrelay
```

## 10. Maintenance

| What | When | How |
|---|---|---|
| Client secret | Before it expires | New secret (step 2.4), update `/etc/graphrelay.env`, `systemctl restart graphrelay`, delete the old secret |
| Cloudflare token (`acme` mode) | If it has an expiry | New token (step 5a), update the env file, restart |
| Own certificate (`files` mode) | Before it expires | Replace the certificate and key files (step 5b); no restart needed |
| TLS certificate (`acme` mode) | Automatic | Renewed about 30 days before expiry |
| OS updates | Automatic | `unattended-upgrades` (set up by `harden-host.sh`) |
| Relay update | When a new release is published | `graphrelay-update` (Docker: `graphrelay-docker-update`), see below |
| Backup | Regularly | `/opt/graphrelay/data/` (database, certificates), `/opt/graphrelay/config.yaml` and `/etc/graphrelay.env` restore everything |
| Message log | Automatic | Rows older than `log_retention_days` (default 30) are deleted |

### Updating

```bash
graphrelay-update --check     # shows the installed and the newest version; exit code 10 = update available
graphrelay-update             # update to the newest release
```

| Command | What it does |
|---|---|
| `graphrelay-update --version v1.2.3` | Install exactly that release |
| `graphrelay-update --archive FILE` | Install from an archive you copied to the server (no network needed; `SHA256SUMS` next to it is checked) |
| `bash update.sh` in an extracted archive | Install that release |
| `--force` | Reinstall the same version, or go back to an older one |
| `--keep N` | Number of backups to keep (default 5) |

The Docker tool `graphrelay-docker-update` takes the same options, plus `--dir`.

The tool reads the repository from `/etc/graphrelay/update.conf`, asks GitHub for the newest release,
downloads the archive for the server's architecture and verifies it against `SHA256SUMS`. It then hands
over to the `update.sh` **inside the new release**, so an update always runs the steps the new version
needs, even ones the installed tool didn't know about. Every update runs all of them, in this order:

1. **Preflight:** checks that it can run and that the release is complete.
2. **Validate the configuration with the new binary** (`-check-config`). Nothing has been touched yet; an
   update that would not start with your config stops here.
3. **Back up the installed files** into `/opt/graphrelay/backups/`.
4. **Stop the relay.** Clients get a connection error for the next seconds and retry.
5. **Back up the database.**
6. **Host migrations:** every script in `migrations/` that this server has not run yet (see below).
7. **Install the new files:** binary, systemd units, firewall helpers, update tooling. Your `config.yaml`
   and env file are never overwritten.
8. **Start the relay.** It migrates its database to the new schema while starting.
9. **Health check:** the service is active and listening, the running version is the new one, and the log
   shows no fatal error.
10. **Clean up:** backups beyond `--keep` are removed.

If anything from step 4 on fails, the tool restores the previous files, database and firewall ruleset,
starts the old version again and exits with an error. Sending devices only see a short outage. (The Docker
tool runs the same sequence with "build the new image" in place of host migrations.)

To be told about updates without installing them, run the check from cron or a monitoring system and alert
on exit code 10. Automatic installs are possible the same way (`graphrelay-update` without options), but
for a mail relay it is better to choose the time yourself.

### How updates stay correct (migrations)

Three kinds of things change between releases, and each has its own mechanism so that no step can be
forgotten:

| What changes | How it is applied | What makes sure it isn't forgotten |
|---|---|---|
| Binary, systemd units, helper scripts | Replaced on every update (step 7) | Nothing to forget: always complete |
| Database schema | The relay applies the missing entries of an ordered migration list on start and records the schema version in the database (`PRAGMA user_version`). A database newer than the binary is refused, so a downgrade can't corrupt it | Tests compare the schema with a recorded copy and upgrade a fixture of **every released schema version**: changing the schema without adding a migration fails the build |
| Things on the server an update doesn't overwrite: the firewall ruleset, keys in your `config.yaml` | Numbered host migration scripts in `migrations/`, each run once per server (step 6); applied ones are listed in `/opt/graphrelay/host-migrations.applied` | The build fails when one of the guarded files (`harden-host.sh`, `config.example.yaml`, the database schema) changes, until the developer records a decision: add a migration, or confirm none is needed |

Releases can be skipped: updating from any older release runs all the migrations in between, in order.

### Building it yourself

You only need this to change the code. With Go installed:

```bash
go test ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-s -w" -o graphrelay ./cmd/graphrelay
deploy/push.sh root@relay.example.com   # development: build, upload and run the same update steps
```

When a build fails because guarded files changed, read `deploy/migrations/README.md`: decide whether
existing servers need a host migration for your change, then run `bash deploy/migrations/guard.sh --update`.

To publish a release, push a version tag; the workflow in `.github/workflows/build.yml` tests, builds and
publishes it:

```bash
git tag v1.1.0
git push origin v1.1.0
```

The workflow runs on a self-hosted runner that has Docker, and uses no external actions. It starts
throwaway Debian containers that fetch the source, install the newest stable Go, verify the dependencies, run
the format check, `go vet`, all tests and the migration guard, and build the `amd64` and `arm64` archives.
It also builds the Docker image once and checks that it starts. Afterwards it removes the containers, the
images and its workspace.

## 11. Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `AADSTS7000215: Invalid client secret` | The Secret **ID** was used instead of the **Value**, or the secret expired |
| `AADSTS900021` / `AADSTS90002` | `GRAPH_TENANT_ID` is wrong; use the Directory (tenant) ID GUID |
| `AADSTS700016` application not found | `GRAPH_CLIENT_ID` is wrong or from another tenant |
| `New-ServicePrincipal`: `AADServicePrincipalNotFound` | Wrong Object ID: use the Enterprise application's (step 3b) |
| Test send: `403 ErrorAccessDenied` | Mailbox not tagged (3a), wrong scope, or the change hasn't propagated yet (up to 2 h). Check with `Test-ServicePrincipalAuthorization` |
| Test send: `404 ErrorInvalidUser` | The sender mailbox doesn't exist |
| Sign-in: `AADSTS50011` redirect URI mismatch | Fix the redirect URI (step 2.2); it must include `:8443` |
| Sign-in: `AADSTS50105` user not assigned | *Assignment required* is on and the user isn't assigned (step 4.2) |
| Portal: "... is not allowed to manage this relay" | No `Relay.Admin` role in the token: assign it, check nested groups, sign in again in a private window. The journal shows the roles found (`roles=[...]`) |
| Certificate: `timed out waiting for record to fully propagate` | The network redirects outbound DNS to an internal resolver that doesn't see the Cloudflare TXT record. Set `tls.skip_dns_propagation_check: true` |
| Log: `TLS certificate files changed but could not be loaded` | `files` mode: the new certificate and key don't match or are unreadable (wrong owner or mode, or a renewal still in progress). The previous certificate stays in use until a valid pair is in place |
| Certificate: Cloudflare `403` / `9109` | Token lacks `Zone:Read` or `DNS:Edit`, or isn't limited to the right zone |
| Device reports an SSL/TLS error although the CA is imported | The device may support only RSA cipher suites or only TLS 1.0/1.1. RSA-only devices need `tls.rsa_fallback: true` (the default); the relay requires TLS 1.2 or newer. `LOG_LEVEL=debug` shows handshake errors |
| Client: connection refused / timeout | No enabled host rule for its IP, so the firewall blocks it: `nft list set inet filter smtp4` and `journalctl -u graphrelay-fw.service` |
| Client gets `530 Authentication required` | The rule has an SMTP login; configure it on the device (section 8) |
| Client gets `554 ... Sender mailbox ... not allowed` | The rule's mailbox was removed from the allowed list |
| Client gets `451` | Temporary Graph error or throttling; the client retries by itself |
| Client gets `554` with a Graph error | Permanent refusal (e.g. size over the mailbox's `MaxSendSize`); details in **Log** |
