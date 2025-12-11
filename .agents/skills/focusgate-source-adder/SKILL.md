---
name: focusgate-source-adder
description: >-
  Add a new notification or feed ingestion source to FocusGate while keeping
  binary size, memory usage, deduplication, and tests compliant. Use when adding
  a new poller, webhook format, or mobile notification bridge.
---

# Adding a New FocusGate Source

## Steps

1. **Config Schema (`internal/config/config.go`)**:
   - Add source fields under `SourcesConfig`, apply defaults and validation inside `NormalizeAndValidate()`, and document an example disabled block in `focusgate.example.yaml`.
2. **Ingestor (`internal/ingest/<source>.go`)**:
   - Normalize every parsed message into `ingest.IncomingItem` and call `item.Normalize(maxSnippet)`.
   - Set a deterministic `ExternalID` so `store.Exists` deduplicates before Stage-2 classification.
   - Cap network reads with `io.LimitReader` (1–2 MiB max) and strip HTML with `ingest.StripHTML`.
   - Use conditional HTTP headers (`ETag` / `Last-Modified`) or cursor watermarks via `store.GetFeedState` / `store.SaveFeedState`.
3. **Engine Wiring (`internal/app/engine.go`)**:
   - Wire the new poller into both `SyncAll` and `StartBackgroundLoops` using `runIntervalLoop`.
4. **Verification**:
   - Add table/mock HTTP tests in `internal/ingest/ingest_test.go`.
   - Run `go vet ./... && go test -count=1 ./...` and rebuild `focusgate.exe`.
