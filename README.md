# FocusGate

FocusGate is a single Go and SQLite binary (~13 MB on disk, ~25 MB RAM) that filters RSS feeds, GitHub notifications, IMAP email, and webhooks for a Windows laptop and an Android phone (Redmi Note 11 5G).

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
4. Open `http://127.0.0.1:8787` on your laptop. On first visit, initialize your personal vault with a Master Passphrase or PIN.

## Personal Authentication & GitHub Profile

- **Master Passphrase Vault**: Guided first-time setup screen hashes your passphrase using PBKDF2-HMAC-SHA256 (100,000 iterations) in SQLite, issuing a persistent 30-day session cookie for desktop and mobile access.
- **Profile Drawer**: Access via the **PROFILE** button in the topbar to view session details, change your master passphrase, or lock the app immediately.
- **Dynamic GitHub Notifications**: Link your GitHub account directly from the Profile drawer without modifying configuration files:
  - **1-Click Device Flow**: Click **CONNECT WITH GITHUB**, open `github.com/login/device`, and confirm your one-time code.
  - **Classic PAT**: Paste a classic Personal Access Token with `notifications` read scope.
  - **Participating Only Filter**: Toggle between direct mentions/reviews/assignments only vs all watched repository activity.
  - Disconnect or update sync preferences at any time; background loops adapt dynamically.

## Redmi Note 11 5G setup

1. Install **ntfy** from Google Play or F-Droid.
2. Replace the example value for `notify.ntfy.topic` in `focusgate.yaml` with a unique, unguessable topic name (or configure an access-controlled topic or self-hosted ntfy server) before enabling notifications (`notify.ntfy.enabled: true`), then subscribe to that topic in the ntfy app.
3. In MIUI/HyperOS app settings for **ntfy**, set **Battery saver** to `No restrictions` and enable **Autostart**.
4. To open the FocusGate PWA from your phone over LAN or Tailscale:
   - For full PWA installation and service-worker caching on a phone, serve over HTTPS with a certificate trusted by the phone (e.g. via Tailscale Funnel / HTTPS cert, Caddy with local CA trusted on the device, or Cloudflare Tunnel).
   - If connecting over plain HTTP (`http://<ip>:8787`), the web interface runs as a standard browser app without service workers or offline caching (the browser localhost exception applies only directly on the host machine running the browser).
   - Set `server.listen` to `0.0.0.0:8787` (or your LAN/Tailscale IP).
   - Set `server.public_url` to your HTTPS URL (or `http://<ip>:8787` for browser-only use).
   - Set `FOCUSGATE_AUTH_TOKEN` and `FOCUSGATE_WEBHOOK_SECRET` (non-loopback bindings require both).
   - Open `server.public_url` in Chrome on your phone (tap **Add to Home screen** for HTTPS installs).

## Cross-compile for a Linux VPS

With pure-Go SQLite (`modernc.org/sqlite`) and `CGO_ENABLED=0`, you can cross-compile from Windows in one command:

```powershell
$env:CGO_ENABLED="0"; $env:GOOS="linux"; $env:GOARCH="amd64"
go build -ldflags="-s -w" -o focusgate-linux-amd64 ./cmd/focusgate
```

Use `$env:GOARCH="arm64"` for ARM VPS hosts or Termux. Or run the multi-platform release scripts:
- Windows: `powershell -ExecutionPolicy Bypass -File scripts/build-release.ps1`
- Linux/macOS: `bash scripts/build-release.sh`

## CI/CD pipelines

GitHub Actions workflows are configured in `.github/workflows/`:
- **`ci.yml`**: Runs on every push and pull request to `main`. Executes `gofmt -s` format checks, `go vet`, multi-OS unit tests (`ubuntu-latest`, `windows-latest`, `macos-latest`), and cross-compilation checks for Linux (amd64, arm64), macOS (amd64, arm64), and Windows (amd64).
- **`release.yml`**: Triggers on tag pushes (`v*.*.*`). Automatically cross-compiles static binaries, packages archives (`.tar.gz` and `.zip`), calculates SHA256 checksums (`checksums.txt`), and publishes a GitHub Release.

## CLI commands

- `focusgate serve [--config focusgate.yaml]`: Run pollers, digest scheduler, webhook endpoint, and PWA server.
- `focusgate check`: Run one poll and classification pass across configured sources and exit.
- `focusgate digest`: Dispatch queued digest items immediately.
- `focusgate mode <normal|deep_work|dnd>`: Switch the active focus mode.
- `focusgate simulate --title "Prod API 502" --body "Health check failing" --mode deep_work`: Dry-run classification without saving or sending notifications.
