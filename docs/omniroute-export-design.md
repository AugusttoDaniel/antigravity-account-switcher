# Design: export accounts to OmniRoute (OmniRouter)

Status: **IMPLEMENTED** — `export-omniroute` command (files + API modes, per-account proxy
binding). See `cmd/antigravity-account-switcher/omniroute.go` and `internal/omniroute/`.
Goal: export the Google/Antigravity accounts managed by this switcher into
[OmniRoute](https://github.com/diegosouzapw/OmniRoute) so it can route through them.

## What OmniRoute is

`diegosouzapw/OmniRoute` — a local, MIT-licensed AI gateway (one endpoint, 300+ providers,
quota-aware auto-fallback, RTK+Caveman token compression). It has first-class **Antigravity
runtime support** via the provider id **`agy`** (shares the Antigravity backend, incl. Claude
models). Runs locally, default `https://localhost:20128`.

Auth for its management API: a Bearer token from `POST /api/auth/login`, or `REQUIRE_API_KEY=false`
for local development.

## Verified import contract (read from OmniRoute source)

**Endpoints** (`skills/omni-providers/SKILL.md`, `src/app/api/providers/agy-auth/*`):

| Endpoint | Purpose | Body |
|---|---|---|
| `POST /api/providers/agy-auth/import` | one account | `{ source: {kind:"json", json:<tok>} \| {kind:"text", text:"<json>"}, name?, email?, overwriteExisting? }` |
| `POST /api/providers/agy-auth/import-bulk` | up to 50 | `{ entries: [{ json:<tok>, name?, email? }...], overwriteExisting? }` |
| `POST /api/providers/agy-auth/zip-extract` | extract `.json` from a ZIP for bulk | — |
| `POST /api/providers/agy-auth/apply-local` | auto-detect local agy login | — |

**Token object `<tok>`** (`parseAndValidateAgyToken`, `src/lib/oauth/utils/agyAuthImport.ts`):
- **Required:** `access_token`, `refresh_token` (may be flat or nested under `.token`).
- Optional expiry: `expiry` (ISO) or `expires_at` (ISO) or `expiry_date` (unix ms).
- Optional: `token_type` (default `Bearer`), `auth_method`.
- **NOT required:** `client_id` / `client_secret` / `project_id` — `projectId`, `email` and tier are
  enriched from the Antigravity Code Assist backend using the tokens (`enrichWithAntigravityBackend`).

**Per-connection proxy** (OmniRoute Proxy Guide) — assign after import, using the connection id the
import returns:
- `PUT /api/settings/proxy` body `{ level:"key", id:"<connection-uuid>", proxy:{ type:"http"|"socks5", host, port, username?, password? } }`
- 4-level hierarchy resolved in order: **account/connection → provider → combo → global**.
- (Alt: `PUT /api/v1/management/proxies/assignments` `{scope, scopeId?, proxyId?}`.)

## Mapping from our `accounts.db`

Per account we hold `Email`, `AccessToken`, `RefreshToken`, `TokenExpiry`, `ProxyURL`. That maps
directly onto the agy token object:

```json
{ "access_token": "<AccessToken>", "refresh_token": "<RefreshToken>", "token_type": "Bearer", "expiry": "<TokenExpiry RFC3339>" }
```

with `name` = `email` = the account email. We already have proxy-aware `EnsureValidToken`, so the
exporter refreshes each account through its own proxy first, guaranteeing a fresh `access_token`.

## Proposed command: `export-omniroute`

Two modes behind one command:

- **files** (`--out <dir>`, default): write one `<email>.json` per account in the agy token format.
  The user then pastes/uploads them, or zips them, into OmniRoute (`agy-auth/import`,
  `import-bulk`, or `zip-extract`). Zero network/TLS/auth dependency — the safe first increment.
- **api** (`--api`): POST directly to `agy-auth/import-bulk` (batched ≤50) at `--url`
  (default `https://localhost:20128`) with `--token` (or `$OMNIROUTE_TOKEN`); `--insecure` for the
  localhost self-signed cert. With `--assign-proxies`, then `PUT /api/settings/proxy` per returned
  connection to bind each account's `ProxyURL` in OmniRoute.

Flags: `--db`, `--out`, `--api`, `--url`, `--token`, `--assign-proxies`, `--overwrite`,
`--refresh` (refresh tokens before export; default on), `--filter <email substring>`.

## Security notes

- The export contains decrypted refresh/access tokens. That is acceptable because OmniRoute is the
  user's own **local** tool; tokens are only ever sent to the configured OmniRoute URL.
- files mode writes tokens to disk — the output dir and files must be owner-only (`0700`/`0600`).
- Never send tokens to any host other than the explicit `--url`.

## Open decisions (before building)

1. **Mode:** files, api, or both? (Recommend both; files works with zero OmniRoute config.)
2. **Auto-assign each account's proxy** in OmniRoute after import? (Recommend yes.)
3. **Refresh tokens before export** so `access_token` is fresh? (Recommend yes.)
