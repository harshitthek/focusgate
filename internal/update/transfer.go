package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

var errTransferIdle = errors.New("asset download stalled")

type progressReader struct {
	io.Reader
	timer *time.Timer
	idle  time.Duration
}

func (r progressReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 {
		r.timer.Reset(r.idle)
	}
	return n, err
}

// Metadata keeps the ordinary client timeout; assets use bounded setup and
// thirty seconds of read inactivity rather than a total body-transfer timeout.
func (c *Client) downloadArchive(ctx context.Context, dst io.Writer, uri string) error {
	return c.downloadArchiveWithIdle(ctx, dst, uri, 30*time.Second)
}

func (c *Client) downloadArchiveWithIdle(ctx context.Context, dst io.Writer, uri string, idleAfter time.Duration) error {
	if idleAfter <= 0 {
		return errors.New("download inactivity budget must be positive")
	}
	base := c.HTTPClient
	if base == nil {
		base = http.DefaultClient
	}
	client := *base
	client.Timeout = 0
	var transport *http.Transport
	if base.Transport == nil {
		transport = http.DefaultTransport.(*http.Transport).Clone()
	} else if existing, ok := base.Transport.(*http.Transport); ok {
		transport = existing.Clone()
	}
	if transport != nil {
		if base.Transport == nil {
			transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		}
		transport.ResponseHeaderTimeout = 15 * time.Second
		transport.TLSHandshakeTimeout = 10 * time.Second
		transport.MaxResponseHeaderBytes = 16 << 10
		client.Transport = transport
		defer transport.CloseIdleConnections()
	}
	transferCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	req, err := http.NewRequestWithContext(transferCtx, http.MethodGet, uri, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "FocusGate-Updater")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("downloading release archive: %w", err)
	}
	defer resp.Body.Close()
	idle := time.AfterFunc(idleAfter, func() { cancel(errTransferIdle) })
	defer idle.Stop()
	stopClose := context.AfterFunc(transferCtx, func() { _ = resp.Body.Close() })
	defer stopClose()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned status %d", resp.StatusCode)
	}
	idle.Reset(idleAfter)
	if _, err := copyBounded(transferCtx, dst, progressReader{Reader: resp.Body, timer: idle, idle: idleAfter}, maxAssetBytes); err != nil {
		if errors.Is(context.Cause(transferCtx), errTransferIdle) {
			return errTransferIdle
		}
		return fmt.Errorf("download release archive: %w", err)
	}
	return nil
}
