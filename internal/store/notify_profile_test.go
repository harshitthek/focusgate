package store_test

import (
	"context"
	"testing"

	"focusgate/internal/store"
)

func TestNotifyProfile_GetAndSet(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer st.Close()

	// Initial default should be empty / false
	initial, err := st.GetNotifySettings(ctx)
	if err != nil {
		t.Fatalf("GetNotifySettings failed: %v", err)
	}
	if initial.SlackEnabled || initial.MatrixEnabled || initial.OnlyMentions {
		t.Errorf("expected all disabled initially, got %+v", initial)
	}

	// Update settings
	toSave := store.NotifySettings{
		SlackEnabled:     true,
		SlackWebhookURL:  "https://hooks.slack.com/services/T123/B456/789",
		SlackMention:     "<@U12345>",
		MatrixEnabled:    true,
		MatrixWebhookURL: "https://matrix.org/_matrix/hook",
		MatrixMention:    "@dev:matrix.org",
		OnlyMentions:     true,
		MentionHandles:   "@dev, harshit",
	}

	if err := st.SetNotifySettings(ctx, toSave); err != nil {
		t.Fatalf("SetNotifySettings failed: %v", err)
	}

	loaded, err := st.GetNotifySettings(ctx)
	if err != nil {
		t.Fatalf("GetNotifySettings failed: %v", err)
	}

	if !loaded.SlackEnabled || loaded.SlackWebhookURL != toSave.SlackWebhookURL || loaded.SlackMention != toSave.SlackMention {
		t.Errorf("slack mismatch: got %+v, want %+v", loaded, toSave)
	}
	if !loaded.MatrixEnabled || loaded.MatrixWebhookURL != toSave.MatrixWebhookURL || loaded.MatrixMention != toSave.MatrixMention {
		t.Errorf("matrix mismatch: got %+v, want %+v", loaded, toSave)
	}
	if !loaded.OnlyMentions || loaded.MentionHandles != toSave.MentionHandles {
		t.Errorf("mentions mismatch: got %+v, want %+v", loaded, toSave)
	}
}
