package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
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

	digestMu sync.Mutex

	subMu       sync.RWMutex
	subscribers map[chan []byte]struct{}
}

// NewEngine initializes the FocusGate runtime engine from configuration and open SQLite store.
func NewEngine(ctx context.Context, cfg config.Config, st *store.Store) (*Engine, error) {
	mode, err := st.GetFocusMode(ctx, cfg.Focus.DefaultMode)
	if err != nil {
		return nil, err
	}
	r := classify.NewRouter(cfg)
	if st != nil {
		r.SetUsageTracker(st)
	}
	return &Engine{
		cfg:         cfg,
		store:       st,
		router:      r,
		dispatcher:  notify.NewDispatcher(cfg),
		httpClient:  &http.Client{Timeout: 15 * time.Second},
		mode:        mode,
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

// SourceSummary describes a configured ingestion source without exposing secrets.
type SourceSummary struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Detail  string `json:"detail"`
}

// RuntimeStatus summarizes RAM, disk footprint, queue counters, quota, and channels.
type RuntimeStatus struct {
	Stats          store.Stats     `json:"stats"`
	HeapMB         float64         `json:"heap_mb"`
	SysMB          float64         `json:"sys_mb"`
	DBSizeKB       float64         `json:"db_size_kb"`
	Classifier     string          `json:"classifier"`
	NtfyEnabled    bool            `json:"ntfy_enabled"`
	DesktopEnabled bool            `json:"desktop_enabled"`
	NextDigestAt   string          `json:"next_digest_at,omitempty"`
	NextDigestHHMM string          `json:"next_digest_hhmm,omitempty"`
	DigestTimes    []string        `json:"digest_times"`
	APIUsedToday   int             `json:"api_used_today"`
	APIDailyLimit  int             `json:"api_daily_limit"`
	Sources        []SourceSummary `json:"sources"`
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

	usedAPI, limitAPI := e.router.DailyAPIUsage()
	nextTime, nextLabel, ok := NextDigestSlot(time.Now(), e.cfg.Focus.DigestTimes)
	nextAt := ""
	if ok {
		nextAt = nextTime.Format(time.RFC3339)
	}

	return RuntimeStatus{
		Stats:          stats,
		HeapMB:         float64(m.HeapAlloc) / (1024 * 1024),
		SysMB:          float64(m.Sys) / (1024 * 1024),
		DBSizeKB:       float64(e.store.DBFileSizeBytes(e.cfg.Storage.Path)) / 1024.0,
		Classifier:     classifierLabel,
		NtfyEnabled:    e.cfg.Notify.Ntfy.Enabled,
		DesktopEnabled: e.cfg.Notify.DesktopWindows.Enabled,
		NextDigestAt:   nextAt,
		NextDigestHHMM: nextLabel,
		DigestTimes:    append([]string(nil), e.cfg.Focus.DigestTimes...),
		APIUsedToday:   usedAPI,
		APIDailyLimit:  limitAPI,
		Sources:        e.summarizeSources(),
	}, nil
}

func (e *Engine) summarizeSources() []SourceSummary {
	out := make([]SourceSummary, 0, len(e.cfg.Sources.RSS)+len(e.cfg.Sources.IMAP)+2)
	for _, r := range e.cfg.Sources.RSS {
		out = append(out, SourceSummary{
			Kind:    "rss",
			Name:    r.Name,
			Enabled: true,
			Detail:  fmt.Sprintf("Every %dm", max(1, r.PollMinutes)),
		})
	}
	ghDetail := "Disabled"
	if e.cfg.Sources.GitHub.Enabled {
		ghDetail = fmt.Sprintf("Every %dm", max(1, e.cfg.Sources.GitHub.PollMinutes))
	}
	out = append(out, SourceSummary{
		Kind:    "github",
		Name:    "GitHub Notifications",
		Enabled: e.cfg.Sources.GitHub.Enabled,
		Detail:  ghDetail,
	})
	for _, im := range e.cfg.Sources.IMAP {
		detail := "Disabled"
		if im.Enabled {
			detail = fmt.Sprintf("%s · %dm", im.Host, max(1, im.PollMinutes))
		}
		out = append(out, SourceSummary{
			Kind:    "imap",
			Name:    im.Name,
			Enabled: im.Enabled,
			Detail:  detail,
		})
	}
	whDetail := "POST /api/webhook/{source}"
	if !e.cfg.Sources.Webhook.Enabled {
		whDetail = "Disabled"
	}
	out = append(out, SourceSummary{
		Kind:    "webhook",
		Name:    "Inbound Webhooks",
		Enabled: e.cfg.Sources.Webhook.Enabled,
		Detail:  whDetail,
	})
	return out
}

// NextDigestSlot finds the earliest upcoming scheduled digest timestamp at or after now+1m.
func NextDigestSlot(now time.Time, slots []string) (time.Time, string, bool) {
	if len(slots) == 0 {
		return time.Time{}, "", false
	}
	type parsedSlot struct {
		hour int
		min  int
		raw  string
	}
	valid := make([]parsedSlot, 0, len(slots))
	for _, raw := range slots {
		parts := strings.Split(strings.TrimSpace(raw), ":")
		if len(parts) != 2 {
			continue
		}
		h, errH := strconv.Atoi(parts[0])
		m, errM := strconv.Atoi(parts[1])
		if errH != nil || errM != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			continue
		}
		valid = append(valid, parsedSlot{hour: h, min: m, raw: fmt.Sprintf("%02d:%02d", h, m)})
	}
	if len(valid) == 0 {
		return time.Time{}, "", false
	}
	sort.Slice(valid, func(i, j int) bool {
		if valid[i].hour != valid[j].hour {
			return valid[i].hour < valid[j].hour
		}
		return valid[i].min < valid[j].min
	})
	loc := now.Location()
	for dayOffset := 0; dayOffset <= 1; dayOffset++ {
		day := now.AddDate(0, 0, dayOffset)
		for _, s := range valid {
			cand := time.Date(day.Year(), day.Month(), day.Day(), s.hour, s.min, 0, 0, loc)
			if cand.After(now) {
				return cand, s.raw, true
			}
		}
	}
	return time.Time{}, "", false
}

// BulkUpdateStatus updates multiple items and broadcasts refreshed stats over SSE.
func (e *Engine) BulkUpdateStatus(ctx context.Context, ids []int64, fromStatus, toStatus string) (int64, error) {
	n, err := e.store.BulkUpdateStatus(ctx, ids, fromStatus, toStatus)
	if err != nil {
		return 0, err
	}
	e.broadcastStats(ctx)
	return n, nil
}

// SendTestAlert dispatches a test alert to enabled notification channels.
func (e *Engine) SendTestAlert(ctx context.Context) ([]string, error) {
	return e.dispatcher.SendTestAlert(ctx)
}

// ProcessItem deduplicates, classifies, applies the active focus mode, stores, and dispatches an item.
func (e *Engine) ProcessItem(ctx context.Context, item ingest.IncomingItem) (store.StoredItem, bool, error) {
	item.Normalize(e.cfg.Storage.MaxSnippetChars)
	exists, err := e.store.Exists(ctx, item.SourceType, item.ExternalID)
	if err != nil {
		return store.StoredItem{}, false, err
	}
	if exists {
		return store.StoredItem{}, false, nil
	}

	match, sig := e.router.Classify(ctx, e.Mode(), item)
	// Fetch the latest focus mode after classification completes to ensure
	// mode transitions during inflight semantic requests are respected.
	currentMode := e.Mode()
	dec := e.router.Route(currentMode, match, sig)

	stored, inserted, err := e.store.InsertItem(ctx, item, dec)
	if err != nil || !inserted {
		return stored, inserted, err
	}

	if dec.Route == "push_now" {
		if pushErr := e.dispatcher.PushItem(ctx, stored); pushErr != nil {
			log.Printf("notify error (%s): %v", stored.Title, pushErr)
			if _, qErr := e.store.BulkUpdateStatus(ctx, []int64{stored.ID}, stored.Status, "queued"); qErr != nil {
				log.Printf("requeue after push failure (%s): %v", stored.Title, qErr)
			} else {
				stored.Status = "queued"
			}
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
	match, sig := e.router.Classify(ctx, mode, item)
	return e.router.Route(mode, match, sig)
}

// FlushDigest dispatches all queued items in bounded batches until no eligible queued items
// remain, marking each batch digested if delivery succeeds.
// Holds digestMu across the operation so concurrent calls cannot duplicate dispatch.
func (e *Engine) FlushDigest(ctx context.Context) (int, error) {
	e.digestMu.Lock()
	defer e.digestMu.Unlock()

	const batchSize = 50
	totalFlushed := 0
	for {
		queued, err := e.store.ListQueuedForDigest(ctx, batchSize)
		if err != nil || len(queued) == 0 {
			if totalFlushed > 0 {
				e.broadcast("digest", map[string]int{"count": totalFlushed})
				e.broadcastStats(ctx)
			}
			return totalFlushed, err
		}
		delivered, sendErr := e.dispatcher.SendDigest(ctx, queued)
		if sendErr != nil {
			if !delivered {
				if totalFlushed > 0 {
					e.broadcast("digest", map[string]int{"count": totalFlushed})
					e.broadcastStats(ctx)
				}
				return totalFlushed, sendErr
			}
			log.Printf("digest partial delivery warning: %v", sendErr)
		}
		ids := make([]int64, len(queued))
		for i, it := range queued {
			ids[i] = it.ID
		}
		if err := e.store.MarkDigested(ctx, ids); err != nil {
			if totalFlushed > 0 {
				e.broadcast("digest", map[string]int{"count": totalFlushed})
				e.broadcastStats(ctx)
			}
			return totalFlushed, err
		}
		totalFlushed += len(ids)
		if len(queued) < batchSize {
			break
		}
	}
	e.broadcast("digest", map[string]int{"count": totalFlushed})
	e.broadcastStats(ctx)
	return totalFlushed, nil
}

// SyncAll runs one full polling pass across RSS, GitHub, and IMAP sources.
// It continues polling remaining sources even if some fail, returning total ingested
// items and an aggregated error if any source failed.
func (e *Engine) SyncAll(ctx context.Context) (int, error) {
	totalNew := 0
	var pollErrors []error
	for _, rss := range e.cfg.Sources.RSS {
		n, err := e.pollSingleRSS(ctx, rss)
		totalNew += n
		if err != nil {
			log.Printf("rss poll (%s): %v", rss.Name, err)
			pollErrors = append(pollErrors, fmt.Errorf("rss (%s): %w", rss.Name, err))
			continue
		}
	}
	if e.cfg.Sources.GitHub.Enabled {
		n, err := e.pollGitHub(ctx)
		totalNew += n
		if err != nil {
			log.Printf("github poll: %v", err)
			pollErrors = append(pollErrors, fmt.Errorf("github: %w", err))
		}
	}
	for _, im := range e.cfg.Sources.IMAP {
		if !im.Enabled {
			continue
		}
		n, err := e.pollSingleIMAP(ctx, im)
		totalNew += n
		if err != nil {
			log.Printf("imap poll (%s): %v", im.Name, err)
			pollErrors = append(pollErrors, fmt.Errorf("imap (%s): %w", im.Name, err))
			continue
		}
	}
	return totalNew, errors.Join(pollErrors...)
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
	added, err := e.ingestBatch(ctx, items)
	if err != nil {
		return added, err
	}
	if err := e.store.SaveFeedState(ctx, store.FeedSyncState{
		SourceKey:    key,
		ETag:         nextCache.ETag,
		LastModified: nextCache.LastModified,
	}); err != nil {
		return added, err
	}
	return added, nil
}

func (e *Engine) pollGitHub(ctx context.Context) (int, error) {
	key := "github:notifications"
	st, err := e.store.GetFeedState(ctx, key)
	if err != nil {
		return 0, err
	}
	items, nextLM, nextCursor, err := ingest.FetchGitHubNotifications(
		ctx, e.httpClient, e.cfg.Sources.GitHub.Token, e.cfg.Sources.GitHub.ParticipatingOnly,
		st.LastModified, st.ETag, e.cfg.Storage.MaxSnippetChars,
	)
	if err != nil {
		return 0, err
	}
	added, err := e.ingestBatch(ctx, items)
	if err != nil {
		return added, err
	}
	if nextLM != st.LastModified || nextCursor != st.ETag {
		if err := e.store.SaveFeedState(ctx, store.FeedSyncState{
			SourceKey:    key,
			ETag:         nextCursor,
			LastModified: nextLM,
		}); err != nil {
			return added, err
		}
	}
	return added, nil
}

func (e *Engine) pollSingleIMAP(ctx context.Context, src config.IMAPConfig) (int, error) {
	key := "imap:" + hashKey(fmt.Sprintf("%s:%d|%s|%s", src.Host, src.Port, src.Username, src.Mailbox))
	st, err := e.store.GetFeedState(ctx, key)
	if err != nil {
		return 0, err
	}
	var prevUIDValidity uint32
	if st.ETag != "" {
		if v, convErr := strconv.ParseUint(st.ETag, 10, 32); convErr == nil {
			prevUIDValidity = uint32(v)
		}
	}

	items, nextUID, uidValidity, err := ingest.FetchIMAPUnread(ctx, src, st.LastUID, prevUIDValidity, e.cfg.Storage.MaxSnippetChars)
	if err != nil {
		return 0, err
	}

	added := 0
	var succeededUID uint32
	if prevUIDValidity == 0 || uidValidity == prevUIDValidity {
		succeededUID = st.LastUID
	}
	var batchErr error
	for _, it := range items {
		_, inserted, procErr := e.ProcessItem(ctx, it)
		if procErr != nil {
			batchErr = fmt.Errorf("process imap item (%s): %w", it.Title, procErr)
			break
		}
		if inserted {
			added++
		}
		if rawUID := it.Metadata["uid"]; rawUID != "" {
			if u, convErr := strconv.ParseUint(rawUID, 10, 32); convErr == nil && uint32(u) > succeededUID {
				succeededUID = uint32(u)
			}
		}
	}

	persistedUID := succeededUID
	if batchErr == nil && nextUID > persistedUID {
		persistedUID = nextUID
	}
	nextValidityStr := st.ETag
	if uidValidity != 0 {
		nextValidityStr = fmt.Sprintf("%d", uidValidity)
	}
	if persistedUID != st.LastUID || nextValidityStr != st.ETag {
		if saveErr := e.store.SaveFeedState(ctx, store.FeedSyncState{
			SourceKey: key,
			ETag:      nextValidityStr,
			LastUID:   persistedUID,
		}); saveErr != nil {
			if batchErr != nil {
				return added, fmt.Errorf("%w (also save cursor: %v)", batchErr, saveErr)
			}
			return added, saveErr
		}
	}
	return added, batchErr
}

func (e *Engine) ingestBatch(ctx context.Context, items []ingest.IncomingItem) (int, error) {
	added := 0
	for _, it := range items {
		_, inserted, err := e.ProcessItem(ctx, it)
		if err != nil {
			return added, fmt.Errorf("process item (%s): %w", it.Title, err)
		}
		if inserted {
			added++
		}
	}
	return added, nil
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
			e.mu.Unlock()
			if !alreadyRan {
				if n, err := e.FlushDigest(ctx); err != nil {
					log.Printf("scheduled digest error: %v", err)
				} else {
					e.mu.Lock()
					e.lastDigestSlot = slot
					e.mu.Unlock()
					if n > 0 {
						log.Printf("sent scheduled digest (%d items)", n)
					}
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
