package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"sync"
	"time"

	"focusgate/internal/classify"
	"focusgate/internal/config"
	"focusgate/internal/ingest"
	"focusgate/internal/notify"
	"focusgate/internal/store"
)

// Event represents a lightweight Server-Sent Event pushed to connected PWA clients.
type Event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// Engine coordinates ingestion, focus classification, persistence, notifications, and live UI updates.
type Engine struct {
	cfg        config.Config
	store      *store.Store
	router     *classify.Router
	dispatcher *notify.Dispatcher
	httpClient *http.Client

	mu             sync.Mutex
	mode           config.FocusMode
	lastDigestSlot string

	subMu       sync.RWMutex
	subscribers map[chan []byte]struct{}
}

// NewEngine initializes the FocusGate runtime engine from configuration and open SQLite store.
func NewEngine(ctx context.Context, cfg config.Config, st *store.Store) (*Engine, error) {
	mode, err := st.GetFocusMode(ctx, cfg.Focus.DefaultMode)
	if err != nil {
		return nil, err
	}
	return &Engine{
		cfg:        cfg,
		store:      st,
		router:     classify.NewRouter(cfg),
		dispatcher: notify.NewDispatcher(cfg),
		httpClient: &http.Client{Timeout: 15 * time.Second},
		mode:       mode,
		subscribers: make(map[chan []byte]struct{}),
	}, nil
}

// Config exposes the active runtime configuration.
func (e *Engine) Config() config.Config { return e.cfg }

// Store exposes the underlying SQLite store.
func (e *Engine) Store() *store.Store { return e.store }

// Mode returns the current FocusMode safely, refreshing from SQLite so
// out-of-process CLI mode changes take effect immediately.
func (e *Engine) Mode() config.FocusMode {
	if m, err := e.store.GetFocusMode(context.Background(), e.cfg.Focus.DefaultMode); err == nil {
		e.mu.Lock()
		e.mode = m
		e.mu.Unlock()
		return m
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mode
}

// SetMode updates the active FocusMode, persists it to SQLite, and notifies connected UIs.
func (e *Engine) SetMode(ctx context.Context, mode config.FocusMode) error {
	if !config.ValidFocusMode(mode) {
		return fmt.Errorf("invalid focus mode %q", mode)
	}
	if err := e.store.SetFocusMode(ctx, mode); err != nil {
		return err
	}
	e.mu.Lock()
	e.mode = mode
	e.mu.Unlock()
	e.broadcast("mode", map[string]string{"mode": string(mode)})
	e.broadcastStats(ctx)
	return nil
}

// RuntimeStatus summarizes RAM, disk footprint, and queue counters.
type RuntimeStatus struct {
	Stats       store.Stats `json:"stats"`
	HeapMB      float64     `json:"heap_mb"`
	SysMB       float64     `json:"sys_mb"`
	DBSizeKB    float64     `json:"db_size_kb"`
	Classifier  string      `json:"classifier"`
	NtfyEnabled bool        `json:"ntfy_enabled"`
}

// Status returns live resource usage and queue counts for the PWA telemetry strip.
func (e *Engine) Status(ctx context.Context) (RuntimeStatus, error) {
	stats, err := e.store.GetStats(ctx, e.Mode())
	if err != nil {
		return RuntimeStatus{}, err
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	classifierLabel := "heuristic (rules_only)"
	if e.cfg.Classifier.Mode == "hybrid" {
		if e.cfg.Classifier.TypeSafeAPIKey != "" {
			classifierLabel = "hybrid (rules + TypeSafe Jev)"
		} else {
			classifierLabel = "hybrid (rules + heuristic; set TYPESAFE_API_KEY)"
		}
	}

	return RuntimeStatus{
		Stats:       stats,
		HeapMB:      float64(m.HeapAlloc) / (1024 * 1024),
		SysMB:       float64(m.Sys) / (1024 * 1024),
		DBSizeKB:    float64(e.store.DBFileSizeBytes(e.cfg.Storage.Path)) / 1024.0,
		Classifier:  classifierLabel,
		NtfyEnabled: e.cfg.Notify.Ntfy.Enabled,
	}, nil
}

// ProcessItem deduplicates, classifies under current focus mode, stores, and dispatches an item.
func (e *Engine) ProcessItem(ctx context.Context, item ingest.IncomingItem) (store.StoredItem, bool, error) {
	item.Normalize(e.cfg.Storage.MaxSnippetChars)
	exists, err := e.store.Exists(ctx, item.SourceType, item.ExternalID)
	if err != nil {
		return store.StoredItem{}, false, err
	}
	if exists {
		return store.StoredItem{}, false, nil
	}

	dec := e.router.Evaluate(ctx, e.Mode(), item)
	stored, inserted, err := e.store.InsertItem(ctx, item, dec)
	if err != nil || !inserted {
		return stored, inserted, err
	}

	if dec.Route == "push_now" {
		if pushErr := e.dispatcher.PushItem(ctx, stored); pushErr != nil {
			log.Printf("notify error (%s): %v", stored.Title, pushErr)
		}
	}

	e.broadcast("item", stored)
	e.broadcastStats(ctx)
	return stored, true, nil
}

// SimulateItem classifies a synthetic or pasted item without persisting or sending notifications.
func (e *Engine) SimulateItem(ctx context.Context, mode config.FocusMode, item ingest.IncomingItem) classify.Decision {
	if !config.ValidFocusMode(mode) {
		mode = e.Mode()
	}
	item.Normalize(e.cfg.Storage.MaxSnippetChars)
	return e.router.Evaluate(ctx, mode, item)
}

// FlushDigest dispatches all queued items in one batch and marks them digested
// if at least one enabled channel succeeded (or no channels are enabled).
func (e *Engine) FlushDigest(ctx context.Context) (int, error) {
	queued, err := e.store.ListQueuedForDigest(ctx, 50)
	if err != nil || len(queued) == 0 {
		return 0, err
	}
	delivered, sendErr := e.dispatcher.SendDigest(ctx, queued)
	if sendErr != nil {
		if !delivered {
			return 0, sendErr
		}
		log.Printf("digest partial delivery warning: %v", sendErr)
	}
	ids := make([]int64, len(queued))
	for i, it := range queued {
		ids[i] = it.ID
	}
	if err := e.store.MarkDigested(ctx, ids); err != nil {
		return 0, err
	}
	e.broadcast("digest", map[string]int{"count": len(ids)})
	e.broadcastStats(ctx)
	return len(ids), nil
}

// SyncAll runs one full polling pass across RSS, GitHub, and IMAP sources.
func (e *Engine) SyncAll(ctx context.Context) (int, error) {
	totalNew := 0
	for _, rss := range e.cfg.Sources.RSS {
		n, err := e.pollSingleRSS(ctx, rss)
		if err != nil {
			log.Printf("rss poll (%s): %v", rss.Name, err)
			continue
		}
		totalNew += n
	}
	if e.cfg.Sources.GitHub.Enabled {
		n, err := e.pollGitHub(ctx)
		if err != nil {
			log.Printf("github poll: %v", err)
		} else {
			totalNew += n
		}
	}
	for _, im := range e.cfg.Sources.IMAP {
		if !im.Enabled {
			continue
		}
		n, err := e.pollSingleIMAP(ctx, im)
		if err != nil {
			log.Printf("imap poll (%s): %v", im.Name, err)
			continue
		}
		totalNew += n
	}
	return totalNew, nil
}

func (e *Engine) pollSingleRSS(ctx context.Context, src config.RSSSourceConfig) (int, error) {
	key := "rss:" + hashKey(src.URL)
	st, err := e.store.GetFeedState(ctx, key)
	if err != nil {
		return 0, err
	}
	items, nextCache, changed, err := ingest.FetchFeed(
		ctx, e.httpClient, src.Name, src.URL, src.Tag,
		ingest.FeedCache{ETag: st.ETag, LastModified: st.LastModified},
		e.cfg.Storage.MaxSnippetChars,
	)
	if err != nil || !changed {
		return 0, err
	}
	_ = e.store.SaveFeedState(ctx, store.FeedSyncState{
		SourceKey:    key,
		ETag:         nextCache.ETag,
		LastModified: nextCache.LastModified,
	})
	return e.ingestBatch(ctx, items), nil
}

func (e *Engine) pollGitHub(ctx context.Context) (int, error) {
	key := "github:notifications"
	st, err := e.store.GetFeedState(ctx, key)
	if err != nil {
		return 0, err
	}
	items, nextLM, err := ingest.FetchGitHubNotifications(
		ctx, e.httpClient, e.cfg.Sources.GitHub.Token, e.cfg.Sources.GitHub.ParticipatingOnly,
		st.LastModified, e.cfg.Storage.MaxSnippetChars,
	)
	if err != nil {
		return 0, err
	}
	if nextLM != st.LastModified {
		_ = e.store.SaveFeedState(ctx, store.FeedSyncState{
			SourceKey:    key,
			LastModified: nextLM,
		})
	}
	return e.ingestBatch(ctx, items), nil
}

func (e *Engine) pollSingleIMAP(ctx context.Context, src config.IMAPConfig) (int, error) {
	key := "imap:" + hashKey(src.Host+"|"+src.Username+"|"+src.Mailbox)
	st, err := e.store.GetFeedState(ctx, key)
	if err != nil {
		return 0, err
	}
	items, nextUID, err := ingest.FetchIMAPUnread(ctx, src, st.LastUID, e.cfg.Storage.MaxSnippetChars)
	if err != nil {
		return 0, err
	}
	if nextUID > st.LastUID {
		_ = e.store.SaveFeedState(ctx, store.FeedSyncState{
			SourceKey: key,
			LastUID:   nextUID,
		})
	}
	return e.ingestBatch(ctx, items), nil
}

func (e *Engine) ingestBatch(ctx context.Context, items []ingest.IncomingItem) int {
	added := 0
	for _, it := range items {
		_, inserted, err := e.ProcessItem(ctx, it)
		if err != nil {
			log.Printf("process item (%s): %v", it.Title, err)
			continue
		}
		if inserted {
			added++
		}
	}
	return added
}

// StartBackgroundLoops launches lightweight polling, digest schedule, and retention goroutines.
func (e *Engine) StartBackgroundLoops(ctx context.Context) {
	for _, rss := range e.cfg.Sources.RSS {
		src := rss
		go e.runIntervalLoop(ctx, time.Duration(src.PollMinutes)*time.Minute, func() {
			if _, err := e.pollSingleRSS(ctx, src); err != nil {
				log.Printf("rss (%s): %v", src.Name, err)
			}
		})
	}
	if e.cfg.Sources.GitHub.Enabled {
		go e.runIntervalLoop(ctx, time.Duration(e.cfg.Sources.GitHub.PollMinutes)*time.Minute, func() {
			if _, err := e.pollGitHub(ctx); err != nil {
				log.Printf("github: %v", err)
			}
		})
	}
	for _, im := range e.cfg.Sources.IMAP {
		if !im.Enabled {
			continue
		}
		src := im
		go e.runIntervalLoop(ctx, time.Duration(src.PollMinutes)*time.Minute, func() {
			if _, err := e.pollSingleIMAP(ctx, src); err != nil {
				log.Printf("imap (%s): %v", src.Name, err)
			}
		})
	}

	// Scheduled digest checker & daily retention pruner
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		var lastPruneDay int
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				e.maybeRunDigest(ctx, now)
				if now.Day() != lastPruneDay {
					lastPruneDay = now.Day()
					if deleted, err := e.store.PruneOld(ctx, e.cfg.Storage.RetentionDays); err == nil && deleted > 0 {
						log.Printf("pruned %d old items", deleted)
					}
				}
			}
		}
	}()
}

func (e *Engine) runIntervalLoop(ctx context.Context, every time.Duration, fn func()) {
	fn() // Initial sync on startup
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fn()
		}
	}
}

func (e *Engine) maybeRunDigest(ctx context.Context, now time.Time) {
	hhmm := now.Format("15:04")
	slot := now.Format("2006-01-02 ") + hhmm
	for _, target := range e.cfg.Focus.DigestTimes {
		if target == hhmm {
			e.mu.Lock()
			alreadyRan := e.lastDigestSlot == slot
			if !alreadyRan {
				e.lastDigestSlot = slot
			}
			e.mu.Unlock()
			if !alreadyRan {
				if n, err := e.FlushDigest(ctx); err != nil {
					log.Printf("scheduled digest error: %v", err)
				} else if n > 0 {
					log.Printf("sent scheduled digest (%d items)", n)
				}
			}
			return
		}
	}
}

// Subscribe registers an SSE client channel for real-time UI updates.
func (e *Engine) Subscribe() (ch chan []byte, unsubscribe func()) {
	ch = make(chan []byte, 16)
	e.subMu.Lock()
	e.subscribers[ch] = struct{}{}
	e.subMu.Unlock()
	return ch, func() {
		e.subMu.Lock()
		delete(e.subscribers, ch)
		close(ch)
		e.subMu.Unlock()
	}
}

func (e *Engine) broadcast(eventType string, data any) {
	raw, err := json.Marshal(Event{Type: eventType, Data: data})
	if err != nil {
		return
	}
	frame := []byte("event: " + eventType + "\ndata: " + string(raw) + "\n\n")
	e.subMu.RLock()
	defer e.subMu.RUnlock()
	for ch := range e.subscribers {
		select {
		case ch <- frame:
		default:
			// Drop slow client frames rather than blocking ingestion or accumulating memory.
		}
	}
}

func (e *Engine) broadcastStats(ctx context.Context) {
	if st, err := e.Status(ctx); err == nil {
		e.broadcast("status", st)
	}
}

func hashKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
