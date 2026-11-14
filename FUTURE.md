# FocusGate — Future Roadmap & Architecture RFCs

This document outlines architectural designs, research directions, and planned features for FocusGate beyond the core web UI and server runtime.

---

## 1. Ultra-Lightweight Local AI: Pruned & Distilled Laya ("Nano-Laya")

### The Concept
FocusGate uses a 2-stage hybrid router: Stage 1 handles deterministic regex/metadata rules, while Stage 2 handles ambiguous semantic classification. Today, Stage 2 connects via HTTPS to TypeSafe Jev (`jev-latest`). 

To make FocusGate **100% offline, privacy-first, and zero-cost out of the box**, we can deploy a specialized, pruned local model derived from **Laya** (Convai Innovations' open-source System 1 decision model, based on ModernBERT).

### Weight Stripping & Pruning Architecture

Standard base models carry tens of millions of unused parameters that are unnecessary for developer notification routing. By removing unused pre-training weights and grafting custom task heads, we achieve dramatic reductions in size, memory, and latency:

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                       Standard Laya Base (~350M Params)                     │
│  [Vocab Embeddings (50k tokens, ~38M)]  [22-28 Transformer Layers]  [MLM]   │
└─────────────────────────────────────────────────────────────────────────────┘
                                      │
               Apply Weight Stripping & Layer Dropping:
               1. Vocab Pruning: 50,000 -> 14,000 dev tokens (-65% embeddings)
               2. Layer Truncation: 24 layers -> 8 layers (-67% depth)
               3. Head Replacement: Discard MLM, graft 4 task heads (+8 KB)
               4. Quantization: FP16 -> INT8 ONNX (-50% storage)
                                      │
                                      ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                 FocusGate Nano-Laya (~65M Params, ~45 MB INT8)              │
│                                                                             │
│   Input: (source_type, title, body_snippet, reason, user_context)           │
│                                 │                                           │
│   ┌─────────────────────────────┴─────────────────────────────┐             │
│   │ 8-Layer Pruned ModernBERT Transformer Backbone            │             │
│   │ (~65M parameters, INT8 quantized, 80 MB RAM footprint)    │             │
│   └─────────────────────────────┬─────────────────────────────┘             │
│                                 │                                           │
│   ┌───────────────┬─────────────┴─┬─────────────────┬───────────────────┐   │
│   ▼               ▼               ▼                 ▼                   │   │
│ [Head 1]        [Head 2]        [Head 3]          [Head 4]              │   │
│ Actionability   Time-Sensitive  Category          Urgency               │   │
│ Sigmoid [0..1]  Sigmoid [0..1]  5-Class Softmax   Ordinal (0..3)        │   │
└─────────────────────────────────────────────────────────────────────────────┘
```

### Metrics Comparison: Base Model vs. Nano-Laya

| Metric | Base Laya (Zero-Shot) | FocusGate Nano-Laya (Pruned & Fine-Tuned) | Improvement |
| :--- | :--- | :--- | :--- |
| **Parameter Count** | ~350M | **~65M** | **~81% fewer parameters** |
| **Disk Storage** | ~700 MB (FP16) / ~250 MB (INT8) | **~45 MB (INT8 ONNX)** | **~82% smaller artifact** |
| **RAM Footprint** | ~400–450 MB | **~75–90 MB** | **~80% RAM savings** |
| **CPU Latency** | ~35–50 ms | **~6–10 ms** | **~4x to 5x faster** |
| **Setup Cost** | Requires external Python service | **Embedded in Go via `onnxruntime-go`** | Zero dependencies |
| **Privacy & Offline**| Local or Cloud | 100% Offline / Airplane-ready | Air-gapped privacy |

### Fine-Tuning Recipe
1. **Dataset Generation:** 10,000 synthetic + bootstrapped notification payloads across GitHub (PR reviews, mentions, CI breaks, releases), IMAP emails (incidents, client pings, newsletters, spam), and RSS feeds.
2. **Loss Function:** Multi-task combined loss:
   $$\mathcal{L} = \mathcal{L}_{\text{actionable}} + \mathcal{L}_{\text{time\_sensitive}} + \mathcal{L}_{\text{category}} + \mathcal{L}_{\text{urgency}}$$
3. **Training Budget:** Less than 30 minutes on a single consumer GPU (RTX 3060 / 4060 or Google Colab T4).

---

## 2. Terminal Companion & TUI (`focusgate tui`)

- **Bubble Tea Terminal UI:** Keyboard-driven terminal interface rendering an ASCII pager display, live feed scrolling, search filter, and instant focus mode switches (`N` Normal, `D` Deep Work, `Z` DND).
- **Unix Pipeline Ingestion:**
  ```bash
  # Send build alerts straight to FocusGate
  make test && focusgate push "Tests Passed" "All 42 packages green" --priority 2
  ```
- **Shell / Status Bar Integration:**
  - Waybar, Polybar, and tmux status output (`focusgate status --format waybar` or `--format json`).

---

## 3. Context-Aware Auto Focus

- **Calendar Integration (iCal / CalDAV / Google Calendar):**
  - Read active events to auto-switch into `deep_work` or `dnd` during scheduled meetings or focus blocks.
  - Automatically flush queued digests once meetings conclude.
- **Process & Full-Screen Watcher:**
  - Detect IDE activity (`code.exe`, `nvim`, `goland`) or full-screen presentations to suppress non-urgent interruptions.

---

## 4. Smart Digest Synthesis

- **Executive Summary:** Synthesize 20 queued low-priority notifications into a structured 4-bullet brief rather than displaying raw notifications.
- **Audio Voice Briefing:** Generate an optional 45-second audio summary via local TTS and push it to mobile devices over ntfy for commute listening.

---

## 5. Zero-Config Mobile & Mesh Sync (`focusgate pair`)

- Running `focusgate pair` prints an ASCII QR code in the terminal containing a short-lived signed pairing ticket and local Tailscale/LAN URL.
- Scanning with a phone camera instantly opens the PWA and authenticates local sessions without typing IPs or passwords.
