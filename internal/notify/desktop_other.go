//go:build !windows

package notify

import (
	"context"
	"os/exec"
	"time"

	"focusgate/internal/ingest"
)

// SendDesktopToast uses notify-send if present on Linux/BSD, or returns nil quietly on headless servers.
func SendDesktopToast(ctx context.Context, title, body string) error {
	path, err := exec.LookPath("notify-send")
	if err != nil {
		return nil
	}
	runCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, path, ingest.TruncateRunes(title, 100), ingest.TruncateRunes(body, 240))
	return cmd.Run()
}
