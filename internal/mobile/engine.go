package mobile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
	UserID       int64     `json:"user_id,omitempty"`
	AuthToken    string    `json:"auth_token"`
	AutoOffline  bool      `json:"auto_offline"`
	PollMinutes  int       `json:"poll_minutes"`
	LastSyncTime time.Time `json:"last_sync_time"`
}

// Engine coordinates the local SQLite store, the offline ingest worker, and cloud sync.
type Engine struct {
	mu            sync.RWMutex
	app           fyne.App
	cfg           MobileConfig
	cfgPath       string
	store         *store.Store
	router        *classify.Router
	status        SyncStatus
	httpClient    *http.Client
	cancelPoll    context.CancelFunc
	onStateChg    func()
	accountGen    uint64
	notifying     bool
	pendingNotify bool
	syncMu        sync.Mutex
	requestMu     sync.Mutex
	pollChanged   chan struct{}
}

const (
	defaultServerURL = "https://sandoxed.xyz"
	maxResponseBytes = 64 << 10
	maxInboxBytes    = 4 << 20
)

type accountSnapshot struct {
	MobileConfig
	generation uint64
}

func (e *Engine) account() accountSnapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return accountSnapshot{MobileConfig: e.cfg, generation: e.accountGen}
}

func (e *Engine) accountIsCurrent(account accountSnapshot) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return account.generation == e.accountGen
}

func (e *Engine) inboxRows(ctx context.Context) (accountSnapshot, config.FocusMode, []store.StoredItem, error) {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	account := e.account()
	mode, err := e.store.GetFocusMode(ctx, config.ModeNormal)
	if err != nil {
		return account, mode, nil, err
	}
	items, err := e.store.ListItems(ctx, store.ItemFilter{Limit: 50})
	return account, mode, items, err
}

func normalizeServerURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = defaultServerURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.ForceQuery || u.RawQuery != "" || strings.Contains(raw, "#") {
		return "", errors.New("server URL must be an HTTP or HTTPS base URL without credentials, a query, or a fragment")
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return "", errors.New("server URL port is invalid")
		}
		port = strconv.Itoa(value)
	}
	if u.Scheme == "http" {
		if host == "localhost" {
			host = "127.0.0.1"
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return "", errors.New("remote server connections require HTTPS")
		}
	}
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func readMobileJSON(r io.Reader, limit int64, dst any) error {
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) > limit {
		return errors.New("server response is too large")
	}
	return json.Unmarshal(raw, dst)
}

func mobileCacheOwner(cfg MobileConfig) string {
	base, _ := normalizeServerURL(cfg.ServerURL)
	return mobileOwnerForBase(cfg, base)
}

func legacyMobileCacheOwner(cfg MobileConfig) string {
	base := strings.TrimSpace(cfg.ServerURL)
	if base == "" {
		base = defaultServerURL
	}
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.ForceQuery || u.RawQuery != "" || strings.Contains(base, "#") {
		base = ""
	}
	return mobileOwnerForBase(cfg, strings.TrimRight(base, "/"))
}

func mobileOwnerForBase(cfg MobileConfig, base string) string {
	u, _ := url.Parse(base)
	if u != nil {
		u.Scheme = strings.ToLower(u.Scheme)
		u.Host = strings.ToLower(u.Host)
		base = u.String()
	}
	identity := fmt.Sprintf("id:%d", cfg.UserID)
	if cfg.UserID <= 0 {
		identity = "token:" + cfg.AuthToken
	}
	digest := sha256.Sum256([]byte(base + "|" + identity))
	return hex.EncodeToString(digest[:])
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

	if err := os.MkdirAll(baseDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create storage directory: %w", err)
	}

	if err := os.Chmod(baseDir, 0700); err != nil {
		return nil, fmt.Errorf("restrict mobile directory: %w", err)
	}

	dbPath := filepath.Join(baseDir, "focusgate.db")
	cfgPath := filepath.Join(baseDir, "mobile-config.json")

	st, err := store.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open local sqlite db at %s: %w", dbPath, err)
	}

	if err := st.InitMobileCache(context.Background()); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("initialize mobile cache: %w", err)
	}
	cfg, err := loadMobileConfig(cfgPath)
	if errors.Is(err, os.ErrNotExist) {
		// A fresh installation has no config. A missing credential file on an
		// existing installation must never authorize erasing an owned inbox.
		owner, lookupErr := st.GetSetting(context.Background(), "mobile_cache_owner")
		switch {
		case errors.Is(lookupErr, sql.ErrNoRows), lookupErr == nil && owner == mobileCacheOwner(cfg):
			err = nil
		case lookupErr != nil:
			err = lookupErr
		}
	}
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("read mobile configuration: %w", err)
	}
	if err := st.ActivateMobileAccount(context.Background(), mobileCacheOwner(cfg), legacyMobileCacheOwner(cfg)); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("activate mobile account: %w", err)
	}

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
		app:         fyneApp,
		cfg:         cfg,
		cfgPath:     cfgPath,
		store:       st,
		router:      router,
		status:      StatusOnline,
		pollChanged: make(chan struct{}, 1),
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}

	return e, nil
}

func loadMobileConfig(path string) (MobileConfig, error) {
	def := MobileConfig{
		ServerURL:   defaultServerURL,
		AuthToken:   "",
		AutoOffline: true,
		PollMinutes: 30,
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return def, err
	}
	var loaded *MobileConfig
	if err := json.Unmarshal(data, &loaded); err != nil {
		return def, err
	}
	if loaded == nil {
		return def, errors.New("empty mobile configuration")
	}
	if loaded.ServerURL == "" {
		loaded.ServerURL = def.ServerURL
	}
	if _, err := normalizeServerURL(loaded.ServerURL); err != nil {
		return def, err
	}
	if loaded.PollMinutes <= 0 {
		loaded.PollMinutes = 30
	}
	loaded.PollMinutes = min(loaded.PollMinutes, 1440)
	return *loaded, nil
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
	return e.saveConfig(e.cfg)
}

// saveConfig replaces a private config file without exposing a partial token.
func (e *Engine) saveConfig(cfg MobileConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(e.cfgPath), ".mobile-config-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return replaceConfigFile(tmp.Name(), e.cfgPath)
}

func (e *Engine) SetOnStateChange(fn func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onStateChg = fn
}

func (e *Engine) notifyChange() {
	e.mu.Lock()
	if e.notifying {
		e.pendingNotify = true
		e.mu.Unlock()
		return
	}
	e.notifying = true
	for {
		fn := e.onStateChg
		e.pendingNotify = false
		e.mu.Unlock()
		if fn != nil {
			fn()
		}
		e.mu.Lock()
		if !e.pendingNotify {
			e.notifying = false
			e.mu.Unlock()
			return
		}
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
	left, leftErr := normalizeServerURL(a)
	right, rightErr := normalizeServerURL(b)
	return leftErr == nil && rightErr == nil && left == right
}

func (e *Engine) applyAccountTransition(ctx context.Context, serverURL, username, token string, autoOffline bool, pollMinutes int) error {
	err := e.applyVerifiedAccount(ctx, serverURL, username, token, 0, autoOffline, pollMinutes)
	if err == nil {
		e.notifyChange()
	}
	return err
}

func (e *Engine) applyVerifiedAccount(ctx context.Context, serverURL, username, token string, userID int64, autoOffline bool, pollMinutes int) error {
	return e.applyVerifiedAccountFrom(ctx, e.account(), serverURL, username, token, userID, autoOffline, pollMinutes)
}

func (e *Engine) applyVerifiedAccountFrom(ctx context.Context, old accountSnapshot, serverURL, username, token string, userID int64, autoOffline bool, pollMinutes int) error {
	serverURL, err := normalizeServerURL(serverURL)
	if err != nil {
		return err
	}
	username = strings.TrimSpace(username)
	token = strings.TrimSpace(token)
	if pollMinutes <= 0 {
		pollMinutes = 30
	}
	verifiedLegacyOwner := false
	if userID > 0 && old.UserID == 0 && old.AuthToken != "" && sameServerURL(old.ServerURL, serverURL) {
		// Legacy settings allow a manually entered username and token to disagree.
		// Bind the old cache to the authenticated identity behind that token.
		if old.AuthToken == token {
			verifiedLegacyOwner = true
		} else {
			var identity struct {
				User struct {
					ID int64 `json:"id"`
				} `json:"user"`
			}
			err := e.serverJSON(ctx, old, http.MethodGet, "/api/auth/me", nil, maxResponseBytes, &identity)
			if err != nil && !errors.Is(err, serverStatus(http.StatusUnauthorized)) && !errors.Is(err, serverStatus(http.StatusForbidden)) {
				return fmt.Errorf("verify previous cache owner: %w", err)
			}
			if err == nil && identity.User.ID <= 0 {
				return errors.New("server did not identify the previous cache owner")
			}
			verifiedLegacyOwner = err == nil && identity.User.ID == userID
		}
	}
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if old.generation != e.accountGen {
		return errAccountChanged
	}
	previous := e.cfg
	sameOrigin := sameServerURL(previous.ServerURL, serverURL)
	sameIdentity := verifiedLegacyOwner || (sameOrigin && userID > 0 && previous.UserID == userID)
	if userID == 0 && sameOrigin && previous.AuthToken == token &&
		strings.EqualFold(previous.Username, username) {
		userID = previous.UserID
		sameIdentity = true
	}
	staged := previous
	staged.ServerURL, staged.Username, staged.AuthToken = serverURL, username, token
	staged.UserID = userID
	staged.AutoOffline, staged.PollMinutes = autoOffline, min(pollMinutes, 1440)
	changed := !sameIdentity
	if changed {
		staged.LastSyncTime = time.Time{}
	}
	var equivalent []string
	if sameIdentity {
		equivalent = append(equivalent, mobileCacheOwner(previous), legacyMobileCacheOwner(previous))
		if err := e.store.PrepareMobileOwnerTransition(ctx, mobileCacheOwner(staged), equivalent...); err != nil {
			return fmt.Errorf("prepare cache owner: %w", err)
		}
	}
	if err := e.saveConfig(staged); err != nil {
		return fmt.Errorf("persist config: %w", err)
	}
	if err := e.store.ActivateMobileAccount(ctx, mobileCacheOwner(staged), equivalent...); err != nil {
		// Restore old credentials on disk before returning to the old identity.
		if rollbackErr := e.saveConfig(previous); rollbackErr != nil {
			// The old cache is still active. Startup will reconcile the on-disk
			// configuration before making that identity or its inbox available.
			return fmt.Errorf("cache reset failed: %v; config rollback failed: %w", err, rollbackErr)
		}
		return fmt.Errorf("reset account cache: %w", err)
	}
	e.cfg = staged
	if previous.PollMinutes != staged.PollMinutes {
		select {
		case e.pollChanged <- struct{}{}:
		default:
		}
	}
	if changed || previous.AuthToken != token {
		e.accountGen++
		e.status = StatusOffline
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
	account := e.account()
	_, err := e.loginWithServer(ctx, account, serverURL, username, password, account.AutoOffline, account.PollMinutes)
	return err
}

func (e *Engine) loginWithServer(ctx context.Context, account accountSnapshot, serverURL, username, password string, autoOffline bool, pollMinutes int) (accountSnapshot, error) {
	serverURL, err := normalizeServerURL(serverURL)
	if err != nil {
		return accountSnapshot{}, err
	}
	loginURL := serverURL + "/api/auth/login"

	payload, err := json.Marshal(map[string]string{
		"username":   username,
		"password":   password,
		"passphrase": password,
	})
	if err != nil {
		return accountSnapshot{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loginURL, bytes.NewReader(payload))
	if err != nil {
		return accountSnapshot{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return accountSnapshot{}, fmt.Errorf("connection error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp map[string]string
		_ = readMobileJSON(resp.Body, maxResponseBytes, &errResp)
		if msg, ok := errResp["error"]; ok {
			return accountSnapshot{}, errors.New(msg)
		}
		return accountSnapshot{}, fmt.Errorf("login failed with status %d", resp.StatusCode)
	}

	var authResp struct {
		Token string `json:"token"`
		User  struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
		} `json:"user"`
	}
	if err := readMobileJSON(resp.Body, maxResponseBytes, &authResp); err != nil {
		return accountSnapshot{}, fmt.Errorf("decode auth response: %w", err)
	}
	if strings.TrimSpace(authResp.Token) == "" {
		return accountSnapshot{}, errors.New("empty auth token received from server")
	}
	finalUsername := authResp.User.Username
	if finalUsername == "" {
		finalUsername = username
	}

	if err := e.applyVerifiedAccountFrom(ctx, account, serverURL, finalUsername, authResp.Token, authResp.User.ID, autoOffline, pollMinutes); err != nil {
		return accountSnapshot{}, err
	}

	committed := e.account()
	if committed.AuthToken != strings.TrimSpace(authResp.Token) || !sameServerURL(committed.ServerURL, serverURL) {
		return accountSnapshot{}, errAccountChanged
	}
	e.notifyChange()
	_ = e.SyncWithServer(ctx)
	return committed, nil
}

// ClassifyOnline evaluates an item online using the VPS TypeSafe Jev System One / smart router,
// falling back cleanly to the local heuristic rules router if offline or unreachable.
func (e *Engine) ClassifyOnline(ctx context.Context, item ingest.IncomingItem) classify.Decision {
	account := e.account()
	if decision, err := e.classifyOnServer(ctx, account, item); err == nil && e.accountIsCurrent(account) {
		return decision
	}
	mode, _ := e.store.GetFocusMode(ctx, config.ModeNormal)
	return e.router.Evaluate(ctx, mode, item)
}

func (e *Engine) classifyOnServer(ctx context.Context, account accountSnapshot, item ingest.IncomingItem) (classify.Decision, error) {
	var response struct {
		classify.Decision
		Wrapped classify.Decision `json:"decision"`
	}
	if account.AuthToken == "" {
		return classify.Decision{}, errors.New("no server session")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err := e.serverJSON(ctx, account, http.MethodPost, "/api/classify", item, maxResponseBytes, &response)
	if err != nil {
		return classify.Decision{}, err
	}
	if response.Route != "" {
		return response.Decision, nil
	}
	if response.Wrapped.Route != "" {
		return response.Wrapped, nil
	}
	return classify.Decision{}, errors.New("server returned an empty classification")
}

// StartBackgroundLoop initiates scheduled syncing and cellular direct polling.
func (e *Engine) StartBackgroundLoop(ctx context.Context) {
	pollCtx, cancel := context.WithCancel(ctx)
	e.mu.Lock()
	e.cancelPoll = cancel
	e.mu.Unlock()

	go func() {
		for pollCtx.Err() == nil {
			err := e.SyncWithServer(pollCtx)
			cfg := e.Config()
			if err != nil && pollCtx.Err() == nil && cfg.AutoOffline {
				e.DirectCellularFetch(pollCtx)
			}
			timer := time.NewTimer(time.Duration(cfg.PollMinutes) * time.Minute)
		wait:
			for {
				select {
				case <-pollCtx.Done():
					timer.Stop()
					return
				case <-timer.C:
					break wait
				case <-e.pollChanged:
					timer.Reset(time.Duration(e.Config().PollMinutes) * time.Minute)
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
	if _, err := e.store.PruneOld(ctx, 30); err != nil {
		log.Printf("prune mobile cache: %v", err)
	}
	e.notifyChange()
}

// SyncWithServer refreshes the newest inbox page for the active account.
func (e *Engine) SyncWithServer(ctx context.Context) error {
	account := e.account()
	e.requestMu.Lock()
	notify := false
	defer func() {
		e.requestMu.Unlock()
		if notify {
			e.notifyChange()
		}
	}()
	if !e.accountIsCurrent(account) {
		return nil
	}
	notify = true
	e.setSyncResult(account, StatusSyncing)
	err := e.syncInbox(ctx, account)
	if err != nil {
		if !e.accountIsCurrent(account) {
			return nil
		}
		e.setSyncResult(account, StatusOffline)
		return fmt.Errorf("server sync failed: %w", err)
	}
	e.setSyncResult(account, StatusOnline)
	return nil
}

func (e *Engine) syncInbox(ctx context.Context, account accountSnapshot) error {
	var page struct {
		Items []store.StoredItem `json:"items"`
		Mode  config.FocusMode   `json:"mode"`
	}
	if err := e.serverJSON(ctx, account, http.MethodGet, "/api/items?limit=50", nil, maxInboxBytes, &page); err != nil {
		return err
	}
	if page.Items == nil {
		return errors.New("server response did not contain an inbox")
	}
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	if !e.accountIsCurrent(account) {
		return nil
	}
	if err := e.store.ImportMobileItems(ctx, page.Items); err != nil {
		return err
	}
	if config.ValidFocusMode(page.Mode) {
		if err := e.store.SetFocusMode(ctx, page.Mode); err != nil {
			return err
		}
	}
	_, err := e.store.PruneOld(ctx, 30)
	return err
}

func (e *Engine) setSyncResult(account accountSnapshot, status SyncStatus) {
	e.mu.Lock()
	if e.accountGen != account.generation {
		e.mu.Unlock()
		return
	}
	e.status = status
	if status == StatusOnline {
		e.cfg.LastSyncTime = time.Now().UTC()
	}
	e.mu.Unlock()
}

type serverStatus int

func (s serverStatus) Error() string { return fmt.Sprintf("server returned HTTP %d", int(s)) }

func (e *Engine) serverJSON(ctx context.Context, account accountSnapshot, method, path string, payload any, limit int64, dst any) error {
	base, err := normalizeServerURL(account.ServerURL)
	if err != nil {
		return err
	}
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return err
	}
	if account.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+account.AuthToken)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return serverStatus(resp.StatusCode)
	}
	if dst == nil {
		return nil
	}
	return readMobileJSON(resp.Body, limit, dst)
}

// MarkRead archives local items and uses the stored server ID for synced items.
func (e *Engine) MarkRead(ctx context.Context, id int64) error {
	return e.markRead(ctx, e.account(), id)
}

var errAccountChanged = errors.New("active account changed")

func (e *Engine) markRead(ctx context.Context, account accountSnapshot, id int64) (err error) {
	e.requestMu.Lock()
	defer func() {
		e.requestMu.Unlock()
		if err == nil {
			e.notifyChange()
		}
	}()
	e.syncMu.Lock()
	if !e.accountIsCurrent(account) {
		e.syncMu.Unlock()
		return errAccountChanged
	}
	serverID, err := e.store.MobileServerID(ctx, id)
	e.syncMu.Unlock()
	if err != nil {
		return err
	}
	if serverID > 0 {
		if !e.accountIsCurrent(account) {
			return errAccountChanged
		}
		path := fmt.Sprintf("/api/items/%d/status", serverID)
		if err := e.serverJSON(ctx, account, http.MethodPost, path, map[string]string{"status": "archived"}, 0, nil); err != nil && !errors.Is(err, serverStatus(http.StatusNotFound)) {
			return err
		} else if errors.Is(err, serverStatus(http.StatusNotFound)) {
			e.syncMu.Lock()
			if !e.accountIsCurrent(account) {
				e.syncMu.Unlock()
				return errAccountChanged
			}
			err = e.store.ForgetMobileServerID(ctx, id)
			e.syncMu.Unlock()
			if err != nil {
				return err
			}
		}
	}
	e.syncMu.Lock()
	if !e.accountIsCurrent(account) {
		e.syncMu.Unlock()
		return errAccountChanged
	}
	err = e.store.UpdateItemStatus(ctx, id, "archived")
	e.syncMu.Unlock()
	return err
}

func (e *Engine) setFocusMode(ctx context.Context, account accountSnapshot, mode config.FocusMode) error {
	e.requestMu.Lock()
	changed := false
	defer func() {
		e.requestMu.Unlock()
		if changed {
			e.notifyChange()
		}
	}()
	if !e.accountIsCurrent(account) {
		return errAccountChanged
	}
	var remoteErr error
	if account.AuthToken != "" {
		remoteErr = e.serverJSON(ctx, account, http.MethodPost, "/api/mode", map[string]config.FocusMode{"mode": mode}, 0, nil)
		var networkErr *url.Error
		if remoteErr != nil && !errors.As(remoteErr, &networkErr) {
			return remoteErr
		}
	}
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	if !e.accountIsCurrent(account) {
		return errAccountChanged
	}
	if err := e.store.SetFocusMode(ctx, mode); err != nil {
		return err
	}
	changed = true
	return remoteErr
}
