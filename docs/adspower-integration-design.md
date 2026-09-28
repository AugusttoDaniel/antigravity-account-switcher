# Design: ADS Power login automation + proxy-bound onboarding

Status: **APPROVED (with one open item: proxy pool source — see §5).**
Scope of this doc: automating Google account onboarding through ADS Power isolated
browser profiles, and binding each account to the profile's proxy end-to-end.
OmniRouter integration is intentionally **out of scope here** (deferred by request).

Locked decisions: automation = **assisted** (no credential storage); CDP driver =
**chromedp** (pure Go); onboarding = **one profile at a time first**, batch later;
proxy = **allocation policy in §3.5**.

---

## 1. Problem this solves

Before the account-switcher can rotate a pool of Google accounts safely, each account
must be *created / authorized without the accounts ever touching the operator's real
IP or browser fingerprint*. Today:

- The data-plane proxy per account already works (`Account.ProxyURL`).
- **[DONE, shipped in the leak fix]** OAuth code exchange, userinfo and background
  token refresh now egress through the account's proxy
  (`OAuthService.RefreshTokenVia` / `ExchangeCodeVia` / `FetchUserInfoVia`,
  `StartLoopbackFlowWithProxy`).
- **[STILL LEAKING]** The *interactive consent screen* opens in the operator's real
  default browser (`DefaultBrowserOpener`), so Google sees the real IP + real
  fingerprint at the exact moment of authorization. This is the remaining leak.

ADS Power gives each account an isolated Chromium profile with its own proxy and
antidetect fingerprint. If the consent screen loads *inside that profile*, Google only
ever sees the proxy IP + isolated fingerprint — closing the last leak.

## 2. Key insight that makes this clean

The RFC 8252 loopback flow already redirects to `http://127.0.0.1:<port>/oauth/callback`.
That redirect is resolved **by the browser on the local machine**, so even when the
ADS Power browser egresses all its Google traffic through a proxy, the final redirect
to `127.0.0.1` stays local and hits our existing callback listener. The switcher then
runs the code exchange through the *same* proxy (already implemented in the leak fix).

So the integration reduces to: **replace the `BrowserOpener` with one that opens the
auth URL inside an ADS Power profile instead of the OS default browser.** Everything
downstream (callback, PKCE/state validation, proxy-bound exchange, persistence) is
unchanged.

```
StartLoopbackFlowWithProxy(ctx, adsPowerOpener(profileID), logger, profileProxy)
                                   │                                   │
                                   │ opens auth URL in ADS Power       │ exchange + userinfo
                                   ▼ (proxy + fingerprint)             ▼ egress via same proxy
                        Google consent  ──redirect──►  127.0.0.1 loopback (local) ──► account saved
```

## 3. Proposed components (all pure Go, zero CGO)

### 3.1 `internal/adspower/` — Local API client
Wraps the ADS Power Local API (default `http://local.adspower.net:50325`, override via
config/env `ADSPOWER_API_URL`; optional API key).

Endpoints used:
- `GET  /api/v1/user/list`            — enumerate profiles (id, name, proxy config)
- `POST /api/v1/user/create`          — create a profile (proxy + fingerprint) [optional]
- `GET  /api/v1/browser/start?user_id`— launch profile → returns `ws.puppeteer` (CDP), `debug_port`
- `GET  /api/v1/browser/stop?user_id` — close profile
- `GET  /api/v1/browser/active?user_id`— status

Notes: Local API is rate-limited (~1 req/s) — client serializes calls with a small
throttle. Proxy for the account is read from the profile config so the switcher
*inherits* whatever proxy the profile already uses (override still possible).

### 3.2 CDP driver (opener)
A `BrowserOpener` implementation that:
1. calls `browser/start` for the profile,
2. connects to the returned CDP endpoint (`chromedp.NewRemoteAllocator`),
3. navigates the profile to the auth URL.

Dependency: `github.com/chromedp/chromedp` — **pure Go, no CGO** (satisfies the
zero-CGO invariant; it talks CDP over websocket to the already-running ADS Power
Chromium, it does not download or link Chrome).

### 3.3 Two automation modes (the pivotal decision — see §5)
- **(a) Assisted (recommended default):** switcher opens the isolated profile at the
  auth URL; the human types email / password / 2FA *inside that isolated window*.
  Switcher auto-captures the callback and binds the proxy. We never handle Google
  credentials. Robust against Google DOM/bot-detection changes.
- **(b) Full credential injection:** switcher types email + password via CDP and
  answers 2FA from a stored TOTP secret. Removes the human but is **fragile**
  (Google form changes, "verify it's you" interstitials, bot detection) and requires
  **storing Google passwords / TOTP secrets** — high blast radius if the DB leaks.

### 3.5 Proxy allocation policy (from the onboarding decision)
Per account being onboarded:
- **Existing account** (found by email in the pool and already carrying a `ProxyURL`):
  create/reuse an ADS Power profile bound to that **same** proxy. Never rotate a proxy
  that a working account already uses.
- **New account** (not in the pool, or in the pool with no proxy yet): allocate a
  **never-used** proxy from the pool, create the profile bound to it, and persist it as
  the account's `ProxyURL`.

"Never used" = a proxy from the pool that is **not currently the `ProxyURL` of any
account in the DB**. Allocation is transactional so two concurrent onboardings cannot
grab the same proxy. If the pool is exhausted (no unused proxy left), onboarding of a
new account fails loudly rather than silently reusing one.

Profile creation (`POST /api/v1/user/create`) is therefore **core**, not optional: the
switcher owns creating the ADS Power profile with the selected proxy + a fresh
fingerprint.

> Open item: **where the pool of candidate proxies comes from** is not yet decided —
> see §5. The allocation logic above is source-agnostic and sits behind a
> `ProxyPool` interface so the source can be swapped.

### 3.4 CLI + storage
- New command `add-account-adspower --profile <user_id> [--proxy <override>]`.
- Batch: `import-adspower --all` iterates every profile and onboards each.
- Storage: add nullable column `adspower_profile_id` to `accounts` so re-auth reuses
  the same profile; proxy auto-populated from the profile unless overridden.

## 4. What is already done vs. remaining

| Piece                                             | Status |
|---------------------------------------------------|--------|
| Proxy-bound token exchange / userinfo / refresh   | ✅ done (leak fix) |
| `StartLoopbackFlowWithProxy`                       | ✅ done (leak fix) |
| `internal/adspower/` Local API client             | ✅ done (`client.go`) |
| ADS Power CDP opener (attach to existing tab)      | ✅ done (`opener.go`) |
| `internal/proxypool/` static allocator            | ✅ done (`pool.go`) |
| `config.proxies` pool field                        | ✅ done |
| `adspower_profile_id` column + repo method (migration v3) | ✅ done |
| `add-account-adspower` CLI (one at a time)         | ✅ done (`cmd/.../adspower.go`) |
| `import-adspower --all` (batch)                    | ✅ done (reuses each profile's own proxy) |
| Mode (b) form/2FA automation                       | ⬜ out of scope (assisted chosen) |

### Implementation notes
- `chromedp` is pinned to **v0.11.2** on purpose: newer chromedp bumps the go.mod directive
  to `go 1.26`, which would break CI (pinned at Go 1.24). v0.11.2 keeps `go 1.24.0`.
- Proxy pool source is a **static list** in `config.json` under `proxies: [...]`.
- Allocation policy in the CLI (`decideProxy`): `--proxy` override > existing account's proxy
  (reuse) > a never-used proxy from the pool. New accounts also get a freshly-created ADS Power
  profile bound to that proxy; existing accounts reuse their `adspower_profile_id`.

### To validate on a machine with ADS Power (cannot be tested in CI)
1. ADS Power running with the Local API enabled (default `http://local.adspower.net:50325`).
2. Add a `proxies` list to the config.
3. `add-account-adspower --email you@gmail.com` → a profile is created with a fresh proxy, its
   browser opens at the Google consent page; complete sign-in there.
4. Confirm with `list-accounts` that the account shows the expected outbound proxy.
5. Verify (e.g. via the profile's IP-check page) that the consent + token traffic used the proxy,
   not the real IP.

## 5. Decisions

Resolved:
1. Automation mode → **assisted** ✅
2. `chromedp` dependency → **approved** ✅
3. Onboarding → **one at a time first** ✅
4. Proxy binding → **allocation policy in §3.5** ✅

**Still open — blocks the allocation layer only:**
- **Where does the pool of candidate proxies come from?** Options considered:
  - **Static list in config** (`proxies: [url, url, ...]`) — simplest; switcher marks
    each used once bound to an account.
  - **Provider API (e.g. Webshare)** — switcher requests a fresh proxy/session on demand
    from the provider's API; "never used" is guaranteed by the provider.
  - **Hybrid** — static list now, provider adapter behind the same `ProxyPool` interface
    later.
  The `internal/adspower/` client and the ADS Power `BrowserOpener` do **not** depend on
  this answer and are being built first.

## 6. Risks / caveats (must acknowledge)

- **Google ToS / automation:** automating Google sign-in (esp. mode b) may violate
  Google's terms and can trigger account challenges or bans. Isolated proxy+fingerprint
  reduces linkage but does not eliminate risk.
- **Credential storage (mode b only):** storing Google passwords/TOTP secrets is a
  serious liability. If pursued, secrets must be supplied at runtime and encrypted at
  rest, never committed, never logged.
- **Fragility (mode b):** Google's login DOM and anti-bot flows change frequently;
  form automation will need maintenance.
- **ADS Power dependency:** requires ADS Power running locally with the Local API
  enabled; version drift in its API is possible.
- **Rate limits:** Local API ~1 req/s — batch onboarding must throttle.
