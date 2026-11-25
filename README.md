# FocusGate

FocusGate is a single Go and SQLite binary (about 14 MB on disk, with a 32 MiB Go soft memory target) that filters RSS feeds, GitHub notifications, IMAP email, and webhooks for a Windows laptop and an Android phone.

## Architecture

- **Single binary (`focusgate.exe` / `focusgate`)**: Polls RSS/Atom feeds, GitHub Notifications, read-only IMAP mailboxes (`BODY.PEEK`), and authenticated webhooks.
- **Two-stage routing with 0 MB local AI RAM**:
  1. Stage 1 runs local regex, sender, source, and GitHub reason rules from `focusgate.yaml`.
  2. Stage 2 sends only ambiguous items to TypeSafe System One (`jev-latest`) for four parallel judgments (`is_actionable`, `is_time_sensitive`, `category`, `urgency`), falling back to local word-boundary heuristics when `TYPESAFE_API_KEY` is unset.
- **Focus modes (`normal`, `deep_work`, `dnd`)**: Only high-confidence outages, security alerts, or priority-5 rules break through `deep_work` or `dnd`. Other items queue for scheduled digests.
- **Phone and laptop delivery**:
  - Pushes immediate breakthroughs and scheduled digest batches to Android via **ntfy** (reliable under MIUI/HyperOS battery restrictions without a custom APK).
  - Serves an embedded PWA at `/` for switching focus modes, reviewing queued items, and dry-running rules.
  - Triggers native Windows toast notifications on demand with zero idle GUI memory.

## Quick start (Windows)

1. Copy the example config:
   ```powershell
   copy focusgate.example.yaml focusgate.yaml
   ```
2. Set optional environment variables for API keys and network auth:
   ```powershell
   $env:TYPESAFE_API_KEY = "your-typesafe-key"
   $env:FOCUSGATE_AUTH_TOKEN = "pick-a-long-random-token"
   $env:FOCUSGATE_WEBHOOK_SECRET = "pick-a-webhook-secret"
   ```
3. Build and run:
   ```powershell
   go build -ldflags="-s -w" -o focusgate.exe ./cmd/focusgate
   .\focusgate.exe serve
   ```
4. Open `http://127.0.0.1:8787` on your laptop. On first visit, initialize your personal vault with a Master Passphrase of at least 12 characters.

## Accounts and integrations

On a fresh loopback-only installation without a server token, initialize the vault in the dashboard. On a remote installation, open the dashboard over HTTPS, leave the username blank, and enter the configured server token to open the setup form. Keep the token restricted to the operator.

For a shared instance, enable `server.allow_registration` to let members create accounts after setup. Turn it off again to close remote signups. The loopback-only dashboard continues to support local signup.

- **Members** get their own inbox, focus mode, RSS/Atom subscriptions, and semantic API allowance. Manage feeds in the Sources drawer. Each account can add up to eight feeds. Unsubscribing retains inbox history.
- **Operator** controls the server-wide GitHub, IMAP, webhook, and outbound delivery integrations. Member items appear in their own dashboard and do not use operator notification destinations.
- **Capacity** stays within a single daemon and SQLite database. The daemon polls user feeds one at a time, checking up to four due feeds per cycle. Each RSS or Atom document can contain up to 512 entries, 16,384 XML elements and 64 levels of nesting within 1 MiB. The poller rejects larger documents and keeps the previous sync position. The provider-wide call cap still applies in addition to the member limit.

- **Master Passphrase Vault**: Guided first-time setup screen hashes your passphrase using PBKDF2-HMAC-SHA256 (100,000 iterations) in SQLite, issuing a persistent 30-day session cookie for desktop and mobile access.
- **Profile Drawer**: Access via the **PROFILE** button in the topbar to view session details, change your master passphrase, or lock the app immediately.
- **Dynamic GitHub Notifications**: Link your GitHub account directly from the Profile drawer without modifying configuration files:
  - **1-Click Device Flow**: Click **CONNECT WITH GITHUB**, open `github.com/login/device`, and confirm your one-time code.
  - **Classic PAT**: Paste a classic Personal Access Token with `notifications` read scope.
  - **Participating Only Filter**: Toggle between direct mentions/reviews/assignments only vs all watched repository activity.
  - Disconnect or update sync preferences at any time; background loops adapt dynamically.

## Android Phone Setup

The native client syncs the latest server inbox page and sends focus changes and archives to the signed-in account. Items fetched directly on the device can be archived locally.

### 1-Tap PWA Install & Phone Alerts

FocusGate runs as an installable Progressive Web App (PWA) on your Android phone, with push breakthroughs delivered via **ntfy**:

1. **Install ntfy for Outbound Push Alerts**:
   - Install **ntfy** from Google Play or F-Droid.
   - Set a unique topic in `focusgate.yaml` (`notify.ntfy.topic: "your-secret-topic"`) and enable it (`notify.ntfy.enabled: true`).
   - Subscribe to the topic in the ntfy app. In MIUI/HyperOS settings, set **Battery saver** to `No restrictions` and enable **Autostart**.

2. **Access & Install the Dashboard on Phone**:
   - **Full PWA Installation (HTTPS)**: Serve FocusGate over HTTPS (e.g. via Tailscale Funnel, Tailscale Serve with HTTPS cert, Caddy with trusted CA, or Cloudflare Tunnel). Open the HTTPS URL in Chrome/Firefox on Android, tap the install banner or menu -> **Install app** / **Add to Home screen**. The app installs directly to your home screen and launches in fullscreen standalone mode with service-worker offline caching.
   - **Secure LAN Browser Access**: To access FocusGate across your local network or phone:
     1. For an HTTPS proxy on the same machine, keep `server.listen: "127.0.0.1:8787"` and set `server.public_url` to the HTTPS address that the phone will open.
     2. Set `server.auth_token` (or environment variable `FOCUSGATE_AUTH_TOKEN`) and `sources.webhook.secret` (or `FOCUSGATE_WEBHOOK_SECRET`), which are required for security validation on non-loopback network interfaces.
     3. Put a trusted HTTPS reverse proxy in front of FocusGate and open its HTTPS address on the phone. HTTP access is suitable only inside an encrypted private tunnel. API tokens do not encrypt browser traffic.

## Cross-compile for a Linux VPS or Android (Termux)

With pure-Go SQLite (`modernc.org/sqlite`) and `CGO_ENABLED=0`, you can cross-compile from Windows in one command:

```powershell
$env:CGO_ENABLED="0"; $env:GOOS="linux"; $env:GOARCH="amd64"
go build -ldflags="-s -w" -o focusgate-linux-amd64 ./cmd/focusgate
```

Use `$env:GOARCH="arm64"` for ARM VPS hosts or `$env:GOOS="android"; $env:GOARCH="arm64"` for native Android / Termux. Or run the multi-platform release scripts:
- Windows: `powershell -ExecutionPolicy Bypass -File scripts/build-release.ps1`
- Linux/macOS: `bash scripts/build-release.sh`

## CI/CD pipelines

GitHub Actions workflows are configured in `.github/workflows/`:
- **`ci.yml`**: Runs on every push and pull request to `main`. Executes `gofmt -s` format checks, `go vet`, multi-OS unit tests (`ubuntu-latest`, `windows-latest`, `macos-latest`), and cross-compilation checks for Linux (amd64, arm64), macOS (amd64, arm64), Windows (amd64), and Android (arm64).
- **`release.yml`**: Triggers on tag pushes (`v*.*.*`). Automatically cross-compiles static binaries, packages archives (`.tar.gz` and `.zip`), calculates SHA256 checksums (`checksums.txt`), and publishes a GitHub Release for all platforms including Android.
- **`android.yml`**: Packages and verifies a native preview APK for every pull request to `main`, Android-related pushes to `main`, and manual runs. The `focusgate-preview-apk` artifact contains `focusgate-preview.apk` and builds without signing secrets.

The `CI required (PR)` check passes only when format and vet checks, all platform tests, and all cross-compiles succeed. `Android preview (PR)` verifies the installable test APK. Require both checks on `main` in GitHub branch protection.

The standalone artifact workflow uses Fyne's development signing certificate and the preview app ID `xyz.sandoxed.focusgate.preview` for testing. Preview builds are not production releases and do not guarantee a stable signing identity across packaging-tool changes; reinstalling a preview can require removing the previous preview and its local data. Tagged releases use `xyz.sandoxed.focusgate` and require a persistent Android signing keystore. Set `ANDROID_KEYSTORE_BASE64`, `ANDROID_KEYSTORE_PASSWORD`, `ANDROID_KEY_ALIAS`, and `ANDROID_KEY_PASSWORD` in the repository’s Actions secrets for tagged releases. Updates install over an existing APK only when the signing identity matches that installation.

## CLI commands

- `focusgate serve [--config focusgate.yaml]`: Run pollers, digest scheduler, webhook endpoint, and PWA server.
- `focusgate check`: Run one poll and classification pass across configured sources and exit.
- `focusgate digest`: Dispatch queued digest items immediately.
- `focusgate mode <normal|deep_work|dnd>`: Switch the active focus mode.
- `focusgate simulate --title "Prod API 502" --body "Health check failing" --mode deep_work`: Dry-run classification without saving or sending notifications.
- `focusgate update [--check]`: Check for or install in-place updates directly from GitHub Releases.
- `focusgate version`: Print version, build commit, and system architecture.

## Roadmap & Architecture RFCs

For upcoming designs—including our pruned local AI decision engine ("Nano-Laya"), terminal TUI, calendar sync, and zero-config mesh pairing—see [FUTURE.md](FUTURE.md).

The bundled systemd unit protects the executable directory. Apply service updates through the operator, then restart the service. The standalone CLI updater streams archives through temporary files and requires a release checksum manifest.

Each IMAP poll examines the mailbox read-only and fetches previews for up to 20 messages. The client caps response data at 2 MiB per connection, including search results and envelopes. A response that exceeds this limit leaves the stored cursor available for retry.

### Shared access and local simulation

New account passwords and passphrases require 12 to 1024 characters. Existing credentials still work. Authentication permits 10 login attempts per account and 60 credential requests per socket peer per minute, with bounded in-memory counters. For a same-host HTTPS proxy, set `server.trusted_proxy_ips: ["127.0.0.1"]` (include `"::1"` if used) and make the proxy overwrite `X-Real-IP` with the client socket IP, for example Nginx `proxy_set_header X-Real-IP $remote_addr;`. Credential quotas then apply separately to those client IPs. Headers from all other peers are ignored; missing or invalid headers retain the socket-peer budget. Restrict direct access to the backend listener and keep per-client limits at the proxy. Without this configuration, all proxied clients share the 60-request upstream-peer budget; configure an aggregate proxy limit below that budget. Enter operator tokens through the sign-in form; token URLs are no longer accepted.

`focusgate simulate` uses local rules and heuristics without opening SQLite or contacting the AI provider. Add `--semantic` to explicitly use the configured provider and persisted quota. Semantic classification sends the item's sender, title, snippet and configured user context to that provider. The authenticated classification API also uses that shared quota.

Outbound delivery receipts track success per item and enabled destination. Failed destinations can retry without repeating recorded successes. A crash between an external send and its SQLite receipt can still repeat that send; external services do not share a transaction with SQLite.

The Android inbox caches the newest 50 server items per sync and applies the server's focus mode. Older items stay in the local history until retention or the queue cap removes them. A mode change applies locally when the server is unreachable. An explicit server rejection preserves the previous mode. The next successful sync adopts the server mode. Missing or inaccessible archive targets return HTTP 404.

On Windows, the updater retains `focusgate.exe.old` as a recovery copy. If Windows or the process stops between the two rename steps, stop the service and restore that file to `focusgate.exe` before starting again. The Windows replacement steps do not form one atomic operation. New IMAP message IDs include the full mailbox endpoint and port, including TLS port 993. Old portless IDs lack reliable per-item endpoint provenance because item insertion and cursor updates were separate operations. Legacy items retain their original rows; replay can import the message again under its port-qualified identity. This avoids merging messages from different mailboxes.

The native phone client requires HTTPS for remote servers and accepts plain HTTP on loopback. When upgrading a cache that only records a token, the phone checks which account that token belongs to before carrying history forward. An expired token requires a fresh cache after sign-in.

Configuration loading rejects unknown routing rule fields and empty selectors, including selectors that depend on missing environment variables. Omit selectors deliberately to define a catch-all rule.
