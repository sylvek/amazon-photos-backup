package amazon

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

// Download streams the original file of a node into w and returns the number
// of bytes written. It does not retry once data has started flowing, because
// w may already hold a partial copy; the caller decides what to do with it.
func (c *Client) Download(ctx context.Context, n Node, w io.Writer) (int64, error) {
	owner := n.OwnerID
	if owner == "" {
		root, err := c.Root(ctx)
		if err != nil {
			return 0, err
		}
		owner = root.OwnerID
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	path := "/drive/v1/nodes/" + url.PathEscape(n.ID) + "/contentRedirection"
	resp, err := c.get(ctx, path, url.Values{"querySuffix": {"?download=true"}, "ownerId": {owner}}, "*/*")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, &StatusError{Op: "download " + n.ID, Code: resp.StatusCode, Body: snippet(resp.Body)}
	}

	// Cut the transfer if no data arrives for a while, so that one hung
	// connection cannot block the backup forever.
	var stalled atomic.Bool
	watchdog := time.AfterFunc(c.idle, func() {
		stalled.Store(true)
		cancel()
	})
	defer watchdog.Stop()
	written, err := io.Copy(w, &progressReader{r: resp.Body, onRead: func() { watchdog.Reset(c.idle) }})
	if err != nil {
		if stalled.Load() {
			return written, fmt.Errorf("download %s: no data received for %s, transfer aborted", n.ID, c.idle)
		}
		return written, fmt.Errorf("download %s: %w", n.ID, err)
	}
	return written, nil
}

type progressReader struct {
	r      io.Reader
	onRead func()
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.onRead()
	}
	return n, err
}
