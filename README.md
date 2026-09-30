# Antigravity Account Switcher

[English](README.md) | [Português (Brasil)](README.pt-BR.md)

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.24%2B-00ADD8?logo=go)](https://go.dev/)
[![CI Status](https://github.com/AugusttoDaniel/antigravity-account-switcher/actions/workflows/ci.yml/badge.svg)](https://github.com/AugusttoDaniel/antigravity-account-switcher/actions)
[![CodeRabbit Pull Request Reviews](https://img.shields.io/coderabbit/prs/github/AugusttoDaniel/antigravity-account-switcher?utm_source=oss&utm_medium=github&utm_campaign=AugusttoDaniel%2Fantigravity-account-switcher&labelColor=171717&color=FF570A&link=https%3A%2F%2Fcoderabbit.ai&label=CodeRabbit+Reviews)](https://coderabbit.ai)

Automatic Multi-Account Pool Management, Real-Time Quota Tracking, and Seamless HTTP 429 Failover for **Google Antigravity 2.0** and CLI (`agy`).

> [!WARNING]
> **Disclaimer — interoperability & educational use only.**
> This is an independent, unofficial project. It is **not affiliated with, authorized, or endorsed by** Google, ADS Power, AliasMode, OmniRoute, or any other third party; all trademarks belong to their respective owners.
> The tool interoperates with third-party services that each have their own Terms of Service. Automating sign-in, pooling/rotating multiple accounts to work around rate limits, and using antidetect browser profiles **may violate those Terms** and can result in account suspension or other consequences. **You are solely responsible** for ensuring your use complies with all applicable terms, laws, and regulations. The software is provided "AS IS", without warranty and with no liability to the authors — see [LICENSE](LICENSE).

---

## What is Antigravity 2.0 and why is this needed?

**Google Antigravity 2.0** is Google DeepMind's standalone AI-first development application (distinct from the legacy preview VS Code extension).

When working intensely with Antigravity 2.0, developers frequently hit rate limits (`HTTP 429 RESOURCE_EXHAUSTED` or rolling 5-hour limits) across Claude, GPT, and Gemini model tiers.

**Antigravity Account Switcher** runs as a transparent, high-performance local supervisor that intercepts Antigravity 2.0 requests. When your active account exhausts its quota, the switcher **seamlessly rotates to the next available account in your pool and replays the in-flight request in memory** — without interrupting your agent's thinking, breaking generation streams, or causing errors in the editor.

---

## Installing Google Antigravity 2.0 on Linux

> [!IMPORTANT]
> **Google Antigravity 2.0 is NOT distributed via package managers (`apt`, `snap`, `dnf`, `pacman`, or `flatpak`).**  
> Google provides it directly as a standalone `.tar.gz` archive containing the bundled binary and runtime libraries.

On Linux, software distributed this way should be placed in either the **XDG User Directory** (recommended, no root required) or the **System `/opt` Directory** (requires root).

### Option 1: User Installation (Recommended — No `sudo` required)

This follows the **Linux XDG Base Directory Specification** (`~/.local/share/` and `~/.local/bin/`). It does not require administrator privileges and will not affect other users or system files:

```bash
# 1. Create the application directory
mkdir -p ~/.local/share/antigravity

# 2. Extract the downloaded Google archive
tar -xzf ~/Downloads/Antigravity-linux-x64.tar.gz -C ~/.local/share/antigravity --strip-components=1

# 3. Ensure the binary has execution permissions
chmod +x ~/.local/share/antigravity/antigravity

# 4. Create a symlink in ~/.local/bin (in your PATH on modern Linux distros)
mkdir -p ~/.local/bin
ln -sf ~/.local/share/antigravity/antigravity ~/.local/bin/antigravity
```

### Option 2: System-Wide Installation (Requires `sudo` / FHS Standard)

This follows the **Filesystem Hierarchy Standard (FHS)** for add-on third-party software in `/opt`:

```bash
# 1. Create system directory
sudo mkdir -p /opt/antigravity

# 2. Extract the archive
sudo tar -xzf ~/Downloads/Antigravity-linux-x64.tar.gz -C /opt/antigravity --strip-components=1

# 3. Create global symlink
sudo ln -sf /opt/antigravity/antigravity /usr/local/bin/antigravity
```

---

## How the Switcher Connects to Antigravity 2.0

### Automatic Detection (Zero Configuration)
If you followed either of the standard installation methods above, **you do not need to configure any paths**.  
When you run `antigravity-account-switcher launch`, the switcher automatically discovers your binary by checking standard Linux locations:
1. `~/.local/bin/antigravity`
2. `~/.local/share/antigravity/antigravity`
3. `/usr/local/bin/antigravity`
4. `/opt/antigravity/antigravity`
5. `~/tools/Antigravity/Antigravity-x64/antigravity` *(user tools folder fallback)*
6. Any `antigravity` or `agy` command available in your system `$PATH`

### Custom Path Configuration
If you unpacked Antigravity 2.0 into a custom folder, you can explicitly configure the binary location using any of the following methods:

**Method 1: Permanent setting via CLI (Recommended)**
```bash
antigravity-account-switcher config set antigravity_bin /path/to/your/antigravity
```

**Method 2: Environment Variable**
```bash
export ANTIGRAVITY_BIN="/path/to/your/antigravity"
```

**Method 3: Per-launch flag**
```bash
antigravity-account-switcher launch --bin /path/to/your/antigravity
```

---

## Quick Start (3 Steps)

### 1. Build and Install the Switcher
```bash
git clone https://github.com/AugusttoDaniel/antigravity-account-switcher.git
cd antigravity-account-switcher
make install
```
*Compiles a single static binary with CGO_ENABLED=0 and installs it to `~/.local/bin/antigravity-account-switcher`.*

### 2. Onboard Your Accounts
Authenticate one or more Google accounts:
```bash
antigravity-account-switcher add-account
```
*(If you already have Antigravity 2.0 installed and logged in, the switcher automatically detects and imports your active login on first launch!)*

### 3. Launch Antigravity 2.0
```bash
antigravity-account-switcher launch
```
The switcher starts the background proxy, launches Antigravity 2.0 with scoped proxy variables, and monitors health. When you close Antigravity 2.0, the switcher shuts down automatically.

---

## Desktop Integration (GNOME / KDE / XFCE)

To launch Antigravity 2.0 directly from your application launcher or dock with multi-account supervision:

```bash
antigravity-account-switcher install-desktop
```
This automatically:
- Resolves your Antigravity 2.0 executable.
- Extracts and installs the official application icon to `~/.local/share/icons/antigravity.png`.
- Creates `~/.local/share/applications/antigravity.desktop` pointing to `antigravity-account-switcher launch %F`.

To remove the desktop integration:
```bash
antigravity-account-switcher uninstall-desktop
```

---

## Commands Reference

The CLI provides commands for launch supervision, manual switching, and configuration:

| Command | Description |
| :--- | :--- |
| `launch` | **(Recommended)** Launches Antigravity 2.0 under proxy supervision. |
| `serve` | Runs the proxy, background quota monitor, and web dashboard as a daemon. |
| `wrap -- <cmd>` | Executes any arbitrary command with scoped switcher proxy variables. |
| `add-account` | Initiates RFC 8252 loopback OAuth2 flow to register a Google account. |
| `add-account-adspower` | Onboards an account through an isolated ADS Power profile + proxy (assisted login). |
| `import-adspower` | Batch-onboards every ADS Power profile as an account, reusing each profile's proxy. |
| `export-omniroute` | Exports accounts to OmniRoute (agy token files and/or its API), binding each account's proxy. |
| `set-account-proxy` | Assigns or updates the outbound proxy URL for a specific account. |
| `list-accounts` | Displays all registered accounts, active status, and quota percentages. |
| `refresh-quotas` | Forces an immediate live quota sync from Google for all registered accounts. |
| `status` | Shows the currently active account, token totals, and switcher health. |
| `config` | Inspects or updates persistent settings (`get`, `set`, `list`). |
| `install-desktop` | Installs GNOME / XDG `.desktop` launcher shortcut and application icon. |
| `uninstall-desktop`| Removes GNOME / XDG `.desktop` launcher shortcut. |
| `version` | Displays binary version, commit hash, and build timestamp. |

### Useful Command Flags

- **Open Web Dashboard on Launch:**
  ```bash
  antigravity-account-switcher launch --open
  ```
- **Headless / Remote SSH Account Addition:**
  ```bash
  antigravity-account-switcher add-account --no-browser
  ```
- **Add an Account Without Leaking Your IP:** the dashboard's **Authenticate New Google Account**
  button asks for the account's proxy first (from the pool, or typed), sends the token exchange
  through it and saves it on the account. Then it either:
  - **opens an isolated AliasMode / ADS Power profile** (one click): the profile is created bound to
    that proxy, its browser opens Google, and you sign in inside that window. The dashboard finds
    AliasMode on its default port (`http://127.0.0.1:50400`) or ADS Power's; to point it elsewhere use
    `config set adspower_api_url <url>` (it must be on this machine, since the proxy credentials are
    sent to it), plus `adspower_api_key` and `adspower_engine` (default `cloak`, Chromium) if needed; or
  - **shows the sign-in link** instead of opening your default browser (that browser reaches Google
    from your real IP), for you to open in a browser profile that uses the same proxy.

  From the terminal, `add-account --proxy <url>` does the same for the token exchange, and
  `add-account-adspower` runs the profile flow.
- **Specify Custom Port:**
  ```bash
  antigravity-account-switcher launch --port 1831
  ```
- **Multi-Tier Model Fallback on Launch:**
  ```bash
  antigravity-account-switcher launch --fallback-secondary --model-primary gemini-2.5-pro --model-secondary claude-3-7-sonnet
  ```

### Isolated Onboarding via ADS Power

To keep each account's sign-in and traffic off your real IP and browser fingerprint, onboard
through [ADS Power](https://www.adspower.com/) antidetect profiles. Every OAuth call — consent,
code exchange, userinfo and background token refresh — egresses through the account's proxy, so
Google never sees your real connection.

Requirements: ADS Power running locally with its Local API enabled (default
`http://local.adspower.net:50325`).

1. Define a static pool of proxies for new accounts in the config file:
   ```json
   { "proxies": ["http://user:pass@host-a:8080", "socks5://user:pass@host-b:1080"] }
   ```
2. Onboard one account (creates an isolated profile bound to a never-used proxy, then opens its
   browser at the Google consent page for you to sign in):
   ```bash
   antigravity-account-switcher add-account-adspower --email you@gmail.com
   ```
   - An existing account reuses its bound proxy/profile; a new one draws a never-used proxy from
     the pool. Use `--proxy <url>` to force a specific proxy.
3. Or batch-onboard every existing ADS Power profile, reusing each profile's own proxy:
   ```bash
   antigravity-account-switcher import-adspower --all
   ```

> Login is **assisted**: the switcher opens the isolated profile at the consent screen and you
> complete the Google sign-in (including 2FA) inside that window. Credentials are never stored by
> the switcher.

### Exporting accounts to OmniRoute

The `export-omniroute` command sends the accounts managed here into
[OmniRoute](https://github.com/diegosouzapw/OmniRoute) as `agy` connections, refreshing each
account through its own proxy first. It can write per-account token files for manual import:

```bash
antigravity-account-switcher export-omniroute --out ./omniroute-tokens
```

It can also push directly to a local OmniRoute instance and bind each account's proxy inside it.
Run `antigravity-account-switcher export-omniroute --help` for the API-mode flags.

Accounts OmniRoute already has are skipped, never duplicated: one already imported (`agy`) is left
as is unless you pass `--overwrite` to refresh its tokens there, and one connected through
OmniRoute's own Antigravity login is skipped unless you pass `--allow-duplicate`, since importing
it would make OmniRoute use the same Google account twice.

The account's proxy follows it into OmniRoute (`--assign-proxies`, on by default), **including for
accounts OmniRoute already has**: the proxy is registered there and bound to the account's
connection, then OmniRoute is asked which proxy the connection now resolves to, and the command
fails loudly if that is not the one bound. It only writes when the two differ, so running it again
changes nothing, and it never removes a proxy: an account without one is left alone. The dashboard's
**OmniRoute Sync** section shows the same comparison and has a **Bind in OmniRoute** button (with a
confirmation click) for accounts whose proxy differs or is missing there.
- **Web Dashboard Privacy Mode:**
  Click the **Privacy** button in the dashboard header or press <kbd>P</kbd> to blur and redact all Google account email addresses across cards, active routing, and live proxy event logs for safe screenshots and screen-sharing.

---

### OpenAI Codex accounts

The same isolation applies to ChatGPT/Codex logins. Codex accounts live in their own table and
never enter the Antigravity routing pool, quota poller or OmniRoute export.

```bash
# Sign in through a dedicated proxy, inside an isolated AliasMode/ADS Power profile (recommended)
antigravity-account-switcher codex-add --adspower --proxy "http://user:pass@host:port"
# or print the sign-in URL and open it yourself in a browser that already uses that proxy
antigravity-account-switcher codex-add --proxy "http://user:pass@host:port"

antigravity-account-switcher codex-import          # adopt the Codex CLI's current ~/.codex/auth.json
antigravity-account-switcher codex-list
antigravity-account-switcher codex-switch me@example.com   # writes ~/.codex/auth.json (honors $CODEX_HOME)
antigravity-account-switcher codex-refresh --all           # renews tokens through each account's proxy
antigravity-account-switcher codex-set-proxy me@example.com "http://user:pass@host:port"
```

- `codex-add` refuses to run without `--proxy` (pass `--allow-direct` to accept your real IP).
  By default it opens no browser: the default browser would reach OpenAI from your real IP.
  The OAuth callback uses port `1455`, the only redirect the Codex CLI's public client has
  registered, so close any running `codex login` first.
- `codex-switch` makes no network call. Before overwriting `auth.json` it saves any token
  rotation the Codex CLI made to the outgoing account; without that, switching back would use a
  refresh token the issuer already retired. Close running Codex sessions before switching.
- `codex-refresh` goes through the account's own proxy and fails closed if it has none or the
  proxy is unusable. An `invalid_grant` marks the account `error`: sign in again with `codex-add`.
- An email shared by a personal and a workspace login is ambiguous: pass the account id.
- **Limits:** `codex-usage [--all] [--cached] [account]` shows each account's 5-hour and weekly
  windows, reset times, credits and whether the limit was reached. It reads the same endpoint the
  Codex CLI uses (`/backend-api/wham/usage`) through the account's own proxy and fails closed without
  one; an expired access token is renewed once and the read retried. `--cached` shows the last
  stored snapshot with no network. The dashboard shows the same as bars in a **Limits** column, with
  **Usage** per account and **Refresh usage** for all (read one at a time, never in a burst). Nothing
  polls on a timer: limits are read only when you ask.
- **Hand-over to OmniRoute:** `codex-export-omniroute` imports Codex accounts into OmniRoute
  (`/api/providers/codex-auth/import-bulk`) and binds each account's proxy to its connection. Without
  `--yes` it only prints the plan. **A Codex refresh token is single-use**, so the tokens can live in
  only one place: once OmniRoute has them it renews them, and this switcher marks the account
  *in OmniRoute* and refuses to renew, read limits for, or switch the Codex CLI to it (`--force`
  overrides, at the cost of breaking OmniRoute's session for that account). Nothing is refreshed
  before the export, since that would rotate the token for nothing. An account OmniRoute already holds
  through its own login is left alone (no tokens sent, no marker), an account that needs a new sign-in
  is skipped, and a fresh sign-in with `codex-add` takes the account back (it is a new token family).
  `--overwrite` replaces a connection OmniRoute already has, but never for an account already handed
  over: what this switcher holds is stale by then.
- **Dashboard:** the **OpenAI Codex Accounts** panel lists the accounts (proxies masked, tokens never
  sent to the page) and offers Use, Refresh, Proxy, Remove and **Add Codex account**, through the
  same proxy pool and AliasMode profile as Google accounts.
- **Callback port blocked (common on Windows):** Windows can reserve `1374-1473` for Hyper-V, WSL or
  Docker (`netsh int ipv4 show excludedportrange protocol=tcp`), which contains `1455`, so no program
  can listen there. The switcher detects it and switches to a manual step: after you sign in, the
  browser lands on a page that fails to load; copy its full address and paste it into the dashboard
  (or into the terminal, for `codex-add`). The address carries a one-time code, so do not share it.
- Protocol constants (issuer, public client id, scopes, `auth.json` layout) come from the
  Apache-2.0 [openai/codex](https://github.com/openai/codex) repository. Using several accounts
  is subject to OpenAI's terms; you are responsible for complying with them.

---


## Multi-Tier Model Fallback & Self-Healing

The switcher includes an intelligent multi-tier contingency system to keep you coding uninterrupted:

### 1. Intra-Account Model Fallback
When your active account runs out of quota on a heavy model tier (e.g. `gemini-2.5-pro` or `claude-3-7-sonnet`), the switcher can automatically fall back to a lighter secondary model (such as `gemini-2.5-flash` or `claude-3-5-sonnet`) on the **same account** before rotating to the next account in the pool.
- Supports both **cross-family** fallback (`claude-3-7-sonnet` -> `gemini-2.5-flash`) and **same-family** fallback (`gemini-2.5-pro` -> `gemini-2.5-flash`, which possess separate quota buckets in Google Cloud Code PA).
- Includes **predictive bypass**: if quota telemetry indicates 0% remaining on the primary model, requests are proactively routed to the secondary tier without incurring an expensive HTTP 429 round-trip.
- When background polling detects that primary quota has reset, traffic automatically reverts to your preferred primary model.

### 2. Dashboard Web UI Configuration
You can configure model fallback directly from the browser dashboard (`http://127.0.0.1:8080` or `launch --open`):
- **Live Toggle**: Enable or disable intra-account secondary fallback on the fly.
- **Model Discovery**: Click **Fetch Models** to query active `language_server` / Cloud Code PA endpoints and populate model selectors with currently supported models.
- **Instant Hot-Reload**: Clicking **Save Settings** persists changes to `config.json` and immediately applies them to the running proxy engine without restarting Antigravity 2.0 or the daemon.

### 3. Automatic 400 Thought Signature Recovery
When switching between model providers (e.g. Claude <-> Gemini) or continuing multi-turn agent sessions, Google Cloud Code PA validates cryptographic HMAC thought signatures (`thought_signature` or Claude `thinking` blocks). Incompatible or stale thought blocks trigger `HTTP 400 ("Corrupted thought signature" or "Invalid signature in thinking block")`.

**The switcher automatically recovers from this condition in-flight:**
- The proxy intercepts the HTTP 400 response before it reaches the client.
- It applies payload sanitization via structural visitors (`skip_thought_signature_validator` injection or HMAC thought block pruning) while preserving complete user chat history.
- The request is instantly replayed in memory to Google Cloud Code PA, ensuring agent thinking continues seamlessly without crashing the editor or losing conversation context.

### 4. Per-Account Custom Outbound Proxy (Webshare / Residential Support)
To prevent IP-based rate limits across multiple Google accounts, each account in your pool can be assigned its own dedicated HTTP/HTTPS outbound proxy (e.g., `http://usr123:pass@p.webshare.io:80`):
- **Account IP Isolation**: When requests are dispatched for a specific account, the switcher routes outbound Google Cloud Code PA traffic through that account's configured proxy.
- **Easy Configuration**: Edit or clear the outbound proxy URL directly from the Web Dashboard card for any account by clicking **Edit Proxy**.
- **Secure Password Masking**: Outbound proxy credentials are safely masked in the Web UI (and hidden in Privacy Mode).

---

## Configuration Reference

Settings are stored in JSON format at `~/.config/antigravity-account-switcher/config.json`.

```bash
# View all current settings
antigravity-account-switcher config list

# Set path to Antigravity 2.0 executable
antigravity-account-switcher config set antigravity_bin ~/.local/share/antigravity/antigravity

# Set default web dashboard port
antigravity-account-switcher config set port 1831

# Adjust background quota check interval
antigravity-account-switcher config set quota_interval 60s

# Configure preferred primary and secondary contingency models
antigravity-account-switcher config set model_primary gemini-2.5-pro
antigravity-account-switcher config set model_secondary claude-3-7-sonnet

# Enable intra-account model fallback before rotating accounts
antigravity-account-switcher config set fallback_secondary_enabled true
```

### Environment Variables

| Variable | Description |
| :--- | :--- |
| `ANTIGRAVITY_BIN` | Explicit path to the Antigravity 2.0 executable. |
| `ANTIGRAVITY_PORT` | Overrides the switcher listening port. |
| `ANTIGRAVITY_DB_PATH` | Path to the SQLite database file (default: `~/.config/.../accounts.db`). |
| `ANTIGRAVITY_CLIENT_ID` | Optional custom Google Cloud Console OAuth Client ID override. |
| `ANTIGRAVITY_CLIENT_SECRET` | Optional custom Google Cloud Console OAuth Client Secret override. |
| `ANTIGRAVITY_MODEL_PRIMARY` | Overrides default primary model tier. |
| `ANTIGRAVITY_MODEL_SECONDARY` | Overrides default secondary fallback model tier. |
| `ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED` | Enables/disables intra-account secondary fallback (`true`/`false`). |

---

## Architecture

```text
               +-----------------------------------+
               |          Antigravity 2.0          |
               |     (Child Process via Supervisor)|
               +-----------------+-----------------+
                                 | HTTP_PROXY / CLOUD_CODE_URL
                                 v
+------------------------------------------------------------------+
|              ANTIGRAVITY ACCOUNT SWITCHER (Single Binary)        |
|                                                                  |
|   +--------------------+     +-------------------------------+   |
|   |   Web Dashboard    |     |      In-Process Reverse Proxy |   |
|   |  (HTML5/Tailwind)  |     | * Dynamic Bearer Token        |   |
|   |  http://127.0.0.1  |     | * 100MB Replay Buffer         |   |
|   +---------+----------+     | * RFC 7231 CONNECT Tunnel     |   |
|             |                +---------------+---------------+   |
|             v                                |                   |
|   +--------------------+         HTTP 429    | SSE Tokens        |
|   | SQLite WAL Store   |<--------------------+                   |
|   | * accounts.db      |                     v                   |
|   | * token telemetry  |     +-------------------------------+   |
|   +---------+----------+     | Quota Poller Daemon           |   |
|             ^                | * Auto-restore past reset     |   |
|             +----------------+ * Official Google PA User-Agent|   |
+----------------------------------------------+-------------------+
                                               |
                                               v
                              +--------------------------------+
                              | Google Cloud Code Infrastructure|
                              +--------------------------------+
```

---

## Troubleshooting & FAQ

#### 1. "Could not automatically locate Antigravity binary"
If your Antigravity 2.0 installation is in a custom location, specify the path to the executable:
```bash
antigravity-account-switcher config set antigravity_bin /path/to/antigravity
```

#### 2. Does this interfere with native voice dictation (Speech-to-Text)?
No. Voice traffic (`speech.googleapis.com`) is carried through a raw RFC 7231 TCP tunnel on `CONNECT` requests, byte-for-byte, so audio streaming is not inspected or altered. Like all non-loopback traffic, the tunnel leaves through the active account's proxy, so dictation never reveals your real IP.

#### 3. Where are my tokens and credentials stored?
Tokens are stored strictly on your local filesystem in SQLite (`~/.config/antigravity-account-switcher/accounts.db`). No credentials or telemetry ever leave your machine.

#### 4. How do I completely uninstall and remove all data?
```bash
# 1. Remove desktop entry
antigravity-account-switcher uninstall-desktop

# 2. Remove binary
make uninstall

# 3. Purge configuration and database
rm -rf ~/.config/antigravity-account-switcher
```

#### 5. Does the in-app auto-updater ("Check for Updates") work?
Yes! On Linux, Antigravity 2.0 uses Electron's `AppImageUpdater`. When Antigravity is run from an extracted `.tar.gz` without an AppImage runtime, `Help -> Check for Updates` normally fails with `ERR_UPDATER_OLD_FILE_NOT_FOUND` because the `APPIMAGE` environment variable is missing.

**Antigravity Account Switcher fixes this automatically.** When launched via `antigravity-account-switcher launch` (or from the `.desktop` launcher created by `install-desktop`), the supervisor automatically injects the exact `APPIMAGE` binary path into the Antigravity 2.0 process, allowing automatic background and manual in-app updates to work seamlessly out-of-the-box.


---

## Security

Please see [SECURITY.md](SECURITY.md) for vulnerability disclosure procedures and details regarding RFC 8252 §8.5 OAuth 2.0 public client credentials.

---

## Contributing

Contributions are welcome! Please read [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, testing with the Go data race detector, and PR guidelines.

---

## Credits & Original Author

This project was originally created and developed by **[Muriel Gasparini](https://github.com/Muriel-Gasparini)** ([Muriel-Gasparini/antigravity-account-switcher](https://github.com/Muriel-Gasparini/antigravity-account-switcher)).  
Maintained and updated by **[AugusttoDaniel](https://github.com/AugusttoDaniel)**.

---

## License

MIT License © 2026 Muriel Gasparini and © 2026 Daniel Augusto Silva. See [LICENSE](LICENSE) for details.
