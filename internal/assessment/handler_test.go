package assessment

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"focusgate/internal/classify"
	"focusgate/internal/config"
	"focusgate/internal/ingest"
	"focusgate/internal/store"
)

type evaluator func(context.Context, string, config.FocusMode, ingest.IncomingItem) (classify.SmartSignals, error)

func (f evaluator) Evaluate(c context.Context, s string, m config.FocusMode, i ingest.IncomingItem) (classify.SmartSignals, error) {
	return f(c, s, m, i)
}

func TestVerifiedAssessmentQuotaAndReplay(t *testing.T) {
	var unavailable atomic.Bool
	var expireOnLookup atomic.Bool
	key, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if unavailable.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "key", "kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "x": base64.RawURLEncoding.EncodeToString(key),
		}}})
	}))
	defer server.Close()
	auth, err := newVerifier(server.URL + "/neondb/auth")
	if err != nil {
		t.Fatal(err)
	}
	auth.client = server.Client()
	clock := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	auth.now = func() time.Time {
		if expireOnLookup.Load() && !auth.refreshed.IsZero() {
			return clock.Add(time.Minute)
		}
		return clock
	}
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	calls := 0
	quotaDay := time.Date(2026, 11, 29, 0, 0, 0, 0, time.UTC)
	h := Handler{
		verify: auth, store: st, slot: make(chan struct{}, 1), validationSlots: make(chan struct{}, 4),
		global: 2, perAccount: 1, clock: func() time.Time { return quotaDay },
		provider: evaluator(
			func(_ context.Context, _ string, _ config.FocusMode, _ ingest.IncomingItem) (classify.SmartSignals, error) {
				calls++
				return classify.SmartSignals{Engine: "local-model", Model: "revision1", Category: "work_or_dev_update"}, nil
			})}
	token := func(sub, aud, kid string, exp time.Time) string {
		base := func(v any) string { raw, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(raw) }
		body := base(map[string]string{"alg": "EdDSA", "kid": kid}) + "." + base(map[string]any{
			"iss": server.URL, "sub": sub, "aud": aud, "exp": exp.Unix(),
		})
		return body + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(body)))
	}
	askWithContext := func(ctx context.Context, token, id, title string) int {
		raw, _ := json.Marshal(request{ID: strings.Repeat(id, 64), Title: title})
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/assessments", strings.NewReader(string(raw)))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	ask := func(token, id, title string) int { return askWithContext(context.Background(), token, id, title) }
	valid := token("one", server.URL, "key", clock.Add(time.Minute))
	unavailable.Store(true)
	if got := ask(valid, "a", "title"); got != 503 {
		t.Fatalf("JWKS outage returned %d, want 503", got)
	}
	unavailable.Store(false)
	if got := ask(valid, "a", "title"); got != 503 {
		t.Fatalf("JWKS retry backoff returned %d, want 503", got)
	}
	clock = clock.Add(3 * time.Second)
	expireOnLookup.Store(true)
	if got := ask(valid, "a", "title"); got != 401 {
		t.Fatalf("token expired during JWKS lookup returned %d, want 401", got)
	}
	expireOnLookup.Store(false)
	other := token("two", server.URL, "key", clock.Add(time.Minute))
	rotated := token("one", server.URL, "new-key", clock.Add(time.Minute))
	if got := ask(rotated, "a", "title"); got != http.StatusServiceUnavailable {
		t.Fatalf("key miss during refresh cooldown returned %d, want 503", got)
	}
	h.slot <- struct{}{}
	for _, tc := range []struct {
		token, id string
		status    int
	}{
		{"", "a", http.StatusUnauthorized},
		{valid, "", http.StatusBadRequest},
		{valid, "a", http.StatusServiceUnavailable},
	} {
		if got := ask(tc.token, tc.id, "title"); got != tc.status {
			t.Fatalf("while provider slot is busy, request returned %d, want %d", got, tc.status)
		}
	}
	<-h.slot
	for _, tc := range []struct {
		token, id, title string
		status           int
	}{
		{token("one", "https://another-project.invalid", "key", clock.Add(time.Minute)), "a", "title", 401},
		{token("one", server.URL, "key", clock.Add(-time.Second)), "a", "title", 401},
		{valid, "a", "title", 200},
		{valid, "a", "title", 200},
		{valid, "a", "changed", 409},
		{valid, "b", "title", 429},
		{other, "a", "title", 200},
		{token("three", server.URL, "key", clock.Add(time.Minute)), "c", "title", 429},
	} {
		if got := ask(tc.token, tc.id, tc.title); got != tc.status {
			t.Fatalf("id %s: HTTP %d, want %d", tc.id, got, tc.status)
		}
	}
	if calls != 2 {
		t.Fatalf("provider called %d times, want 2", calls)
	}

	h.global, h.perAccount = 8, 8
	h.provider = evaluator(func(context.Context, string, config.FocusMode, ingest.IncomingItem) (classify.SmartSignals, error) {
		return classify.SmartSignals{}, errors.New("provider offline")
	})
	if got := ask(valid, "b", "title"); got != 503 {
		t.Fatalf("provider failure returned %d, want 503", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	successful := classify.SmartSignals{Engine: "local-model", Model: "revision1", Category: "work_or_dev_update"}
	h.provider = evaluator(func(context.Context, string, config.FocusMode, ingest.IncomingItem) (classify.SmartSignals, error) {
		cancel()
		return successful, nil
	})
	if got := askWithContext(ctx, valid, "b", "title"); got != 503 {
		t.Fatalf("cancelled write returned %d, want 503", got)
	}
	h.provider = evaluator(func(context.Context, string, config.FocusMode, ingest.IncomingItem) (classify.SmartSignals, error) {
		calls++
		return successful, nil
	})
	for range 2 {
		if got := ask(valid, "b", "title"); got != 200 {
			t.Fatalf("retry or cached result returned %d, want 200", got)
		}
	}
	h.perAccount = 4
	if got := ask(valid, "c", "title"); got != 429 {
		t.Fatalf("failed attempts were not charged: %d, want 429", got)
	}
	h.perAccount = 8
	h.cacheScope = "local-model:http://127.0.0.1/new"
	if got := ask(valid, "b", "title"); got != 200 || calls != 4 {
		t.Fatalf("model scope change returned %d with %d provider calls", got, calls)
	}
}

func TestJevCacheScopeTracksConfiguredModel(t *testing.T) {
	cfg := config.Default().Classifier
	cfg.TypeSafeAPIKey = "test-key"
	scope := func(model string) string {
		t.Helper()
		cfg.Model = model
		h, err := New("jev", "https://auth.example.com/neondb/auth", "", cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		return h.cacheScope
	}
	first := scope("model-one")
	if first != scope("model-one") || first == scope("model-two") {
		t.Fatal("assessment cache scope must change only when the configured model changes")
	}
}
