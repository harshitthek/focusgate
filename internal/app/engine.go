// Package app coordinates feed ingestion, classification routing, and dispatch cycles.
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

	scheduledMu       sync.Mutex
	lastDigestCheck   time.Time
	pendingDigestSlot string
	digestRetryAt     time.Time
	digestMu          sync.Mutex
	processGate       chan struct{}
	modeMu            sync.Mutex

	subMu       sync.RWMutex
	subscribers map[chan []byte]int64
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
	d := notify.NewDispatcher(cfg)
	d.SetDeliveryStore(st)
	if st != nil {
		ns, err := st.GetNotifySettings(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to load notify settings: %w", err)
		}
		merged := mergeNotifySettings(cfg.Notify, ns)
		d.UpdateNotifyConfig(merged)
	}

	return &Engine{
		cfg:         cfg,
		store:       st,
		router:      r,
		dispatcher:  d,
		httpClient:  &http.Client{Timeout: 15 * time.Second},
		mode:        mode,
		subscribers: make(map[chan []byte]int64),
		processGate: make(chan struct{}, 1),
	}, nil
}

// Config exposes the active runtime configuration.
func (e *Engine) Config() config.Config { return e.cfg }

// Store exposes the underlying SQLite store.
func (e *Engine) Store() *store.Store { return e.store }

// Dispatcher returns the engine notification dispatcher.
func (e *Engine) Dispatcher() *notify.Dispatcher { return e.dispatcher }

// ReloadNotifySettings loads the latest notification settings from SQLite and updates the dispatcher.
func (e *Engine) ReloadNotifySettings(ctx context.Context) (config.NotifyConfig, error) {
	merged := e.cfg.Notify
	if e.store != nil {
		ns, err := e.store.GetNotifySettings(ctx)
		if err != nil {
			return merged, err
		}
		merged = mergeNotifySettings(e.cfg.Notify, ns)
	}
	e.dispatcher.UpdateNotifyConfig(merged)
	return merged, nil
}

// mergeNotifySettings overlays SQLite settings overrides onto base notification config.
func mergeNotifySettings(base config.NotifyConfig, ns store.NotifySettings) config.NotifyConfig {
	merged := base
	if ns.HasKey != nil {
		if ns.HasKey["notify_slack_enabled"] {
			merged.Slack.Enabled = ns.SlackEnabled
		}
		if ns.HasKey["notify_slack_url"] {
			merged.Slack.WebhookURL = ns.SlackWebhookURL
		}
		if ns.HasKey["notify_slack_mention"] {
			merged.Slack.MentionHandle = ns.SlackMention
		}
		if ns.HasKey["notify_matrix_enabled"] {
			merged.Matrix.Enabled = ns.MatrixEnabled
		}
		if ns.HasKey["notify_matrix_url"] {
			merged.Matrix.WebhookURL = ns.MatrixWebhookURL
		}
		if ns.HasKey["notify_matrix_mention"] {
			merged.Matrix.MentionHandle = ns.MatrixMention
		}
		if ns.HasKey["notify_only_mentions"] {
			merged.OnlyMentions = ns.OnlyMentions
		}
		if ns.HasKey["notify_mention_handles"] {
			merged.MentionHandles = notify.ParseMentionHandles(ns.MentionHandles)
		}
	}
	return merged
}

// HTTPClient exposes the engine HTTP client.
func (e *Engine) HTTPClient() *http.Client { return e.httpClient }

// TriggerGitHubSync initiates an immediate polling pass for GitHub notifications.
func (e *Engine) TriggerGitHubSync(ctx context.Context) (int, error) {
	return e.pollGitHub(ctx)
}

// Mode returns the current FocusMode safely, refreshing from SQLite so
// out-of-process CLI mode changes take effect immediately.
func (e *Engine) Mode() config.FocusMode {
	return e.operatorMode(context.Background())
}

func (e *Engine) operatorMode(ctx context.Context) config.FocusMode {
	e.modeMu.Lock()
	defer e.modeMu.Unlock()
	if m, err := e.store.GetFocusMode(ctx, e.cfg.Focus.DefaultMode); err == nil {
		e.mu.Lock()
		e.mode = m
		e.mu.Unlock()
		return m
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mode
}

// ModeForUser returns the account's current focus mode.
func (e *Engine) ModeForUser(ctx context.Context, userID int64) config.FocusMode {
	if userID <= 1 {
		return e.operatorMode(ctx)
	}
	mode, err := e.store.GetUserSetting(ctx, userID, "focus_mode")
	if err == nil && config.ValidFocusMode(config.FocusMode(mode)) {
		return config.FocusMode(mode)
	}
	return e.cfg.Focus.DefaultMode
}

// SetMode persists the operator's focus mode, including changes made by the CLI.
func (e *Engine) SetMode(ctx context.Context, mode config.FocusMode) error {
	return e.SetUserMode(ctx, 1, mode)
}

// SetUserMode persists and broadcasts a focus mode for exactly one account.
func (e *Engine) SetUserMode(ctx context.Context, userID int64, mode config.FocusMode) error {
	e.modeMu.Lock()
	defer e.modeMu.Unlock()
	if !config.ValidFocusMode(mode) {
		return fmt.Errorf("invalid focus mode %q", mode)
	}
	if userID <= 1 {
		userID = 1
		if err := e.store.SetFocusMode(ctx, mode); err != nil {
			return err
		}
		e.mu.Lock()
		e.mode = mode
		e.mu.Unlock()
	} else if err := e.store.SetUserSetting(ctx, userID, "focus_mode", string(mode)); err != nil {
		return err
	}
	e.broadcast(userID, "mode", map[string]string{"mode": string(mode)})
	return nil
}

// SourceSummary describes a configured ingestion source without exposing secrets.
type SourceSummary struct {
	ID      int64  `json:"id,omitempty"`
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

// Status returns operator telemetry and queue counts.
func (e *Engine) Status(ctx context.Context) (RuntimeStatus, error) {
	return e.StatusUser(ctx, 1)
}

// StatusUser returns the queue, focus settings, and sources for an account.
func (e *Engine) StatusUser(ctx context.Context, userID int64) (RuntimeStatus, error) {
	userID = max(1, userID)
	stats, err := e.store.GetUserStats(ctx, userID, e.ModeForUser(ctx, userID))
	if err != nil {
		return RuntimeStatus{}, err
	}
	st := RuntimeStatus{
		Stats:      stats,
		Classifier: "heuristic (rules_only)",
		Sources:    []SourceSummary{},
	}
	if e.cfg.Classifier.Mode == "hybrid" {
		if e.cfg.Classifier.TypeSafeAPIKey != "" {
			st.Classifier = "hybrid (rules + TypeSafe Jev)"
		} else {
			st.Classifier = "hybrid (rules + heuristic; set TYPESAFE_API_KEY)"
		}
	}
	if userID == 1 {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		st.HeapMB = float64(m.HeapAlloc) / (1024 * 1024)
		st.SysMB = float64(m.Sys) / (1024 * 1024)
		st.DBSizeKB = float64(e.store.DBFileSizeBytes(e.cfg.Storage.Path)) / 1024.0
		st.NtfyEnabled = e.cfg.Notify.Ntfy.Enabled
		st.DesktopEnabled = e.cfg.Notify.DesktopWindows.Enabled
		st.Sources = e.summarizeSources(ctx)
		st.DigestTimes = append([]string(nil), e.cfg.Focus.DigestTimes...)
		if nextTime, nextLabel, ok := NextDigestSlot(time.Now(), e.cfg.Focus.DigestTimes); ok {
			st.NextDigestAt = nextTime.Format(time.RFC3339)
			st.NextDigestHHMM = nextLabel
		}
	}
	st.APIUsedToday, st.APIDailyLimit = e.router.DailyUserAPIUsage(ctx, userID)
	feeds, err := e.store.ListUserFeeds(ctx, userID)
	if err != nil {
		return RuntimeStatus{}, err
	}
	for _, feed := range feeds {
		st.Sources = append(st.Sources, SourceSummary{
			ID: feed.ID, Kind: "rss", Name: feed.Name, Enabled: true, Detail: feed.URL,
		})
	}
	return st, nil
}

func (e *Engine) resolveGitHubConfig(ctx context.Context) (token string, participating bool, enabled bool) {
	state, err := e.store.GetGitHubCredentialState(ctx)
	if err != nil || state.Disabled {
		return "", false, false
	}
	if strings.TrimSpace(state.Token) != "" {
		return strings.TrimSpace(state.Token), state.ParticipatingOnly, true
	}
	if e.cfg.Sources.GitHub.Enabled && strings.TrimSpace(e.cfg.Sources.GitHub.Token) != "" {
		return strings.TrimSpace(e.cfg.Sources.GitHub.Token), e.cfg.Sources.GitHub.ParticipatingOnly, true
	}
	return "", false, false
}

func (e *Engine) resolveEmailConfig(ctx context.Context) (cfg config.IMAPConfig, enabled bool) {
	if dis, err := e.store.IsEmailDisabled(ctx); err == nil && dis {
		return config.IMAPConfig{}, false
	}
	dbCreds, err := e.store.GetEmailCredentials(ctx)
	if err == nil && dbCreds.Enabled && strings.TrimSpace(dbCreds.Username) != "" && strings.TrimSpace(dbCreds.Password) != "" {
		pollMin := dbCreds.PollMinutes
		if pollMin <= 0 {
			pollMin = 5
		}
		port := dbCreds.Port
		if port <= 0 {
			port = 993
		}
		mailbox := dbCreds.Mailbox
		if mailbox == "" {
			mailbox = "INBOX"
		}
		name := dbCreds.Provider
		if name == "" {
			name = "email"
		}
		return config.IMAPConfig{
			Name:        name,
			Enabled:     true,
			Host:        dbCreds.Host,
			Port:        port,
			Username:    dbCreds.Username,
			Password:    dbCreds.Password,
			Mailbox:     mailbox,
			PollMinutes: pollMin,
		}, true
	}
	return config.IMAPConfig{}, false
}

// TriggerEmailSync initiates an immediate polling pass for email notifications.
func (e *Engine) TriggerEmailSync(ctx context.Context) (int, error) {
	cfg, enabled := e.resolveEmailConfig(ctx)
	if !enabled {
		return 0, nil
	}
	return e.pollSingleIMAP(ctx, cfg)
}

func (e *Engine) summarizeSources(ctx context.Context) []SourceSummary {
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
	_, _, ghEnabled := e.resolveGitHubConfig(ctx)
	if ghEnabled {
		pollMin := max(1, e.cfg.Sources.GitHub.PollMinutes)
		ghDetail = fmt.Sprintf("Every %dm", pollMin)
		if _, userJSON, _, err := e.store.GetGitHubCredentials(ctx); err == nil && strings.TrimSpace(userJSON) != "" {
			var u struct {
				Login string `json:"login"`
			}
			if json.Unmarshal([]byte(userJSON), &u) == nil && u.Login != "" {
				ghDetail = fmt.Sprintf("@%s · %dm", u.Login, pollMin)
			}
		}
	}
	out = append(out, SourceSummary{
		Kind:    "github",
		Name:    "GitHub Notifications",
		Enabled: ghEnabled,
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
	emailCfg, emailEnabled := e.resolveEmailConfig(ctx)
	if emailEnabled {
		out = append(out, SourceSummary{
			Kind:    "email",
			Name:    "Email (" + strings.ToUpper(emailCfg.Name) + ")",
			Enabled: true,
			Detail:  fmt.Sprintf("%s · %dm", emailCfg.Username, max(1, emailCfg.PollMinutes)),
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
	slackDetail := "Disabled"
	if e.cfg.Sources.Slack.Enabled {
		slackDetail = "POST /api/slack/events · mentions"
	}
	out = append(out, SourceSummary{
		Kind:    "slack",
		Name:    "Slack Mentions",
		Enabled: e.cfg.Sources.Slack.Enabled,
		Detail:  slackDetail,
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
	return e.BulkUpdateUserStatus(ctx, 1, ids, fromStatus, toStatus)
}

// BulkUpdateUserStatus updates multiple items scoped to a specific user and broadcasts refreshed stats over SSE.
func (e *Engine) BulkUpdateUserStatus(ctx context.Context, userID int64, ids []int64, fromStatus, toStatus string) (int64, error) {
	if userID <= 0 {
		userID = 1
	}
	n, err := e.store.BulkUpdateUserStatus(ctx, userID, ids, fromStatus, toStatus)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		e.broadcastStats(ctx, userID)
	}
	return n, nil
}

// SendTestAlert dispatches a test alert to enabled notification channels.
func (e *Engine) SendTestAlert(ctx context.Context) ([]string, error) {
	return e.dispatcher.SendTestAlert(ctx)
}

// ProcessItem deduplicates, classifies, applies the active focus mode, stores, and dispatches an item.
func (e *Engine) ProcessItem(ctx context.Context, item ingest.IncomingItem) (store.StoredItem, bool, error) {
	// Serialize ingestion to bound semantic work and deduplicate before reserving quota.
	select {
	case e.processGate <- struct{}{}:
		defer func() { <-e.processGate }()
	case <-ctx.Done():
		return store.StoredItem{}, false, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return store.StoredItem{}, false, err
	}
	if item.UserID <= 0 {
		item.UserID = 1
	}
	item.Normalize(e.cfg.Storage.MaxSnippetChars)
	exists, err := e.store.ReconcileIncomingItem(ctx, item.UserID, item)
	if err != nil {
		return store.StoredItem{}, false, err
	}
	if exists {
		return store.StoredItem{}, false, nil
	}

	targetUID := item.UserID
	initialMode := e.ModeForUser(ctx, targetUID)
	classifyCtx := ctx
	if deadline, ok := ctx.Deadline(); ok {
		// Keep time to persist a heuristic fallback if the semantic call times out.
		var cancel context.CancelFunc
		classifyCtx, cancel = context.WithDeadline(ctx, deadline.Add(-2*time.Second))
		defer cancel()
	}
	match, sig := e.router.Classify(classifyCtx, initialMode, item)
	stored, inserted, err := e.store.InsertPendingItem(ctx, targetUID, item, e.cfg.Focus.DefaultMode, func(mode config.FocusMode) classify.Decision {
		return e.router.Route(mode, match, sig)
	})
	if err != nil || !inserted {
		if stored.UserID == 0 && targetUID > 0 {
			stored.UserID = targetUID
		}
		return stored, inserted, err
	}
	if stored.UserID == 0 {
		stored.UserID = targetUID
	}

	if stored.Route == "push_now" {
		if stored.UserID <= 1 {
			pushErr := e.dispatcher.PushItem(ctx, stored)
			finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			updated, err := e.store.FinishInitialPush(finishCtx, stored.ID, pushErr == nil)
			cancel()
			if err != nil {
				return stored, true, err
			}
			if pushErr != nil {
				log.Printf("notify error (%s): %v", stored.Title, pushErr)
			} else if updated {
				stored.Status = "pushed"
			}
		} else {
			// Non-admin users have no server-wide webhook/ntfy dispatcher;
			// InsertUserItem atomically persists status = "queued" for non-admin push_now items.
			stored.Status = "queued"
		}
	}

	e.broadcast(targetUID, "item", stored)
	e.broadcastStats(ctx, targetUID)
	return stored, true, nil
}

// SimulateItem classifies a synthetic or pasted item without persisting or sending notifications.
func (e *Engine) SimulateItem(ctx context.Context, mode config.FocusMode, item ingest.IncomingItem) classify.Decision {
	if !config.ValidFocusMode(mode) {
		mode = e.ModeForUser(ctx, item.UserID)
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
		// Server-configured notification destinations belong to user 1/administrator.
		// Query user 1's queued items directly in the database so other users' items cannot crowd them out.
		queued, err := e.store.ListUserQueuedForDigest(ctx, 1, batchSize)
		if err != nil || len(queued) == 0 {
			if totalFlushed > 0 {
				e.broadcast(1, "digest", map[string]int{"count": totalFlushed})
				e.broadcastStats(ctx, 1)
			}
			return totalFlushed, err
		}

		delivered, sendErr := e.dispatcher.SendDigest(ctx, queued)
		if sendErr != nil {
			if !delivered {
				if totalFlushed > 0 {
					e.broadcast(1, "digest", map[string]int{"count": totalFlushed})
					e.broadcastStats(ctx, 1)
				}
				return totalFlushed, sendErr
			}
			log.Printf("digest partial delivery warning: %v", sendErr)
		}

		var allDigestedIDs []int64
		for _, it := range queued {
			allDigestedIDs = append(allDigestedIDs, it.ID)
		}

		if len(allDigestedIDs) == 0 {
			break
		}

		if err := e.store.MarkDigested(ctx, allDigestedIDs); err != nil {
			if totalFlushed > 0 {
				e.broadcast(1, "digest", map[string]int{"count": totalFlushed})
				e.broadcastStats(ctx, 1)
			}
			return totalFlushed, err
		}
		totalFlushed += len(allDigestedIDs)
		if len(queued) < batchSize {
			break
		}
	}
	e.broadcast(1, "digest", map[string]int{"count": totalFlushed})
	e.broadcastStats(ctx, 1)
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
	_, _, ghEnabled := e.resolveGitHubConfig(ctx)
	if ghEnabled {
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
	emailCfg, emailEnabled := e.resolveEmailConfig(ctx)
	if emailEnabled {
		n, err := e.pollSingleIMAP(ctx, emailCfg)
		totalNew += n
		if err != nil {
			log.Printf("email poll (%s): %v", emailCfg.Name, err)
			pollErrors = append(pollErrors, fmt.Errorf("email (%s): %w", emailCfg.Name, err))
		}
	}
	userFeeds, err := e.store.ListUserFeeds(ctx, 1)
	if err != nil {
		log.Printf("list operator feeds: %v", err)
		pollErrors = append(pollErrors, fmt.Errorf("list operator feeds: %w", err))
	} else {
		for _, uf := range userFeeds {
			n, err := e.pollUserFeed(ctx, uf)
			totalNew += n
			if err != nil {
				log.Printf("user feed poll (%s, user %d): %v", uf.Name, uf.UserID, err)
				pollErrors = append(pollErrors, fmt.Errorf("user feed (%s): %w", uf.Name, err))
			}
		}
	}
	return totalNew, errors.Join(pollErrors...)
}

func (e *Engine) pollUserFeed(ctx context.Context, uf store.UserFeed) (int, error) {
	key := fmt.Sprintf("uf:%d:%s", uf.ID, hashKey(uf.URL))
	st, err := e.store.GetFeedState(ctx, key)
	if err != nil {
		return 0, err
	}
	items, nextCache, changed, err := ingest.FetchFeedRestricted(
		ctx, e.httpClient, uf.Name, uf.URL, "rss",
		ingest.FeedCache{ETag: st.ETag, LastModified: st.LastModified},
		e.cfg.Storage.MaxSnippetChars,
	)
	if err != nil || !changed {
		return 0, err
	}
	if exists, err := e.store.HasUserFeed(ctx, uf.UserID, uf.ID); err != nil || !exists {
		return 0, err
	}
	for i := range items {
		items[i].UserID = uf.UserID
		items[i].UserFeedID = uf.ID
		if items[i].LegacyExternalID != "" {
			items[i].LegacyExternalID = fmt.Sprintf("uf:%d:%s", uf.ID, items[i].LegacyExternalID)
		}
		items[i].ExternalID = fmt.Sprintf("uf:%d:%s", uf.ID, items[i].ExternalID)
	}
	ingestCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	added, err := e.ingestBatch(ingestCtx, items)
	if err != nil {
		return added, err
	}
	if err := e.store.SaveFeedState(ctx, store.FeedSyncState{
		SourceKey:    key,
		UserFeedID:   uf.ID,
		ETag:         nextCache.ETag,
		LastModified: nextCache.LastModified,
	}); err != nil {
		return added, err
	}
	return added, nil
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
	token, participating, enabled := e.resolveGitHubConfig(ctx)
	if !enabled || token == "" {
		return 0, nil
	}
	key := "github:notifications:" + hashKey(token+fmt.Sprint(participating))
	st, err := e.store.GetFeedState(ctx, key)
	if err != nil {
		return 0, err
	}
	items, nextLM, nextCursor, err := ingest.FetchGitHubNotifications(
		ctx, e.httpClient, token, participating,
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
	pollMinutes := e.cfg.Sources.GitHub.PollMinutes
	if pollMinutes <= 0 {
		pollMinutes = 5
	}
	go e.runIntervalLoop(ctx, time.Duration(pollMinutes)*time.Minute, func() {
		if _, err := e.pollGitHub(ctx); err != nil {
			log.Printf("github: %v", err)
		}
	})
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

	// Dynamic email poller: polls at the user-configured interval (default 5m)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		var lastPoll time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				cfg, enabled := e.resolveEmailConfig(ctx)
				if !enabled {
					continue
				}
				pollMin := max(1, cfg.PollMinutes)
				if lastPoll.IsZero() || now.Sub(lastPoll) >= time.Duration(pollMin)*time.Minute {
					lastPoll = now
					if _, err := e.pollSingleIMAP(ctx, cfg); err != nil {
						log.Printf("email poll (%s): %v", cfg.Name, err)
					}
				}
			}
		}
	}()

	// One worker polls a bounded set of due feeds per tick.
	go e.runIntervalLoop(ctx, 30*time.Second, func() {
		now := time.Now()
		feeds, err := e.store.ListDueUserFeeds(ctx, now, 4)
		if err != nil {
			log.Printf("list due user feeds: %v", err)
			return
		}
		for _, feed := range feeds {
			scheduled, err := e.store.ScheduleUserFeed(ctx, feed, now)
			if err != nil {
				log.Printf("schedule feed %d: %v", feed.ID, err)
				continue
			}
			if !scheduled {
				continue
			}
			_, err = e.pollUserFeed(ctx, feed)
			if err != nil {
				log.Printf("poll feed %d (user %d): %v", feed.ID, feed.UserID, err)
			}
		}
	})

	// Scheduled digest checker & daily retention pruner
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		e.maybeRunDigest(ctx, time.Now())
		var lastPruneDay int
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				e.maybeRunDigest(ctx, now)
				if now.Day() != lastPruneDay {
					lastPruneDay = now.Day()
					if err := e.store.PruneSessions(ctx); err != nil {
						log.Printf("prune sessions: %v", err)
					}
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
	e.scheduledMu.Lock()
	defer e.scheduledMu.Unlock()
	if e.lastDigestCheck.IsZero() {
		e.lastDigestCheck = now.Truncate(time.Minute).Add(-time.Nanosecond)
	}
	if now.After(e.lastDigestCheck) {
		// One flush covers every due item, including slots missed during sleep.
		due, _, ok := NextDigestSlot(e.lastDigestCheck, e.cfg.Focus.DigestTimes)
		if ok && !due.After(now) && e.pendingDigestSlot == "" {
			slot := due.Format("2006-01-02 15:04")
			if e.lastDigestSlot != slot {
				e.pendingDigestSlot = slot
			}
		}
		e.lastDigestCheck = now
	}
	if e.pendingDigestSlot == "" || now.Before(e.digestRetryAt) {
		return
	}
	if n, err := e.FlushDigest(ctx); err != nil {
		e.digestRetryAt = now.Add(time.Minute)
		log.Printf("scheduled digest error: %v", err)
	} else {
		e.lastDigestSlot = e.pendingDigestSlot
		e.pendingDigestSlot = ""
		e.digestRetryAt = time.Time{}
		if n > 0 {
			log.Printf("sent scheduled digest (%d items)", n)
		}
	}
}

// Subscribe registers an SSE client channel for default/legacy real-time UI updates (admin user 1).
func (e *Engine) Subscribe() (ch chan []byte, unsubscribe func()) {
	return e.SubscribeUser(1)
}

// SubscribeUser registers an SSE client channel scoped to a specific authenticated user.
func (e *Engine) SubscribeUser(userID int64) (ch chan []byte, unsubscribe func()) {
	if userID <= 0 {
		userID = 1
	}
	ch = make(chan []byte, 16)
	e.subMu.Lock()
	if len(e.subscribers) >= 64 {
		e.subMu.Unlock()
		return nil, nil
	}
	count := 0
	for _, owner := range e.subscribers {
		if owner == userID {
			count++
		}
	}
	if count >= 4 {
		e.subMu.Unlock()
		return nil, nil
	}
	e.subscribers[ch] = userID
	e.subMu.Unlock()
	return ch, func() {
		e.subMu.Lock()
		if _, exists := e.subscribers[ch]; exists {
			delete(e.subscribers, ch)
			close(ch)
		}
		e.subMu.Unlock()
	}
}

// DisconnectUser closes an account's streams after its sessions are revoked.
func (e *Engine) DisconnectUser(userID int64) {
	e.subMu.Lock()
	defer e.subMu.Unlock()
	for ch, owner := range e.subscribers {
		if owner == userID {
			delete(e.subscribers, ch)
			close(ch)
		}
	}
}

// HasSubscriber reports whether a stream is still registered to receive events.
func (e *Engine) HasSubscriber(ch chan []byte) bool {
	e.subMu.RLock()
	defer e.subMu.RUnlock()
	_, exists := e.subscribers[ch]
	return exists
}

func (e *Engine) broadcast(userID int64, eventType string, data any) {
	raw, err := json.Marshal(Event{Type: eventType, Data: data})
	if err != nil {
		return
	}
	frame := []byte("event: " + eventType + "\ndata: " + string(raw) + "\n\n")
	e.subMu.RLock()
	defer e.subMu.RUnlock()
	for ch, owner := range e.subscribers {
		if owner != userID {
			continue
		}
		select {
		case ch <- frame:
		default:
			// Drop slow client frames rather than blocking ingestion or accumulating memory.
		}
	}
}

func (e *Engine) broadcastStats(ctx context.Context, userID int64) {
	e.subMu.RLock()
	hasSubscribers := false
	for _, owner := range e.subscribers {
		if owner == userID {
			hasSubscribers = true
			break
		}
	}
	e.subMu.RUnlock()
	if hasSubscribers {
		if status, err := e.StatusUser(ctx, userID); err == nil {
			e.broadcast(userID, "status", status)
		}
	}
}

func hashKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
