# FocusGate

FocusGate is a single Go and SQLite binary (~13 MB on disk, ~25 MB RAM) that filters RSS feeds, GitHub notifications, IMAP email, and webhooks for a Windows laptop and an Android phone.

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

## Android Phone Setup

### 1-Tap PWA Install & Phone Alerts

FocusGate runs as an installable Progressive Web App (PWA) on your Android phone, with push breakthroughs delivered via **ntfy**:

1. **Install ntfy for Outbound Push Alerts**:
   - Install **ntfy** from Google Play or F-Droid.
   - Set a unique topic in `focusgate.yaml` (`notify.ntfy.topic: "your-secret-topic"`) and enable it (`notify.ntfy.enabled: true`).
   - Subscribe to the topic in the ntfy app. In MIUI/HyperOS settings, set **Battery saver** to `No restrictions` and enable **Autostart**.

2. **Access & Install the Dashboard on Phone**:
   - **Full PWA Installation (HTTPS)**: Serve FocusGate over HTTPS (e.g. via Tailscale Funnel, Tailscale Serve with HTTPS cert, Caddy with trusted CA, or Cloudflare Tunnel). Open the HTTPS URL in Chrome/Firefox on Android, tap the install banner or menu -> **Install app** / **Add to Home screen**. The app installs directly to your home screen and launches in fullscreen standalone mode with service-worker offline caching.
   - **LAN / HTTP Browser Access**: To access FocusGate across your local network or phone:
     1. In `focusgate.yaml`, bind `server.listen: "0.0.0.0:8787"` (or your LAN/Tailscale IP) and set `server.public_url: "http://<ip>:8787"`.
     2. Set `server.auth_token` (or environment variable `FOCUSGATE_AUTH_TOKEN`) and `server.webhook_secret` (or `FOCUSGATE_WEBHOOK_SECRET`), which are required for security validation on non-loopback network interfaces.
     3. Open `http://<ip>:8787` in Chrome on your phone to use all dashboard features and create a browser home-screen shortcut (the browser localhost exception for service workers applies only directly on the loopback host).

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
