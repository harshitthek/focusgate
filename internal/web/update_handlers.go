package web

import (
	"net/http"

	"focusgate/internal/update"
	"focusgate/internal/version"
)

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, version.Get())
}

func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: requires administrator privileges"})
		return
	}
	client := update.NewClient("")
	res, err := client.Check(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: requires administrator privileges"})
		return
	}
	if !s.updateMu.TryLock() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "update in progress; check the release again after it finishes"})
		return
	}
	defer s.updateMu.Unlock()
	var body struct {
		ReleaseIdentity string `json:"release_identity"`
	}
	if err := decodeJSON(r.Body, &body); err != nil || body.ReleaseIdentity == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "check and review the release before applying"})
		return
	}
	client := update.NewClient("")
	res, err := client.Check(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to check release: " + err.Error()})
		return
	}

	if body.ReleaseIdentity != res.ReleaseIdentity {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "release changed; check and review it again"})
		return
	}

	if !res.HasUpdate {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "up_to_date",
			"message": "FocusGate is already up to date (" + res.CurrentVersion + ").",
		})
		return
	}

	if err := client.Apply(r.Context(), res); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Update installation failed: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "updated",
		"message": "FocusGate successfully updated to " + res.LatestVersion + ". Restart the server to activate the new binary.",
		"version": res.LatestVersion,
	})
}
