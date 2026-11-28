package assessment

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	errToken           = errors.New("invalid Neon access token")
	errKeysUnavailable = errors.New("Neon signing keys unavailable")
)

type verifier struct {
	issuer, jwks                string
	client                      *http.Client
	mu                          sync.Mutex
	keys                        map[string]ed25519.PublicKey
	expires, refreshed, retryAt time.Time
	fetching                    bool
	now                         func() time.Time
}

func newVerifier(raw string) (*verifier, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || u.Path == "" || u.Path == "/" {
		return nil, errors.New("invalid Neon Auth HTTPS base URL")
	}
	host := strings.ToLower(u.Host)
	if u.Port() == "443" {
		host = strings.TrimSuffix(host, ":443")
	}
	return &verifier{issuer: u.Scheme + "://" + host, now: time.Now, jwks: u.JoinPath(".well-known/jwks.json").String(), client: &http.Client{
		Timeout: 2 * time.Second, CheckRedirect: noRedirect,
	}}, nil
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func decodePart(encoded string, output any) error {
	part, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return err
	}
	return json.Unmarshal(part, output)
}

func (v *verifier) verify(ctx context.Context, token string) (string, error) {
	if len(token) > 8192 {
		return "", errToken
	}
	p := strings.Split(token, ".")
	if len(p) != 3 {
		return "", errToken
	}
	var header struct{ Alg, Kid string }
	if decodePart(p[0], &header) != nil || header.Alg != "EdDSA" || header.Kid == "" || len(header.Kid) > 128 {
		return "", errToken
	}
	var claims struct {
		Iss string
		Sub string
		Aud json.RawMessage
		Exp int64
		Nbf int64
	}
	if decodePart(p[1], &claims) != nil {
		return "", errToken
	}
	now := v.now()
	var audience string
	if json.Unmarshal(claims.Aud, &audience) != nil {
		var audiences []string
		if json.Unmarshal(claims.Aud, &audiences) != nil {
			return "", errToken
		}
		for _, a := range audiences {
			if a == v.issuer {
				audience = a
				break
			}
		}
	}
	if claims.Iss != v.issuer || audience != v.issuer || claims.Sub == "" || len(claims.Sub) > 256 || strings.ContainsRune(claims.Sub, 0) ||
		claims.Exp <= now.Unix() || claims.Exp > now.Add(20*time.Minute).Unix() || claims.Nbf > now.Unix() {
		return "", errToken
	}
	signature, err := base64.RawURLEncoding.DecodeString(p[2])
	if err != nil || len(signature) != ed25519.SignatureSize {
		return "", errToken
	}
	key, err := v.key(ctx, header.Kid)
	if err != nil {
		return "", err
	}
	if !ed25519.Verify(key, []byte(p[0]+"."+p[1]), signature) || claims.Exp <= v.now().Unix() {
		return "", errToken
	}
	return claims.Iss + "\x00" + claims.Sub, nil
}

func (v *verifier) key(ctx context.Context, id string) (ed25519.PublicKey, error) {
	v.mu.Lock()
	now := v.now()
	if key := v.keys[id]; key != nil && now.Before(v.expires) {
		v.mu.Unlock()
		return key, nil
	}
	if v.fetching || now.Before(v.retryAt) || now.Sub(v.refreshed) < 30*time.Second {
		v.mu.Unlock()
		return nil, errKeysUnavailable
	}
	v.fetching = true
	v.mu.Unlock()

	// A cache miss may wait for the network. Requests with a known key can
	// still verify while this refresh is in flight.
	keys, err := v.fetchKeys(ctx)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.fetching = false
	now = v.now()
	if err != nil {
		v.retryAt = now.Add(2 * time.Second)
		return nil, errKeysUnavailable
	}
	v.refreshed = now
	v.expires = now.Add(5 * time.Minute)
	v.retryAt = time.Time{}
	v.keys = keys
	if key := keys[id]; key != nil {
		return key, nil
	}
	return nil, errToken
}

func (v *verifier) fetchKeys(ctx context.Context) (map[string]ed25519.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwks, nil)
	if err != nil {
		return nil, err
	}
	res, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, errKeysUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, (16<<10)+1))
	if err != nil || len(raw) > 16<<10 {
		return nil, errKeysUnavailable
	}
	var set struct {
		Keys []struct{ Kid, Kty, Crv, Alg, X, Use string }
	}
	if json.Unmarshal(raw, &set) != nil {
		return nil, errKeysUnavailable
	}
	keys := make(map[string]ed25519.PublicKey)
	for _, k := range set.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" || (k.Alg != "" && k.Alg != "EdDSA") || (k.Use != "" && k.Use != "sig") || k.Kid == "" || len(k.Kid) > 128 {
			continue
		}
		kb, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(kb) != ed25519.PublicKeySize {
			continue
		}
		if _, exists := keys[k.Kid]; exists {
			return nil, errKeysUnavailable
		}
		keys[k.Kid] = ed25519.PublicKey(kb)
	}
	if len(keys) == 0 {
		return nil, errKeysUnavailable
	}
	return keys, nil
}
