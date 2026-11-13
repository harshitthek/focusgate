package web

import (
	"net/http"

	"focusgate/internal/update"
	"focusgate/internal/version"
)

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, version.Get())
}

func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	client := update.NewClient("")
	res, err := client.Check(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	client := update.NewClient("")
	res, err := client.Check(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to check release: " + err.Error()})
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
