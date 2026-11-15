package mobile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	Username     string    `json:"username"`
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
	accountGen uint64
	syncMu     sync.Mutex
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
		AuthToken:   "",
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

func sameServerURL(a, b string) bool {
	if a == b {
		return true
	}
	u1, err1 := url.Parse(a)
	u2, err2 := url.Parse(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return strings.EqualFold(u1.Scheme, u2.Scheme) &&
		strings.EqualFold(u1.Host, u2.Host) &&
		strings.TrimRight(u1.Path, "/") == strings.TrimRight(u2.Path, "/")
}

func (e *Engine) applyAccountTransition(ctx context.Context, serverURL, username, token string, autoOffline bool, pollMinutes int) error {
	var didPurge bool
	err := func() error {
		e.syncMu.Lock()
		defer e.syncMu.Unlock()

		e.mu.Lock()
		prevCfg := e.cfg
		prevServer := prevCfg.ServerURL
		prevUser := prevCfg.Username
		prevToken := prevCfg.AuthToken
		e.mu.Unlock()

		serverURL = strings.TrimRight(strings.TrimSpace(serverURL), "/")
		username = strings.TrimSpace(username)
		token = strings.TrimSpace(token)

		identityChanged := !sameServerURL(prevServer, serverURL) ||
			!strings.EqualFold(prevUser, username) ||
			(prevToken != "" && prevToken != token)

		stagedCfg := prevCfg
		stagedCfg.ServerURL = serverURL
		stagedCfg.Username = username
		stagedCfg.AuthToken = token
		stagedCfg.AutoOffline = autoOffline
		stagedCfg.PollMinutes = pollMinutes

		if identityChanged && (prevServer != "" || prevUser != "" || prevToken != "") {
			if err := e.store.PurgeAllItems(ctx); err != nil {
				return fmt.Errorf("purge cached items for account transition: %w", err)
			}
			didPurge = true
		}

		e.mu.Lock()
		e.cfg = stagedCfg
		if identityChanged {
			e.accountGen++
		}
		e.mu.Unlock()

		if err := e.SaveConfig(); err != nil {
			e.mu.Lock()
			e.cfg = prevCfg
			if identityChanged {
				e.accountGen++
			}
			e.mu.Unlock()
			return fmt.Errorf("persist config: %w", err)
		}
		return nil
	}()

	if err != nil {
		if didPurge {
			e.notifyChange()
		}
		return err
	}

	if didPurge {
		e.notifyChange()
	}
	return nil
}

// UpdateConfig updates connection settings.
func (e *Engine) UpdateConfig(serverURL, token string, autoOffline bool, pollMinutes int) error {
	return e.applyAccountTransition(context.Background(), serverURL, e.Config().Username, token, autoOffline, pollMinutes)
}

// UpdateUserConfig updates server URL, username, and token settings.
func (e *Engine) UpdateUserConfig(serverURL, username, token string, autoOffline bool, pollMinutes int) error {
	return e.applyAccountTransition(context.Background(), serverURL, username, token, autoOffline, pollMinutes)
}

// LoginWithServer authenticates with the VPS server and stores the issued session token.
func (e *Engine) LoginWithServer(ctx context.Context, serverURL, username, password string) error {
	serverURL = strings.TrimRight(strings.TrimSpace(serverURL), "/")
	if serverURL == "" {
		serverURL = "https://sandoxed.xyz"
	}
	loginURL := serverURL + "/api/auth/login"

	payload, err := json.Marshal(map[string]string{
		"username":   username,
		"password":   password,
		"passphrase": password,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loginURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("connection error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		if msg, ok := errResp["error"]; ok {
			return errors.New(msg)
		}
		return fmt.Errorf("login failed with status %d", resp.StatusCode)
	}

	var authResp struct {
		Token string `json:"token"`
		User  struct {
			Username string `json:"username"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&authResp); err != nil {
		return fmt.Errorf("decode auth response: %w", err)
	}
	if strings.TrimSpace(authResp.Token) == "" {
		return errors.New("empty auth token received from server")
	}
	finalUsername := authResp.User.Username
	if finalUsername == "" {
		finalUsername = username
	}

	e.mu.Lock()
	autoOffline := e.cfg.AutoOffline
	pollMinutes := e.cfg.PollMinutes
	e.mu.Unlock()

	if err := e.applyAccountTransition(ctx, serverURL, finalUsername, authResp.Token, autoOffline, pollMinutes); err != nil {
		return err
	}

	_ = e.SyncWithServer(ctx)
	return nil
}

// ClassifyOnline evaluates an item online using the VPS TypeSafe Jev System One / smart router,
// falling back cleanly to the local heuristic rules router if offline or unreachable.
func (e *Engine) ClassifyOnline(ctx context.Context, item ingest.IncomingItem) classify.Decision {
	e.mu.RLock()
	serverURL := e.cfg.ServerURL
	authToken := e.cfg.AuthToken
	e.mu.RUnlock()

	if serverURL != "" && authToken != "" {
		reqURL := strings.TrimRight(serverURL, "/") + "/api/classify"
		raw, err := json.Marshal(item)
		if err == nil {
			timeoutCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(timeoutCtx, http.MethodPost, reqURL, bytes.NewReader(raw))
			if err == nil {
				req.Header.Set("Authorization", "Bearer "+authToken)
				req.Header.Set("Content-Type", "application/json")
				resp, err := e.httpClient.Do(req)
				if err == nil && resp.StatusCode == http.StatusOK {
					defer resp.Body.Close()
					var rawBody bytes.Buffer
					_, _ = rawBody.ReadFrom(resp.Body)
					var dec classify.Decision
					if err := json.Unmarshal(rawBody.Bytes(), &dec); err == nil && dec.Route != "" {
						return dec
					}
					var wrapper struct {
						Decision classify.Decision `json:"decision"`
					}
					if err := json.Unmarshal(rawBody.Bytes(), &wrapper); err == nil && wrapper.Decision.Route != "" {
						return wrapper.Decision
					}
				}
				if err != nil {
					log.Printf("[Mobile Engine] ClassifyOnline HTTP error: %v", err)
				} else if resp != nil {
					if resp.StatusCode != http.StatusOK {
						log.Printf("[Mobile Engine] ClassifyOnline returned status %d from %s", resp.StatusCode, reqURL)
					}
					_ = resp.Body.Close()
				}
			}
		}
	}

	// Offline fallback: evaluate using local embedded heuristic rules
	match, sig := e.router.Classify(ctx, config.ModeNormal, item)
	return e.router.Route(config.ModeNormal, match, sig)
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

	e.mu.Lock()
	curGen := e.accountGen
	e.mu.Unlock()

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
		rawItems, _, _, err := ingest.FetchFeedRestricted(ctx, e.httpClient, src.Name, src.URL, src.Tag, ingest.FeedCache{}, 1200)
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

			e.syncMu.Lock()
			e.mu.Lock()
			stale := e.accountGen != curGen
			e.mu.Unlock()
			if stale {
				e.syncMu.Unlock()
				return
			}

			stored, inserted, err := e.store.InsertItem(ctx, item, dec)
			if err == nil && inserted && stored.Route == "push_now" {
				e.mu.Lock()
				stillActive := e.accountGen == curGen
				e.mu.Unlock()
				if stillActive && e.app != nil {
					e.app.SendNotification(&fyne.Notification{
						Title:   fmt.Sprintf("🚨 %s", stored.Title),
						Content: stored.Body,
					})
				}
			}
			e.syncMu.Unlock()
			if err != nil || !inserted {
				continue
			}
		}
	}

	e.notifyChange()
}

// SyncWithServer synchronizes read items, queues, and digests with the central VPS.
func (e *Engine) SyncWithServer(ctx context.Context) error {
	e.mu.Lock()
	e.status = StatusSyncing
	curGen := e.accountGen
	serverURL := e.cfg.ServerURL
	authToken := e.cfg.AuthToken
	e.mu.Unlock()
	e.notifyChange()

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
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
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

	e.syncMu.Lock()
	e.mu.Lock()
	stale := e.accountGen != curGen
	e.mu.Unlock()
	if stale {
		e.syncMu.Unlock()
		return nil
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
	e.syncMu.Unlock()

	e.mu.Lock()
	if e.accountGen == curGen {
		e.status = StatusOnline
		e.cfg.LastSyncTime = time.Now().UTC()
	}
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
