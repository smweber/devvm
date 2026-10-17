# Proposal: provider backends (devvm creates and destroys cloud VMs)

Status: draft v1, 2026-10-17, from a design discussion with the user. Not
scheduled. It **depends on `config.md`** (the config redesign) having landed
and the three hosts having migrated (`config.md` §8); `config.md` §9 records
the constraints that redesign keeps for this one. First provider: Hetzner
Cloud, because that is what the user runs. Once agreed, the order of work
goes in `ROADMAP.md`.

## Problem

The redesign makes a fresh box a three-word loop: `delete old`, `create new`,
`attach new`. That loop only works for smol. A cloud box today is: open the
provider's console, pick a type and image and key, wait for an address, then
`config add NAME --backend remote --managed --host IP`, then `setup NAME`,
and the reverse by hand to get rid of it. The box that is best reached from
anywhere, and the one that costs money while forgotten, is the one devvm
helps with least.

## Goals

- `create NAME --backend hetzner` and `delete NAME` do for a cloud VM what
  they do for a smol VM, including `start`/`stop`.
- The provider's API is the registry. devvm stores **nothing per VM**, as it
  stores nothing per smol VM after the redesign.
- Everything after allocation is the existing code: the VM is reached over
  ssh like a `remote` + `managed` entry, shaped by `setup`, forwarded by the
  same daemon, bridged by the same bridge.
- One provider, with a package boundary a second one can sit beside.

## Non-goals

- **Not Terraform.** No desired state, no plan/apply, no state file, no
  resource graph. One imperative verb pair over one resource kind. The
  precedent is `multipass launch`, not `terraform apply`; `docker-machine`
  is the cautionary tale (fifteen drivers, then unmaintained).
- **Not a cloud CLI.** No firewalls, volumes, floating IPs, snapshots,
  images, networks, or resize. Those are the provider's console or `hcloud`,
  the same line `resize` already sits on. devvm makes a server exist with a
  key on it and a label, and makes it not exist.
- **No change to `remote`.** A hand-written entry stays what it is, managed
  or adopted. A box the user made in the console is a `remote` entry.
- **No AWS-shaped abstraction up front** (§7).

## Design

### 1. What goes in the file

```toml
[providers.hetzner]              # one table per provider; credentials never here
token_env = "HCLOUD_TOKEN"       # default; the env var holding the API token
location = "nbg1"                # default placement
image = "ubuntu-24.04"           # image name or id; must boot sshd with root + key
ssh_key = "scott-laptop"         # name of a key already uploaded to the project
                                 # (or `ssh_key_file = "~/.ssh/id_ed25519.pub"`:
                                 # uploaded once by name if missing)
ipv4 = true                      # default; Hetzner bills a public v4 extra. false =
                                 # v6 only, for a laptop whose network routes v6

[templates.default]
server_type = "cx23"             # sizing for provider boxes; memory/disk stay smol's
```

- `[providers.*]` is a **third table family** beside `templates` and
  `machines`. `config check` validates it (unknown provider name is an
  error; unknown keys are errors, as elsewhere). `config add`/`rm` never
  touch it; it is written by hand with `config edit`.
- **The token is never in the file.** `token_env` names an environment
  variable. A missing variable is a one-line error naming it, only when a
  provider operation actually runs; `status` with no token lists no provider
  rows and says why once, it does not fail.
- `server_type`, `location`, `image` and `ipv4` are **template fields** too
  (template overrides provider table overrides built-in), so `[templates.big]
  server_type = "cx43"` works. `memory`/`disk` are ignored on a provider box
  and `config check` warns (the §3 rule from `config.md`).
- The shared file carries this across hosts. Two laptops with the same
  `HCLOUD_TOKEN` see the same VMs: that is the point.

### 2. Discovered, labelled, no entry

A provider VM has **no `[machines.*]` entry**. An entry would exist before
the VM and outlive it, which is the dormant state the redesign removed. It is
discovered from the API, exactly as smol VMs are discovered from `smolvm
machine ls`:

- `status` lists every server in the project. Servers carrying the label
  `devvm=true` are **managed**; the rest are **unmanaged**: listed, reachable
  for `shell`/`exec`, hands-off, and `delete` refuses on them and points at
  the console. The label *is* the managed marker: it lives on the provider,
  so it is shared across hosts and needs no state file, and it cannot go
  stale the way the smol marker can.
- **devvm never promotes a provider server.** An unmanaged server is treated
  like an unmanaged `remote` entry, not like a discovered smol VM: `setup`
  **refuses** on it, with no flag that overrides. The only way a server
  becomes managed is the user adding the `devvm=true` label themselves in
  the console or with `hcloud server add-label`, which is the provider-side
  equivalent of writing `managed = true` in a `remote` entry: config written
  by the user, never by a VM command. The user's project holds production
  servers that would be broken by `setup`, and a prompt is not enough of a
  gate for that; the label is a deliberate act in another tool. Nothing in
  the `Provider` interface sets labels after creation (§7).
- A second label, `devvm-template=NAME`, is informational only. Nothing reads
  it; it answers "what made this" in the console.
- Name resolution follows `config.md` §2 as an ordered source list: `HUB/NAME`
  → `[machines.NAME]` → local smol → **each configured provider**. A name
  not found anywhere is "no such machine". The provider lookup is one `GET
  /servers?name=NAME`, run only when the earlier sources miss, so `attach
  web` on a laptop with a local `web` never dials the API. Collisions follow
  the same rule: an entry shadows a provider VM (`shadowed`), and `create NAME
  --backend hetzner` refuses while `[machines.NAME]` or a smol `NAME` exists.
- The name is the server name. Hetzner requires a unique RFC 1123 host name
  per project, which is stricter than smol; `create` validates before calling.

### 3. Lifecycle

```
devvm create NAME --backend hetzner [--template T] [--server-type S] [--location L] [--image I]
devvm delete NAME            # confirm, as lockdown does; --yes to skip
devvm start NAME / stop NAME # power on / power off
```

`create`:

1. Resolve the template; refuse on a name collision (§2).
2. Ensure the ssh key exists in the project (`ssh_key` by name, or upload
   `ssh_key_file` once).
3. `POST /servers` with name, type, image, location, the key, labels
   `devvm=true` and `devvm-template=T`, `public_net` per `ipv4`. No
   `user_data`: cloud-init is a second shaping system, and `setup` already
   does everything it would.
4. Print the address, then wait for ssh (the bounded wait `config.md` §9
   puts in `setup`), then run `setup NAME` unchanged: it finds root-only
   login, creates the template's user with sudo and root's key, installs
   prereqs and the agent, runs the hook, clones repos, adds keys, hardens,
   seeds this host's saved forwards.

If step 3 succeeds and `setup` fails, the server **stays** (it costs money,
but deleting a half-shaped box the user may want to inspect is worse); the
error ends with `run: devvm setup NAME` or `devvm delete NAME`. `create`
is not idempotent: a second `create NAME` finds the server and refuses.

`delete` on a managed provider box: confirm, `DELETE /servers/{id}`, then
the one host-side cleanup helper from `config.md` §9: stop the daemon,
remove `state/ports/NAME` and the pinned `known_hosts` line for the address.
Hetzner reuses addresses, so a stale pin would fail the next `create`.

`start`/`stop` are `POST /servers/{id}/actions/poweron|poweroff`, waited to
completion. `stop` keeps the forwards' daemon as smol does (pending,
reconnecting). A stopped Hetzner server **still bills**, and the smol
intuition that stopped is free is wrong here, so `stop` **warns** every time
(`web is stopped but still billed; devvm delete web frees it`) and `status
-v` repeats it on the row (`stopped (billing)`). The warning is one line on
stderr, not a confirm: `stop` does what was asked.

There is no `provision`/`deprovision`, no dormant state and no resize, as
decided in the redesign.

### 4. Reach and setup

Once it exists, a provider VM is a `remote` + `managed` box in every way
that matters, and shares `backend/ssh.go`:

- The record's host is the server's public IPv4 (or v6 when `ipv4 = false`),
  **read from the API row at resolve time**, never stored. The user is the
  template's `user`, the same rule as a managed `remote` entry. Port 22.
- `transport`, `mosh_server`, `identity`, `connect_timeout` come from the
  template (they are per-entry keys today; they gain template fallback so a
  provider box can set `transport = "mosh"` without an entry).
- The host key is trust-on-first-use into devvm's pinned `known_hosts`, as
  for every managed box. Hetzner doesn't expose host keys, and the cost of
  TOFU on a fresh server is the same as in the console flow today.
- `Managed()` is true from the label, so the agent goes to `/usr/local/bin`,
  `harden` runs, `setup` may try root. `IsRemote()` (the "reached over ssh"
  predicate, a method per `config.md` §9) is true. **Nothing in `session`,
  `bootstrap`, `agentbin`, the bridge, hubs or the Swift app changes.**
- If the hook joins a tailnet and the user wants that path instead of the
  public address, that's a `remote` entry pointing at the MagicDNS name,
  made with `config add`; the provider VM is then `shadowed` by it, which is
  exactly right. Optional later: a `host_template = "{name}.tail.ts.net"`
  provider key. Not in v1.

### 5. Status, cache and `--plain`

- `status` adds one API call per configured provider, alongside the per-hub
  fan-out. It reuses the hub listing's shape: a deadline, a per-source cache
  (`cache/provider-hetzner.list`) returned stale on failure with the rows
  marked, so a menu bar `status --watch` loop with no network shows the last
  known servers rather than nothing. `status --local` skips providers as it
  skips hubs.
- `--plain` rows: backend column `hetzner`; state ∈ {running, stopped,
  starting, stopping, ?} mapped from Hetzner's `status` (`initializing`,
  `starting` → starting; `off` → stopped; `deleting`, `rebuilding`,
  `migrating`, `unknown` → `?`). The fifth column is `managed`/`unmanaged`
  from the label. No new tokens, so the Swift app needs no change; an
  `unreachable` provider shows its cached rows as hubs do.
- `status -v` shows server type, location and address from the row.

### 6. Hubs

Orthogonal. A hub lists its own provider rows if it has the token; the laptop
hides a hub's `hetzner` rows whose names are also in its own listing (same
rule as for a hub's `remote` entries). `HUB/NAME` never addresses a provider
VM: there is no exec to own, so nothing to relay. A provider VM can itself
be registered as a hub by `config add` once it exists, which is what the
current `cloud` box is.

### 7. Other providers, honestly

The provider interface is written from Hetzner alone:

```go
type Provider interface {
    List(ctx) ([]Server, error)                  // every server; Managed from the label
    Get(ctx, name) (*Server, error)              // nil when absent
    Create(ctx, Spec) (*Server, error)           // name, type, image, location, key, labels
    Delete(ctx, id) error
    Power(ctx, id, on bool) error
}
```

`internal/provider/hetzner` implements it over plain REST (`net/http` +
`encoding/json`; the five endpoints are small and the SDK would be the
module's largest dependency). A fake implements it for tests, as the fake
smolvm does. `Server` carries name, id, state, address, type, location,
labels. There is deliberately no method that changes labels on an existing
server (§2): `Create` sets them at birth and nothing else writes them.

DigitalOcean maps onto this one to one (droplets, tags, named keys). AWS
does not: it needs a VPC, a security group, an AMI lookup and a key pair
before anything has a name, and the natural identity is an instance id, not
a name. So the interface is **not** stretched for AWS now; a second provider
is added only when the user runs one, and it is allowed to change the
interface then. The one structural concession today is the package
boundary and that `backend` values for providers are the provider name, so
`--backend digitalocean` would be the same shape.

### 8. Safety and cost

- `delete` confirms unless `--yes`, with the server type and address in the
  prompt. There is no bulk delete.
- `create` prints the type, location and that it bills, before calling.
- `status` is the forgotten-VM guard: every server in the project is listed,
  managed or not, so a box left running is visible in the menu bar.
- Unmanaged servers are untouchable: `setup`, `delete`, `keys`, `repos`,
  `cp-in`, `auth --install-agent` and the bridge all refuse, and no flag
  promotes one (§2). Only `status`, `start`, `stop`, `shell` and `exec`
  work, as for an unmanaged `remote` entry, and `start`/`stop` confirm on
  an unmanaged server since they can take a production box down.
- API errors are surfaced verbatim with the HTTP status; rate limits are not
  retried (the loop that could hit them is `status --watch`, which is why it
  caches).
- The token is read from the environment only; it never appears in argv,
  logs or the daemon's spawn (the daemon never calls the API: it gets the
  address in its `Machine` as it does for a `remote` box).

## CLI surface after

- **Lifecycle:** `create NAME --backend hetzner [--template T] [--server-type]
  [--location] [--image]`, `delete`, `start`, `stop`, `setup` (re-run on a
  managed box only; never promotion), all unchanged in name.
- **Everything else** unchanged.
- **Config:** `[providers.hetzner]` by hand; `config check` and `config show`
  cover it.

A fresh cloud box is `create web --backend hetzner`, `attach web`, and later
`delete web`.

## Implementation order

1. **Provider interface and the Hetzner client** (`internal/provider`,
   `internal/provider/hetzner`) with a fake and tests against recorded
   responses. No CLI.
2. **Discovery and reach**: the provider source in resolve and in `status`
   (with the cache), the `hetzner` backend value built on `sshBackend`,
   `--plain` mapping, `config check` for `[providers.*]`.
3. **Lifecycle**: `create --backend hetzner` through `setup`, `delete` with
   cleanup, `start`/`stop` with the billing warning and the unmanaged
   confirm.
4. **Live test** against the user's project: create, attach, auth, ports,
   bridge, stop/start, delete, then the same from a second host.

## Open questions

None open. Answered by the user (2026-10-17), recorded so they aren't
reopened:

- **Stopped-but-billing:** `stop` warns every time, one line on stderr (§3).
  "I'll totally forget."
- **`ipv4`:** default `true` (§1).
- **Promotion:** devvm never promotes a provider server; the user sets the
  label in the provider's tools (§2). The reason is that the project holds
  production servers an accidental `setup` would break. The same discussion
  tightened promotion of discovered smol VMs in `config.md` §3 (`--adopt`
  plus a typed name, no `--yes`).

## Dependencies on `config.md`

Assumed landed: discovery as the only path for VMs (§2), ordered sources and
per-source `Managed` (§9), templates with per-backend field applicability
(§3), `setup` with root adoption and the bounded ssh wait (§3, §9), the
single host-side cleanup helper in `delete` (§9), `config check` rejecting
unknown tables (§9), the `(cache)` shape of hub listings (hub.md §5, kept by
the redesign).
