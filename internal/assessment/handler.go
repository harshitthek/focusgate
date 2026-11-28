package assessment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"focusgate/internal/classify"
	"focusgate/internal/config"
	"focusgate/internal/ingest"
	"focusgate/internal/store"
)

type provider interface {
	Evaluate(context.Context, string, config.FocusMode, ingest.IncomingItem) (classify.SmartSignals, error)
}

type Handler struct {
	verify             *verifier
	provider           provider
	store              *store.Store
	slot               chan struct{}
	global, perAccount int
	usesJev            bool
	cacheScope         string
}

func New(mode, authURL, modelURL string, cfg config.ClassifierConfig, st *store.Store) (*Handler, error) {
	if mode == "" || mode == "disabled" {
		return nil, nil
	}
	auth, err := newVerifier(authURL)
	if err != nil {
		return nil, err
	}
	h := &Handler{verify: auth, store: st, slot: make(chan struct{}, 1), global: cfg.MaxDailyAPICalls, perAccount: cfg.MaxDailyAPICallsPerUser}
	switch mode {
	case "jev":
		u, err := url.Parse(cfg.TypeSafeBaseURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
			return nil, errors.New("Jev assessment endpoint must use HTTPS")
		}
		h.cacheScope = mode + ":" + strings.TrimRight(u.String(), "/")
		cfg.TimeoutSeconds = 3
		p := classify.NewTypeSafeClient(cfg)
		if !p.Configured() {
			return nil, errors.New("Jev assessment provider needs a TypeSafe API key")
		}
		h.provider = p
		h.usesJev = true
	case "local-model":
		u, err := url.Parse(modelURL)
		if err != nil || u.Scheme != "http" || !net.ParseIP(u.Hostname()).IsLoopback() || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
			return nil, errors.New("local model URL must use a loopback IP and HTTP")
		}
		h.cacheScope = mode + ":" + u.String()
		h.provider = &localModel{url: u.String(), client: &http.Client{
			Transport: &http.Transport{MaxIdleConns: 1, MaxResponseHeaderBytes: 8 << 10},
			Timeout:   3 * time.Second, CheckRedirect: noRedirect,
		}}
	default:
		return nil, errors.New("invalid mobile assessment provider")
	}
	return h, nil
}

type request struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Snippet string `json:"snippet"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case h.slot <- struct{}{}:
		defer func() { <-h.slot }()
	default:
		http.Error(w, "assessment busy", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(3 * time.Second)); err == nil {
		defer http.NewResponseController(w).SetReadDeadline(time.Time{})
	}
	w.Header().Set("Cache-Control", "no-store")
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	account, err := h.verify.verify(ctx, token)
	if errors.Is(err, errKeysUnavailable) {
		http.Error(w, "authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req request
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 12<<10))
	if err == nil {
		err = json.Unmarshal(raw, &req)
	}
	id, decodeErr := hex.DecodeString(req.ID)
	if err != nil || decodeErr != nil || len(id) != 32 || strings.ToLower(req.ID) != req.ID ||
		utf8.RuneCountInString(req.Title) > 512 || utf8.RuneCountInString(req.Snippet) > 900 {
		http.Error(w, "invalid assessment request", http.StatusBadRequest)
		return
	}
	snapshot, _ := json.Marshal(req)
	digest := sha256.Sum256(snapshot)
	fingerprint := hex.EncodeToString(digest[:])
	now := time.Now().UTC()
	cached, lease, err := h.store.ReserveAssessment(ctx, account, req.ID, fingerprint, h.cacheScope, now, h.global, h.perAccount, h.usesJev)
	switch {
	case errors.Is(err, store.ErrAssessmentChanged):
		http.Error(w, "assessment ID already used", http.StatusConflict)
		return
	case err != nil:
		http.Error(w, "assessment unavailable", http.StatusServiceUnavailable)
		return
	case cached != "":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, cached)
		return
	case lease == "":
		http.Error(w, "assessment unavailable", http.StatusTooManyRequests)
		return
	}
	finished := false
	defer func() {
		if !finished {
			cleanup, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			_ = h.store.FinishAssessment(cleanup, account, req.ID, lease, "")
		}
	}()
	item := ingest.IncomingItem{SourceType: "rss", Title: req.Title, Body: req.Snippet}
	sig, err := h.provider.Evaluate(ctx, "A phone user sorting updates from RSS and Atom feeds.", config.ModeNormal, item)
	if err != nil || !sig.ValidAssessment() {
		http.Error(w, "assessment provider unavailable", http.StatusServiceUnavailable)
		return
	}
	output, err := json.Marshal(sig)
	if err != nil || h.store.FinishAssessment(ctx, account, req.ID, lease, string(output)) != nil {
		http.Error(w, "assessment unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	finished = true
	_, _ = w.Write(output)
}

type localModel struct {
	url    string
	client *http.Client
}

func (m *localModel) Evaluate(ctx context.Context, _ string, _ config.FocusMode, item ingest.IncomingItem) (classify.SmartSignals, error) {
	raw, err := json.Marshal(map[string]string{"title": item.Title, "snippet": item.Body})
	if err != nil {
		return classify.SmartSignals{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.url, bytes.NewReader(raw))
	if err != nil {
		return classify.SmartSignals{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return classify.SmartSignals{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return classify.SmartSignals{}, errors.New("model request failed")
	}
	result, err := io.ReadAll(io.LimitReader(resp.Body, 8193))
	if err != nil || len(result) > 8192 {
		return classify.SmartSignals{}, errors.New("invalid model response")
	}
	return classify.DecodeAssessment(result, "local-model")
}
