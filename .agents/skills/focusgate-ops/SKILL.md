---
name: focusgate-ops
description: >-
  Operate, verify, tune, and deploy FocusGate on Windows laptop, micro-VPS, and
  Redmi Note 11 5G. Use when starting or stopping the daemon, debugging ntfy or
  PWA LAN access, simulating focus-mode rules, or building cross-platform binaries.
---

# FocusGate Operations & Troubleshooting

## Quick Health & Memory Check

1. Run [healthcheck.ps1](./scripts/healthcheck.ps1) to verify the live HTTP server, active focus mode, queue counts, SQLite size, and process `WorkingSet64`.
2. To inspect routing decisions without modifying state or sending alerts, use the CLI simulator:
   ```powershell
   .\focusgate.exe simulate --mode deep_work --title "..." --body "..."
   ```

## Common Workflows

### Start / Stop / Autostart on Windows
- Run interactively: `.\focusgate.exe serve --config .\focusgate.yaml`
- Install login scheduled task (and optional Private LAN firewall rule for port `8787`):
  ```powershell
  .\scripts\install-windows-task.ps1 -AllowFirewallLAN
  ```

### Redmi Note 11 5G Delivery Checklist
1. Confirm `notify.ntfy.enabled: true` and `notify.ntfy.topic` uses an unguessable topic name in `focusgate.yaml`.
2. On the Redmi Note 11 5G, subscribe to the same topic in **ntfy** and set MIUI/HyperOS **Battery saver -> No restrictions**.
3. For remote PWA access (`http://<laptop-or-vps-ip>:8787`), set `server.listen: "0.0.0.0:8787"`, `server.public_url`, `FOCUSGATE_AUTH_TOKEN`, and `FOCUSGATE_WEBHOOK_SECRET` before opening port `8787`.

### Tuning Rules & TypeSafe Confidence
- Add deterministic Stage-1 rules in `focusgate.yaml` before relying on Stage-2 API calls.
- Keep `classifier.max_daily_api_calls` bounded to protect quota.
- Adjust focus breakthrough thresholds in `internal/classify/router.go` (`applyFocusPolicy`), then run `go test -count=1 ./internal/classify`.

### Cross-Compile for VPS / Termux
- Run `.\scripts\build-release.ps1` to generate `dist/focusgate-windows-amd64.exe`, `dist/focusgate-linux-amd64`, and `dist/focusgate-linux-arm64`.
- Pair Linux VPS deployments with `deploy/focusgate.service` (`MemoryMax=48M`).
