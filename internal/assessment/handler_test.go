package assessment

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	key, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	calls := 0
	h := Handler{verify: auth, store: st, slot: make(chan struct{}, 1), global: 2, perAccount: 1, provider: evaluator(
		func(_ context.Context, _ string, _ config.FocusMode, _ ingest.IncomingItem) (classify.SmartSignals, error) {
			calls++
			return classify.SmartSignals{Engine: "local-model", Model: "revision1", Category: "work_or_dev_update"}, nil
		})}
	token := func(sub, aud string, exp time.Time) string {
		base := func(v any) string { raw, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(raw) }
		body := base(map[string]string{"alg": "EdDSA", "kid": "key"}) + "." + base(map[string]any{
			"iss": server.URL, "sub": sub, "aud": aud, "exp": exp.Unix(),
		})
		return body + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(body)))
	}
	ask := func(token, id, title string) int {
		raw, _ := json.Marshal(request{ID: strings.Repeat(id, 64), Title: title})
		req := httptest.NewRequest(http.MethodPost, "/v1/assessments", strings.NewReader(string(raw)))
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	valid := token("one", server.URL, time.Now().Add(time.Minute))
	other := token("two", server.URL, time.Now().Add(time.Minute))
	for _, tc := range []struct {
		token, id, title string
		status           int
	}{
		{token("one", "https://another-project.invalid", time.Now().Add(time.Minute)), "a", "title", 401},
		{token("one", server.URL, time.Now().Add(-time.Second)), "a", "title", 401},
		{valid, "a", "title", 200},
		{valid, "a", "title", 200},
		{valid, "a", "changed", 409},
		{valid, "b", "title", 429},
		{other, "a", "title", 200},
		{token("three", server.URL, time.Now().Add(time.Minute)), "c", "title", 429},
	} {
		if got := ask(tc.token, tc.id, tc.title); got != tc.status {
			t.Fatalf("id %s: HTTP %d, want %d", tc.id, got, tc.status)
		}
	}
	if calls != 2 {
		t.Fatalf("provider called %d times, want 2", calls)
	}
}
