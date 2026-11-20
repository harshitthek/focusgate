package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"focusgate/internal/store"
)

func (s *Server) handleAddFeed(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		URL         string `json:"url"`
		PollMinutes int    `json:"poll_minutes"`
	}
	if err := decodeJSON(http.MaxBytesReader(w, r.Body, 4096), &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid feed payload"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	userID := userFromContext(ctx).UserID
	feed, err := s.engine.Store().AddUserFeed(ctx, userID, body.Name, body.URL, body.PollMinutes)
	switch {
	case err == nil:
		writeJSON(w, http.StatusCreated, feed)
	case errors.Is(err, store.ErrInvalidFeed):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, store.ErrFeedAlreadyExists), errors.Is(err, store.ErrFeedLimit):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to subscribe to feed"})
	}
}

func (s *Server) handleDeleteFeed(w http.ResponseWriter, r *http.Request) {
	feedID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || feedID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid feed ID"})
		return
	}
	userID := userFromContext(r.Context()).UserID
	err = s.engine.Store().DeleteUserFeed(r.Context(), userID, feedID)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]bool{"removed": true})
	case errors.Is(err, store.ErrFeedNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "feed not found"})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to remove feed"})
	}
}
