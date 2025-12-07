# FocusGate Project Rules

FocusGate is an ultra-low-RAM, single-binary Go + pure-Go SQLite notification and feed router built for a Windows laptop (or micro-VPS) and a Redmi Note 11 5G companion via `ntfy` and an embedded PWA.

## Core Architectural Invariants

1. **Zero CGO & Pure-Go Stack**:
   - Keep `CGO_ENABLED=0` cross-compilation working (`modernc.org/sqlite`, stdlib XML/HTTP, pure-Go dependencies). Never introduce CGO-only dependencies or heavy runtimes.
2. **Strict Low-RAM & Bounded Storage**:
   - Preserve `debug.SetMemoryLimit(32 << 20)` in `cmd/focusgate/main.go`.
   - Keep SQLite low-memory PRAGMAs (`cache_size(-2000)`, `temp_store=FILE`, `mmap_size=0`, `SetMaxOpenConns(1)`) and retention pruning intact.
   - Always bound external network reads with `io.LimitReader` and truncate stored snippets to `cfg.Storage.MaxSnippetChars`.
   - Keep the embedded PWA (`internal/web/static/`) framework-free (vanilla HTML/CSS/JS).
3. **Read-Only Ingestion & Deduplication Before AI**:
   - IMAP ingestion must stay read-only (`EXAMINE` + `BODY.PEEK`) and bounded to short text previews.
   - Always check `store.Exists(ctx, sourceType, externalID)` before invoking the Stage-2 TypeSafe System One API (`POST /v1/systemone`).
4. **Policy in Code, Secrets in Env**:
   - Keep focus routing thresholds (`normal`, `deep_work`, `dnd`) in deterministic Go code (`internal/classify/router.go`) over calibrated probabilities.
   - Never commit or hardcode secrets in `focusgate.yaml` or code; use `${ENV_VAR}` expansion.
5. **Verification After Any Edit**:
   - Run `go vet ./...` and `go test -count=1 ./...` after every code change.
   - Rebuild `focusgate.exe` (`go build -ldflags="-s -w" -o focusgate.exe ./cmd/focusgate`) when embedded static assets or Go files change.
