package mobile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"focusgate/internal/classify"
	"focusgate/internal/config"
	"focusgate/internal/ingest"
	"focusgate/internal/store"

	"fyne.io/fyne/v2"
)

// SyncStatus represents the current connectivity and sync state.
type SyncStatus string

const (
	StatusOnline  SyncStatus = "ONLINE · SYNCED"
	StatusOffline SyncStatus = "OFFLINE · DIRECT CELLULAR"
	StatusSyncing SyncStatus = "SYNCING..."
)

// MobileConfig holds user settings for the mobile application.
type MobileConfig struct {
	ServerURL    string    `json:"server_url"`
	AuthToken    string    `json:"auth_token"`
	AutoOffline  bool      `json:"auto_offline"`
	PollMinutes  int       `json:"poll_minutes"`
	LastSyncTime time.Time `json:"last_sync_time"`
}

// Engine coordinates the local SQLite store, the offline ingest worker, and cloud sync.
type Engine struct {
	mu         sync.RWMutex
	app        fyne.App
	cfg        MobileConfig
	cfgPath    string
	store      *store.Store
	router     *classify.Router
	status     SyncStatus
	httpClient *http.Client
	cancelPoll context.CancelFunc
	onStateChg func()
}

// NewEngine initializes the local FocusGate mobile engine.
func NewEngine(fyneApp fyne.App) (*Engine, error) {
	return NewEngineWithDir(fyneApp, "")
}

// NewEngineWithDir initializes the mobile engine with a custom base directory.
func NewEngineWithDir(fyneApp fyne.App, customDir string) (*Engine, error) {
	baseDir := customDir
	if baseDir == "" {
		storageRoot := fyneApp.Storage().RootURI()
		if storageRoot != nil && storageRoot.Path() != "" {
			baseDir = storageRoot.Path()
		} else {
			userConfig, _ := os.UserConfigDir()
			baseDir = filepath.Join(userConfig, "focusgate-mobile")
		}
	}

	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create storage directory: %w", err)
	}

	dbPath := filepath.Join(baseDir, "focusgate.db")
	cfgPath := filepath.Join(baseDir, "mobile-config.json")

	st, err := store.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open local sqlite db at %s: %w", dbPath, err)
	}

	cfg := loadMobileConfig(cfgPath)

	coreCfg := config.Config{
		Focus: config.FocusConfig{
			DefaultMode: config.ModeNormal,
			UserContext: "Mobile user triaging notifications and feeds on the go.",
		},
		Rules: []config.RuleConfig{
			{
				Name:       "Tech Feeds Always Digest",
				SourceType: "rss",
				Action:     "queue_digest",
			},
			{
				Name:     "Security & Outage Keywords",
				Regex:    "(?i)\\b(outage|sev1|incident|cve-|security alert|production down)\\b",
				Action:   "push_now",
				Priority: 5,
			},
			{
				Name:         "GitHub Direct Review or Mention",
				SourceType:   "github",
				GitHubReason: "review_requested,mention,assign,security_alert",
				Action:       "smart",
			},
		},
	}

	router := classify.NewRouter(coreCfg)

	e := &Engine{
		app:        fyneApp,
		cfg:        cfg,
		cfgPath:    cfgPath,
		store:      st,
		router:     router,
		status:     StatusOnline,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}

	return e, nil
}

func loadMobileConfig(path string) MobileConfig {
	def := MobileConfig{
		ServerURL:   "https://sandoxed.xyz",
		AuthToken:   "7d98a4ae9da947f992339b212d34ca02",
		AutoOffline: true,
		PollMinutes: 30,
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return def
	}
	var loaded MobileConfig
	if err := json.Unmarshal(data, &loaded); err != nil {
		return def
	}
	if loaded.ServerURL == "" {
		loaded.ServerURL = def.ServerURL
	}
	if loaded.AuthToken == "" {
		loaded.AuthToken = def.AuthToken
	}
	if loaded.PollMinutes <= 0 {
		loaded.PollMinutes = 30
	}
	return loaded
}

// Close terminates background pollers and closes the SQLite database.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cancelPoll != nil {
		e.cancelPoll()
	}
	if e.store != nil {
		return e.store.Close()
	}
	return nil
}

func (e *Engine) SaveConfig() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	data, err := json.MarshalIndent(e.cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(e.cfgPath, data, 0644)
}

func (e *Engine) SetOnStateChange(fn func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onStateChg = fn
}

func (e *Engine) notifyChange() {
	e.mu.RLock()
	fn := e.onStateChg
	e.mu.RUnlock()
	if fn != nil {
		fn()
	}
}

func (e *Engine) Store() *store.Store {
	return e.store
}

func (e *Engine) Status() SyncStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.status
}

func (e *Engine) Config() MobileConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.cfg
}

func (e *Engine) UpdateConfig(serverURL, token string, autoOffline bool, pollMinutes int) error {
	e.mu.Lock()
	e.cfg.ServerURL = strings.TrimRight(strings.TrimSpace(serverURL), "/")
	e.cfg.AuthToken = strings.TrimSpace(token)
	e.cfg.AutoOffline = autoOffline
	e.cfg.PollMinutes = pollMinutes
	e.mu.Unlock()

	return e.SaveConfig()
}

// StartBackgroundLoop initiates scheduled syncing and cellular direct polling.
func (e *Engine) StartBackgroundLoop(ctx context.Context) {
	pollCtx, cancel := context.WithCancel(ctx)
	e.mu.Lock()
	e.cancelPoll = cancel
	e.mu.Unlock()

	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()

		_ = e.SyncWithServer(pollCtx)

		for {
			select {
			case <-pollCtx.Done():
				return
			case <-ticker.C:
				err := e.SyncWithServer(pollCtx)
				if err != nil && e.cfg.AutoOffline {
					e.mu.Lock()
					e.status = StatusOffline
					e.mu.Unlock()
					e.notifyChange()

					e.DirectCellularFetch(pollCtx)
				}
			}
		}
	}()
}

// DirectCellularFetch pulls RSS feeds directly using device mobile data when offline.
func (e *Engine) DirectCellularFetch(ctx context.Context) {
	log.Println("[Mobile Engine] Running Direct Cellular Fetch (Offline Mode)...")

	sources := []struct {
		Name string
		URL  string
		Tag  string
	}{
		{"Go Blog", "https://go.dev/blog/feed.atom", "dev"},
		{"Lobsters", "https://lobste.rs/rss", "tech"},
	}

	mode, _ := e.store.GetFocusMode(ctx, config.ModeNormal)

	for _, src := range sources {
		rawItems, _, _, err := ingest.FetchFeed(ctx, e.httpClient, src.Name, src.URL, src.Tag, ingest.FeedCache{}, 1200)
		if err != nil {
			log.Printf("[Mobile Engine] Failed to fetch feed %s: %v", src.Name, err)
			continue
		}

		for _, item := range rawItems {
			exists, err := e.store.Exists(ctx, item.SourceType, item.ExternalID)
			if err != nil || exists {
				continue
			}

			// Route using local FocusGate rule engine
			dec := e.router.Evaluate(ctx, mode, item)

			stored, inserted, err := e.store.InsertItem(ctx, item, dec)
			if err != nil || !inserted {
				continue
			}

			// Fire native Android heads-up notification for urgent alerts
			if stored.Route == "push_now" {
				e.app.SendNotification(&fyne.Notification{
					Title:   fmt.Sprintf("🚨 %s", stored.Title),
					Content: stored.Body,
				})
			}
		}
	}

	e.notifyChange()
}

// SyncWithServer synchronizes read items, queues, and digests with the central VPS.
func (e *Engine) SyncWithServer(ctx context.Context) error {
	e.mu.Lock()
	e.status = StatusSyncing
	e.mu.Unlock()
	e.notifyChange()

	serverURL := e.cfg.ServerURL
	if serverURL == "" {
		serverURL = "https://sandoxed.xyz"
	}

	reqURL, err := url.Parse(fmt.Sprintf("%s/api/items", serverURL))
	if err != nil {
		e.mu.Lock()
		e.status = StatusOffline
		e.mu.Unlock()
		e.notifyChange()
		return err
	}

	q := reqURL.Query()
	q.Set("limit", "50")
	reqURL.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if err != nil {
		return err
	}
	if e.cfg.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+e.cfg.AuthToken)
	}

	resp, err := e.httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		e.mu.Lock()
		e.status = StatusOffline
		e.mu.Unlock()
		e.notifyChange()
		return fmt.Errorf("server sync failed: %v", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	var itemsResp struct {
		Items []store.StoredItem `json:"items"`
	}
	if err := json.Unmarshal(bodyBytes, &itemsResp); err != nil {
		return err
	}

	mode, _ := e.store.GetFocusMode(ctx, config.ModeNormal)
	for _, item := range itemsResp.Items {
		exists, _ := e.store.Exists(ctx, item.SourceType, item.ExternalID)
		if !exists {
			inc := ingest.IncomingItem{
				SourceType:  item.SourceType,
				SourceName:  item.SourceName,
				ExternalID:  item.ExternalID,
				Title:       item.Title,
				Body:        item.Body,
				Sender:      item.Sender,
				URL:         item.URL,
				Tag:         item.Tag,
				PublishedAt: item.PublishedAt,
			}
			dec := e.router.Evaluate(ctx, mode, inc)
			dec.Route = item.Route
			dec.Priority = item.Priority
			dec.Source = "server_sync"
			_, _, _ = e.store.InsertItem(ctx, inc, dec)
		}
	}

	e.mu.Lock()
	e.status = StatusOnline
	e.cfg.LastSyncTime = time.Now().UTC()
	e.mu.Unlock()
	e.notifyChange()

	return nil
}

// MarkRead marks an item read locally and propagates update to server.
func (e *Engine) MarkRead(ctx context.Context, id int64) error {
	if err := e.store.UpdateItemStatus(ctx, id, "archived"); err != nil {
		return err
	}

	// Propagate to server in background
	go func() {
		serverURL := e.Config().ServerURL
		if serverURL == "" || e.Config().AuthToken == "" {
			return
		}
		patchURL := fmt.Sprintf("%s/api/items/%d/archive", serverURL, id)
		req, _ := http.NewRequest(http.MethodPost, patchURL, bytes.NewBuffer([]byte("{}")))
		req.Header.Set("Authorization", "Bearer "+e.Config().AuthToken)
		_, _ = e.httpClient.Do(req)
	}()

	e.notifyChange()
	return nil
}
