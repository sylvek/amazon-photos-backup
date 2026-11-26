package amazon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrAuth means Amazon no longer accepts the session cookies. Nothing but a
// fresh login fixes it, so callers stop instead of retrying.
var ErrAuth = errors.New("amazon session is not valid (login required)")

// DefaultUserAgent is sent when none is configured. Amazon treats unknown
// agents with suspicion, so this mimics a desktop browser.
const DefaultUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

const sessionHeader = "x-amzn-sessionid"

type Options struct {
	BaseURL   string // e.g. https://www.amazon.fr
	UserAgent string
	Store     *Store
	Retries   int           // retries after the first attempt; default 5
	RetryBase time.Duration // first backoff delay, doubled each retry; default 2s
	// IdleTimeout aborts a download that receives no data for this long;
	// default 2 minutes.
	IdleTimeout time.Duration
}

type Client struct {
	base      *url.URL
	store     *Store
	http      *http.Client
	userAgent string
	retries   int
	retryBase time.Duration
	idle      time.Duration
	allowed   func(*url.URL) bool

	rootMu sync.Mutex
	root   *Node
}

// Node is a file or folder as described by the drive API.
type Node struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Kind         string            `json:"kind"`
	Status       string            `json:"status"`
	OwnerID      string            `json:"ownerId"`
	CreatedDate  string            `json:"createdDate"`
	ModifiedDate string            `json:"modifiedDate"`
	Content      ContentProperties `json:"contentProperties"`
}

type ContentProperties struct {
	MD5         string `json:"md5"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType"`
	Extension   string `json:"extension"`
	ContentDate string `json:"contentDate"`
}

func NewClient(o Options) (*Client, error) {
	if o.Store == nil {
		return nil, errors.New("amazon: a cookie store is required")
	}
	base, err := url.Parse(o.BaseURL)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") {
		return nil, fmt.Errorf("amazon: invalid base URL %q", o.BaseURL)
	}
	domain := strings.TrimPrefix(strings.ToLower(base.Hostname()), "www.")
	c := &Client{
		base:      base,
		store:     o.Store,
		userAgent: o.UserAgent,
		retries:   o.Retries,
		retryBase: o.RetryBase,
	}
	if c.userAgent == "" {
		c.userAgent = DefaultUserAgent
	}
	if c.retries <= 0 {
		c.retries = 5
	}
	if c.retryBase <= 0 {
		c.retryBase = 2 * time.Second
	}
	c.idle = o.IdleTimeout
	if c.idle <= 0 {
		c.idle = 2 * time.Minute
	}
	j := &jar{store: o.Store, domain: domain}
	c.allowed = j.allowed

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 90 * time.Second
	c.http = &http.Client{
		Jar:       j,
		Transport: transport,
		// No overall timeout: videos can take long. Stalled downloads are
		// cut by the idle watchdog in Download.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if c.allowed(req.URL) {
				if strings.HasPrefix(req.URL.Path, "/ap/signin") {
					return fmt.Errorf("redirected to the sign-in page: %w", ErrAuth)
				}
			} else {
				// Content is served from other hosts through signed URLs;
				// nothing identifying the session may follow the redirect.
				req.Header.Del(sessionHeader)
			}
			return nil
		},
	}
	return c, nil
}

// retryable reports whether a status is worth another attempt.
func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// get performs a GET with retries on network errors and transient statuses.
// The caller owns the response body. Auth failures are never retried.
func (c *Client) get(ctx context.Context, path string, query url.Values, accept string) (*http.Response, error) {
	u := *c.base
	u.Path = path
	u.RawQuery = query.Encode()

	var lastErr error
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			delay := c.retryBase << (attempt - 1)
			if delay > time.Minute || delay <= 0 {
				delay = time.Minute
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", c.userAgent)
		req.Header.Set("Accept", accept)
		req.Header.Set("Accept-Language", "fr-FR,fr;q=0.9,en;q=0.8")
		if sid := c.store.Get("session-id"); sid != "" {
			req.Header.Set(sessionHeader, sid)
		}
		resp, err := c.http.Do(req)
		switch {
		case err != nil:
			if errors.Is(err, ErrAuth) || ctx.Err() != nil {
				return nil, err
			}
			lastErr = err
		case resp.StatusCode == http.StatusUnauthorized:
			body := snippet(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("GET %s: HTTP 401 %s: %w", path, body, ErrAuth)
		case retryable(resp.StatusCode):
			lastErr = fmt.Errorf("GET %s: HTTP %d %s", path, resp.StatusCode, snippet(resp.Body))
			resp.Body.Close()
		default:
			return resp, nil
		}
		if attempt >= c.retries {
			return nil, fmt.Errorf("giving up after %d attempts: %w", attempt+1, lastErr)
		}
	}
}

func snippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 300))
	return strings.Join(strings.Fields(string(b)), " ")
}

func baseParams() url.Values {
	return url.Values{
		"asset":           {"ALL"},
		"tempLink":        {"false"},
		"resourceVersion": {"V2"},
		"ContentType":     {"JSON"},
	}
}

// getJSON calls a /drive/v1 endpoint and decodes its JSON answer.
func (c *Client) getJSON(ctx context.Context, path string, params url.Values, out any) error {
	q := baseParams()
	for k, v := range params {
		q[k] = v
	}
	resp, err := c.get(ctx, "/drive/v1"+path, q, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("GET %s: HTTP 403 %s: %w", path, snippet(resp.Body), ErrAuth)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d %s", path, resp.StatusCode, snippet(resp.Body))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		// An expired session can answer 200 with the sign-in page.
		return fmt.Errorf("GET %s: got %q instead of JSON: %w", path, ct, ErrAuth)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("GET %s: decoding response: %w", path, err)
	}
	return nil
}

type nodeList struct {
	Count int    `json:"count"`
	Data  []Node `json:"data"`
}

// Root returns the account's root folder; its ownerId is needed for
// downloads. It also serves as the cheapest check that the session works.
func (c *Client) Root(ctx context.Context) (Node, error) {
	c.rootMu.Lock()
	defer c.rootMu.Unlock()
	if c.root != nil {
		return *c.root, nil
	}
	var list nodeList
	if err := c.getJSON(ctx, "/nodes", url.Values{"filters": {"isRoot:true"}}, &list); err != nil {
		return Node{}, err
	}
	if len(list.Data) == 0 {
		return Node{}, errors.New("amazon: no root node in response")
	}
	c.root = &list.Data[0]
	return *c.root, nil
}

// KeepAlive loads the Amazon Photos page like a browser would, which gives
// Amazon the opportunity to refresh the session cookies.
func (c *Client) KeepAlive(ctx context.Context) error {
	resp, err := c.get(ctx, "/photos/", nil, "text/html,application/xhtml+xml")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("GET /photos/: HTTP 403: %w", ErrAuth)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /photos/: HTTP %d", resp.StatusCode)
	}
	return nil
}
