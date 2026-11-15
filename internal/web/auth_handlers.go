// Package web provides HTTP routing, dashboard handlers, and embedded assets.
package web

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"focusgate/internal/auth"
	"focusgate/internal/config"
	"focusgate/internal/ingest"
	"focusgate/internal/store"
)

const (
	// Default client ID for GitHub OAuth device flow when not overridden.
	defaultGitHubClientID = "Ov23liJ4hM2LwK8f6FG"
	sessionCookieName     = "focusgate_session"
)

type gitHubUserProfile struct {
	Login       string `json:"login"`
	ID          int64  `json:"id"`
	AvatarURL   string `json:"avatar_url"`
	HTMLURL     string `json:"html_url"`
	Name        string `json:"name"`
	Bio         string `json:"bio"`
	PublicRepos int    `json:"public_repos"`
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
}

func (s *Server) extractToken(r *http.Request) string {
	if tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")); tok != "" {
		return tok
	}
	if tok := strings.TrimSpace(r.Header.Get("X-FocusGate-Token")); tok != "" {
		return tok
	}
	if c, err := r.Cookie(sessionCookieName); err == nil && strings.TrimSpace(c.Value) != "" {
		return strings.TrimSpace(c.Value)
	}
	if c, err := r.Cookie("focusgate_token"); err == nil && strings.TrimSpace(c.Value) != "" {
		return strings.TrimSpace(c.Value)
	}
	return ""
}

// handleAuthStatus returns whether the initial passphrase setup is required,
// whether current session is authenticated, and the status of GitHub integration.
func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	setupReq, err := s.engine.Store().IsAuthSetupRequired(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to check auth status"})
		return
	}

	authConfigured := !setupReq || strings.TrimSpace(s.engine.Config().Server.AuthToken) != ""
	user := s.authenticateRequest(r)
	authenticated := user != nil

	resp := map[string]any{
		"setup_required":     setupReq && strings.TrimSpace(s.engine.Config().Server.AuthToken) == "",
		"auth_configured":    authConfigured,
		"authenticated":      authenticated,
		"has_github":         false,
		"github_user":        nil,
		"participating_only": false,
	}
	if user != nil {
		resp["user"] = map[string]any{
			"id":       user.UserID,
			"username": user.Username,
			"role":     user.Role,
		}
		if user.Role == "admin" || user.UserID <= 1 {
			dbTok, userJSON, part, _ := s.engine.Store().GetGitHubCredentials(ctx)
			hasGitHub := strings.TrimSpace(dbTok) != "" || (s.engine.Config().Sources.GitHub.Enabled && strings.TrimSpace(s.engine.Config().Sources.GitHub.Token) != "")
			var ghUser any
			if userJSON != "" {
				_ = json.Unmarshal([]byte(userJSON), &ghUser)
			}
			resp["has_github"] = hasGitHub
			resp["github_user"] = ghUser
			resp["participating_only"] = part
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAuthSetup sets the master passphrase on first launch.
func (s *Server) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req, err := s.engine.Store().IsAuthSetupRequired(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to inspect auth status"})
		return
	}
	if !req {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "vault already initialized, use change-passphrase"})
		return
	}

	var body struct {
		Passphrase string `json:"passphrase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json payload"})
		return
	}

	pass := body.Passphrase
	if len(pass) < 4 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "passphrase must be at least 4 characters"})
		return
	}

	hashB64, saltB64, err := auth.HashPassphrase(pass)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to hash passphrase"})
		return
	}

	if err := s.engine.Store().InitializeMasterPassphrase(ctx, hashB64, saltB64); err != nil {
		if errors.Is(err, store.ErrAlreadyInitialized) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "vault already initialized, use change-passphrase"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to store passphrase"})
		return
	}

	token, err := auth.GenerateSessionToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to generate session"})
		return
	}

	exp := time.Now().UTC().Add(auth.DefaultSessionTTL)
	if err := s.engine.Store().CreateSession(ctx, token, exp); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create session"})
		return
	}

	s.setSessionCookie(w, r, token, auth.DefaultSessionTTL)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "initialized",
		"token":      token,
		"expires_in": int(auth.DefaultSessionTTL.Seconds()),
	})
}

// handleAuthRegister registers a new user account.
func (s *Server) handleAuthRegister(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json payload"})
		return
	}

	username := strings.TrimSpace(body.Username)
	password := body.Password

	// Enforce server-side username format and length rules
	if len(username) < 3 || len(username) > 30 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username must be between 3 and 30 characters"})
		return
	}
	for _, ch := range username {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-') {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username may only contain letters, numbers, underscores, and hyphens"})
			return
		}
	}

	// Enforce server-side password length
	if len(password) < 4 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "password must be at least 4 characters"})
		return
	}

	users, err := s.engine.Store().ListUsers(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to verify user status"})
		return
	}

	authorizedBootstrap := false
	if len(users) == 0 {
		setupReq, err := s.engine.Store().IsAuthSetupRequired(ctx)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to check setup status"})
			return
		}
		expectedToken := strings.TrimSpace(s.engine.Config().Server.AuthToken)
		if !setupReq {
			// Legacy master passphrase exists: first-user admin bootstrap requires authenticating
			// with either the master passphrase or configured server token!
			authenticated := false
			provided := s.extractToken(r)
			if expectedToken != "" && provided != "" {
				if subtle.ConstantTimeCompare([]byte(provided), []byte(expectedToken)) == 1 {
					authenticated = true
					authorizedBootstrap = true
				}
			}
			if !authenticated {
				hashB64, saltB64, mErr := s.engine.Store().GetMasterPassphrase(ctx)
				if mErr == nil && hashB64 != "" && saltB64 != "" {
					if auth.VerifyPassphrase(password, hashB64, saltB64) || auth.VerifyPassphrase(provided, hashB64, saltB64) {
						authenticated = true
						if auth.VerifyPassphrase(provided, hashB64, saltB64) {
							authorizedBootstrap = true
						}
					}
				}
			}
			if !authenticated {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "admin account already protected; provide valid credentials to claim admin"})
				return
			}
		} else if expectedToken != "" {
			provided := s.extractToken(r)
			if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(expectedToken)) != 1 {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "admin bootstrap requires authorization token"})
				return
			}
			authorizedBootstrap = true
		} else {
			// No bootstrap credentials configured: initial administrator registration
			// is permitted only via local loopback. Remote clients must provide a configured server token or master passphrase.
			if !isLoopbackClient(r) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "initial admin registration requires local setup or configured server token"})
				return
			}
			authorizedBootstrap = true
		}
	}

	user, err := s.engine.Store().CreateUserAuthorized(ctx, username, password, authorizedBootstrap)
	if err != nil {
		if errors.Is(err, store.ErrUserAlreadyExists) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "username already taken"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	token, err := auth.GenerateSessionToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to generate session"})
		return
	}

	exp := time.Now().UTC().Add(auth.DefaultSessionTTL)
	if err := s.engine.Store().CreateUserSessionChecked(ctx, token, user.ID, user.PasswordHash, exp); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create session: " + err.Error()})
		return
	}

	s.setSessionCookie(w, r, token, auth.DefaultSessionTTL)
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "registered",
		"token":  token,
		"user": map[string]any{
			"id":       user.ID,
			"username": user.Username,
			"role":     user.Role,
		},
		"expires_in": int(auth.DefaultSessionTTL.Seconds()),
	})
}

// handleAuthLogin authenticates with username+password, master passphrase, or config auth_token.
func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		Passphrase string `json:"passphrase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json payload"})
		return
	}

	var user *store.User
	var err error
	var verifiedHash string

	// 1. Try username + password authentication
	if strings.TrimSpace(body.Username) != "" && body.Password != "" {
		user, err = s.engine.Store().AuthenticateUser(ctx, body.Username, body.Password)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid username or password"})
			return
		}
		verifiedHash = user.PasswordHash
	} else {
		// 2. Legacy fallback: check passphrase against master PIN or config auth_token
		pass := body.Passphrase
		if pass == "" {
			pass = body.Password
		}
		if pass == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "credentials required"})
			return
		}

		authenticated := false
		hashB64, saltB64, err := s.engine.Store().GetMasterPassphrase(ctx)
		if err == nil && hashB64 != "" && saltB64 != "" {
			if auth.VerifyPassphrase(pass, hashB64, saltB64) {
				authenticated = true
				verifiedHash = hashB64
			}
		}
		if !authenticated {
			expected := strings.TrimSpace(s.engine.Config().Server.AuthToken)
			if expected != "" && pass == expected {
				authenticated = true
			}
		}
		if !authenticated {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid passphrase or token"})
			return
		}

		// Map to admin user (ID 1)
		user, err = s.engine.Store().EnsureAdminUser(ctx)
		if err != nil || user == nil {
			user = &store.User{ID: 1, Username: "admin", Role: "admin"}
		}
		if verifiedHash == "" {
			verifiedHash = user.PasswordHash
		}
	}

	token, err := auth.GenerateSessionToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to generate session"})
		return
	}

	exp := time.Now().UTC().Add(auth.DefaultSessionTTL)
	if err := s.engine.Store().CreateUserSessionChecked(ctx, token, user.ID, verifiedHash, exp); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create session: " + err.Error()})
		return
	}

	s.setSessionCookie(w, r, token, auth.DefaultSessionTTL)
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "authenticated",
		"token":  token,
		"user": map[string]any{
			"id":       user.ID,
			"username": user.Username,
			"role":     user.Role,
		},
		"expires_in": int(auth.DefaultSessionTTL.Seconds()),
	})
}

// handleAuthMe returns current authenticated identity.
func (s *Server) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	u := userFromContext(r.Context())
	if u == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"user": map[string]any{
			"id":       u.UserID,
			"username": u.Username,
			"role":     u.Role,
		},
	})
}

// handleClassifyProxy allows online clients (e.g. mobile app or web) to evaluate an item
// using the VPS server's TypeSafe Jev System One or focus router.
func (s *Server) handleClassifyProxy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var rawBody bytes.Buffer
	if _, err := rawBody.ReadFrom(r.Body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body too large or unreadable"})
		return
	}

	var item ingest.IncomingItem
	directErr := json.Unmarshal(rawBody.Bytes(), &item)
	if directErr != nil || (item.Title == "" && item.Body == "") {
		var wrapper struct {
			Item ingest.IncomingItem `json:"item"`
		}
		wrapErr := json.Unmarshal(rawBody.Bytes(), &wrapper)
		if wrapErr == nil && (wrapper.Item.Title != "" || wrapper.Item.Body != "") {
			item = wrapper.Item
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid item payload"})
			return
		}
	}

	targetUserID := int64(1)
	effectiveMode := s.engine.Mode()
	if u := userFromContext(ctx); u != nil && u.UserID > 0 {
		targetUserID = u.UserID
		if u.UserID > 1 {
			if userMode, err := s.engine.Store().GetUserSetting(ctx, u.UserID, "focus_mode"); err == nil && userMode != "" {
				switch config.FocusMode(userMode) {
				case config.ModeNormal, config.ModeDeepWork, config.ModeDND:
					effectiveMode = config.FocusMode(userMode)
				}
			}
		}
	}
	item.UserID = targetUserID

	dec := s.engine.SimulateItem(ctx, effectiveMode, item)
	writeJSON(w, http.StatusOK, dec)
}

// handleAuthLogout terminates the active session and invalidates user sessions.
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := s.extractToken(r)
	if token != "" {
		if su, err := s.engine.Store().ValidateSessionUser(ctx, token); err == nil && su != nil {
			_ = s.engine.Store().InvalidateUserSessions(ctx, su.UserID)
		}
		_ = s.engine.Store().DeleteSession(ctx, token)
	}
	s.clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged_out"})
}

// handleAuthChangePassphrase changes the master passphrase.
func (s *Server) handleAuthChangePassphrase(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		CurrentPassphrase string `json:"current_passphrase"`
		NewPassphrase     string `json:"new_passphrase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json payload"})
		return
	}

	curr := body.CurrentPassphrase
	next := body.NewPassphrase
	if len(next) < 4 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "new passphrase must be at least 4 characters"})
		return
	}

	u := userFromContext(ctx)
	if u != nil {
		nextHash, err := s.engine.Store().ChangeUserPassword(ctx, u.UserID, curr, next)
		if err != nil {
			if strings.Contains(err.Error(), "incorrect") {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "current password incorrect"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		// Issue a fresh session to the authenticated caller
		newToken, err := auth.GenerateSessionToken()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to issue new session"})
			return
		}
		exp := time.Now().UTC().Add(auth.DefaultSessionTTL)
		if err := s.engine.Store().CreateUserSessionChecked(ctx, newToken, u.UserID, nextHash, exp); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to persist new session"})
			return
		}

		s.setSessionCookie(w, r, newToken, auth.DefaultSessionTTL)
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "password_changed",
			"token":      newToken,
			"expires_in": int(auth.DefaultSessionTTL.Seconds()),
		})
		return
	}

	// Administrator or legacy master-passphrase rotation (unauthenticated or pre-session)
	authenticated := false
	hashB64, saltB64, err := s.engine.Store().GetMasterPassphrase(ctx)
	if err == nil && hashB64 != "" && saltB64 != "" {
		if auth.VerifyPassphrase(curr, hashB64, saltB64) {
			authenticated = true
		}
	}
	if !authenticated {
		if adminUser, aErr := s.engine.Store().AuthenticateUser(ctx, "admin", curr); aErr == nil && adminUser != nil {
			authenticated = true
		}
	}
	if !authenticated {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "current passphrase incorrect"})
		return
	}

	newHash, newSalt, err := auth.HashPassphrase(next)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to hash new passphrase"})
		return
	}

	if err := s.engine.Store().SetMasterPassphrase(ctx, newHash, newSalt); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update passphrase"})
		return
	}

	// Invalidate all previous sessions upon credential change
	_ = s.engine.Store().InvalidateAllSessions(ctx)

	// Issue a fresh session to the authenticated administrator
	newToken, err := auth.GenerateSessionToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to issue new session"})
		return
	}
	exp := time.Now().UTC().Add(auth.DefaultSessionTTL)
	adminID := int64(1)
	if u != nil && u.UserID > 0 {
		adminID = u.UserID
	}
	if err := s.engine.Store().CreateUserSession(ctx, newToken, adminID, exp); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to persist new session"})
		return
	}
	s.setSessionCookie(w, r, newToken, auth.DefaultSessionTTL)

	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "passphrase_updated",
		"token":      newToken,
		"expires_in": int(auth.DefaultSessionTTL.Seconds()),
	})
}

func requireAdmin(r *http.Request) bool {
	u := userFromContext(r.Context())
	if u == nil {
		return false
	}
	return u.Role == "admin" || u.UserID <= 1
}

// handleGetProfile returns user profile, session details, and GitHub connection state.
func (s *Server) handleGetProfile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	setupReq, _ := s.engine.Store().IsAuthSetupRequired(ctx)
	u := userFromContext(ctx)
	var userInfo any
	if u != nil {
		userInfo = map[string]any{
			"id":       u.UserID,
			"username": u.Username,
			"role":     u.Role,
		}
	}

	// Non-admin users only see their own account profile and cannot access server-wide integration secrets
	if u != nil && u.Role != "admin" {
		writeJSON(w, http.StatusOK, map[string]any{
			"auth_configured": !setupReq,
			"user":            userInfo,
			"github": map[string]any{
				"connected": false,
			},
			"email": map[string]any{
				"connected": false,
			},
			"slack": map[string]any{
				"connected": false,
			},
			"matrix": map[string]any{
				"connected": false,
			},
			"notifications": map[string]any{
				"only_mentions": false,
			},
		})
		return
	}

	dbTok, userJSON, part, _ := s.engine.Store().GetGitHubCredentials(ctx)

	tokenPreview := ""
	if len(dbTok) > 8 {
		tokenPreview = dbTok[:4] + "..." + dbTok[len(dbTok)-4:]
	} else if dbTok != "" {
		tokenPreview = "***"
	}

	var ghUser any
	if userJSON != "" {
		_ = json.Unmarshal([]byte(userJSON), &ghUser)
	}

	customClientID, _ := s.engine.Store().GetSetting(ctx, "github_client_id")
	emailCreds, _ := s.engine.Store().GetEmailCredentials(ctx)
	notifySettings, _ := s.engine.Store().GetNotifySettings(ctx)

	writeJSON(w, http.StatusOK, map[string]any{
		"auth_configured": !setupReq,
		"user":            userInfo,
		"github": map[string]any{
			"connected":          dbTok != "",
			"token_preview":      tokenPreview,
			"participating_only": part,
			"user":               ghUser,
			"client_id":          customClientID,
		},
		"email": map[string]any{
			"connected":    emailCreds.Enabled && emailCreds.Username != "",
			"provider":     emailCreds.Provider,
			"host":         emailCreds.Host,
			"port":         emailCreds.Port,
			"username":     emailCreds.Username,
			"poll_minutes": emailCreds.PollMinutes,
		},
		"slack": map[string]any{
			"connected":      notifySettings.SlackEnabled && notifySettings.SlackWebhookURL != "",
			"enabled":        notifySettings.SlackEnabled,
			"url_preview":    maskWebhookURL(notifySettings.SlackWebhookURL),
			"mention_handle": notifySettings.SlackMention,
		},
		"matrix": map[string]any{
			"connected":      notifySettings.MatrixEnabled && notifySettings.MatrixWebhookURL != "",
			"enabled":        notifySettings.MatrixEnabled,
			"url_preview":    maskWebhookURL(notifySettings.MatrixWebhookURL),
			"mention_handle": notifySettings.MatrixMention,
		},
		"notifications": map[string]any{
			"only_mentions":   notifySettings.OnlyMentions,
			"mention_handles": notifySettings.MentionHandles,
		},
	})
}

// fetchGitHubUserProfile calls GitHub's /user API to validate the token and retrieve profile info.
func fetchGitHubUserProfile(ctx context.Context, client *http.Client, token string) (*gitHubUserProfile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "FocusGate")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github api request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("github api rejected token (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var profile gitHubUserProfile
	if err := json.NewDecoder(resp.Body).Decode(&profile); err != nil {
		return nil, fmt.Errorf("decode github profile: %w", err)
	}
	return &profile, nil
}

// handleGitHubToken connects a GitHub account via Personal Access Token (PAT).
func (s *Server) handleGitHubToken(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: requires administrator privileges"})
		return
	}
	ctx := r.Context()
	var body struct {
		Token             string `json:"token"`
		ParticipatingOnly *bool  `json:"participating_only"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json payload"})
		return
	}

	token := strings.TrimSpace(body.Token)
	if token == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "github token cannot be empty"})
		return
	}

	profile, err := fetchGitHubUserProfile(ctx, s.engine.HTTPClient(), token)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	userBytes, err := json.Marshal(profile)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to encode profile"})
		return
	}

	part := true
	if body.ParticipatingOnly != nil {
		part = *body.ParticipatingOnly
	}

	if err := s.engine.Store().SetGitHubCredentials(ctx, token, string(userBytes), part); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to persist github credentials"})
		return
	}

	// Trigger immediate initial notification sync in background
	go func() {
		_, _ = s.engine.TriggerGitHubSync(context.Background())
	}()

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "connected",
		"user":   profile,
	})
}

// handleGitHubDisconnect removes linked GitHub account and credentials.
func (s *Server) handleGitHubDisconnect(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: requires administrator privileges"})
		return
	}
	ctx := r.Context()
	if err := s.engine.Store().ClearGitHubCredentials(ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to clear credentials"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "disconnected"})
}

// handleGitHubSettings updates GitHub sync preferences (e.g. participating_only).
func (s *Server) handleGitHubSettings(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: requires administrator privileges"})
		return
	}
	ctx := r.Context()
	var body struct {
		ParticipatingOnly *bool  `json:"participating_only"`
		ClientID          string `json:"client_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json payload"})
		return
	}

	if body.ParticipatingOnly != nil {
		partVal := "false"
		if *body.ParticipatingOnly {
			partVal = "true"
		}
		if err := s.engine.Store().SetSetting(ctx, "github_participating_only", partVal); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update preference"})
			return
		}
	}

	if strings.TrimSpace(body.ClientID) != "" {
		if err := s.engine.Store().SetSetting(ctx, "github_client_id", strings.TrimSpace(body.ClientID)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save client_id"})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "settings_saved"})
}

// handleGitHubDeviceStart initiates the GitHub Device Flow (RFC 8628).
func (s *Server) handleGitHubDeviceStart(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: requires administrator privileges"})
		return
	}
	ctx := r.Context()
	var body struct {
		ClientID string `json:"client_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	clientID := strings.TrimSpace(body.ClientID)
	if clientID == "" {
		storedID, _ := s.engine.Store().GetSetting(ctx, "github_client_id")
		clientID = strings.TrimSpace(storedID)
	}
	if clientID == "" {
		clientID = defaultGitHubClientID
	}

	data := url.Values{}
	data.Set("client_id", clientID)
	data.Set("scope", "repo,read:user,notifications")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://github.com/login/device/code", strings.NewReader(data.Encode()))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to prepare device request"})
		return
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "FocusGate")

	resp, err := s.engine.HTTPClient().Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("failed to reach github: %v", err)})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":             "oauth_app_not_found",
			"error_description": "GitHub OAuth App Client ID not recognized by GitHub. Please use the Personal Access Token (PAT) below or provide a registered OAuth Client ID.",
		})
		return
	}

	var result struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
		Error           string `json:"error"`
		ErrorDesc       string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "invalid response from github device auth"})
		return
	}

	if result.Error != "" {
		errDesc := result.ErrorDesc
		if result.Error == "Not Found" {
			errDesc = "GitHub OAuth App Client ID not recognized by GitHub. Please use the Personal Access Token (PAT) below or provide a registered OAuth Client ID."
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":             result.Error,
			"error_description": errDesc,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"device_code":      result.DeviceCode,
		"user_code":        result.UserCode,
		"verification_uri": result.VerificationURI,
		"expires_in":       result.ExpiresIn,
		"interval":         max(result.Interval, 5),
		"client_id":        clientID,
	})
}

// handleGitHubDevicePoll checks if user has approved the device code on GitHub.
func (s *Server) handleGitHubDevicePoll(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: requires administrator privileges"})
		return
	}
	ctx := r.Context()
	var body struct {
		DeviceCode string `json:"device_code"`
		ClientID   string `json:"client_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json payload"})
		return
	}

	deviceCode := strings.TrimSpace(body.DeviceCode)
	clientID := strings.TrimSpace(body.ClientID)
	if deviceCode == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "device_code is required"})
		return
	}
	if clientID == "" {
		storedID, _ := s.engine.Store().GetSetting(ctx, "github_client_id")
		clientID = strings.TrimSpace(storedID)
	}
	if clientID == "" {
		clientID = defaultGitHubClientID
	}

	data := url.Values{}
	data.Set("client_id", clientID)
	data.Set("device_code", deviceCode)
	data.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://github.com/login/oauth/access_token", strings.NewReader(data.Encode()))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to prepare poll request"})
		return
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "FocusGate")

	resp, err := s.engine.HTTPClient().Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("failed to poll github: %v", err)})
		return
	}
	defer resp.Body.Close()

	var pollResult struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
		Interval    int    `json:"interval"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pollResult); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "invalid poll response from github"})
		return
	}

	if pollResult.Error != "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":            pollResult.Error,
			"error_description": pollResult.ErrorDesc,
			"interval":          pollResult.Interval,
		})
		return
	}

	if pollResult.AccessToken == "" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "authorization_pending"})
		return
	}

	// Token granted! Fetch user profile
	profile, err := fetchGitHubUserProfile(ctx, s.engine.HTTPClient(), pollResult.AccessToken)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to fetch user profile with new token: " + err.Error()})
		return
	}

	userBytes, err := json.Marshal(profile)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to encode profile"})
		return
	}

	if err := s.engine.Store().SetGitHubCredentials(ctx, pollResult.AccessToken, string(userBytes), true); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save credentials"})
		return
	}

	// Trigger immediate sync
	go func() {
		_, _ = s.engine.TriggerGitHubSync(context.Background())
	}()

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "success",
		"user":   profile,
	})
}
