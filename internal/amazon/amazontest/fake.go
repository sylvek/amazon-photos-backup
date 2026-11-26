// Package amazontest provides a fake Amazon Photos web API for tests. It
// reproduces the behaviours the client depends on: cookie auth, the search
// paging cap, date filters, and downloads that redirect to another host.
package amazontest

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"amazon-photos-backup/internal/amazon"
)

const (
	MaxLimit  = 200
	MaxWindow = 9999 // offset + limit may not exceed this
)

// Request is one call received by the fake API host.
type Request struct {
	Path   string
	Query  url.Values
	Header http.Header
}

type Fake struct {
	t testing.TB

	mu                sync.Mutex
	Nodes             []amazon.Node
	Content           map[string][]byte
	OwnerID           string
	ValidAT           string
	RotateSessionID   bool            // every response sets a new session-id cookie
	SigninRedirect    bool            // unauthenticated calls redirect to /ap/signin instead of 401
	HTMLInsteadOfJSON bool            // API answers 200 text/html, as an expired session can
	Fail503           int             // next N API calls answer 503
	FailDownloads     map[string]int  // node id → number of 503s before content is served
	Truncate          map[string]bool // node id → every download is cut mid-body
	TruncateTimes     map[string]int  // node id → number of downloads cut mid-body before one succeeds
	ExpireAfter       int             // revoke the session after this many download requests (0 = never)
	Forbidden         map[string]bool // node id → download answers 403
	Stall             map[string]bool // node id → download sends half, then hangs
	BadRequests       int             // number of 400 answers given
	Requests          []Request
	Downloads         map[string]int // node id → completed content requests
	rotations         int

	api, cdn *httptest.Server
}

func New(t testing.TB) *Fake {
	f := &Fake{
		t:             t,
		Content:       map[string][]byte{},
		OwnerID:       "OWNER1",
		ValidAT:       "valid-token",
		FailDownloads: map[string]int{},
		Truncate:      map[string]bool{},
		TruncateTimes: map[string]int{},
		Forbidden:     map[string]bool{},
		Stall:         map[string]bool{},
		Downloads:     map[string]int{},
	}
	f.api = httptest.NewServer(http.HandlerFunc(f.serveAPI))
	f.cdn = httptest.NewServer(http.HandlerFunc(f.serveCDN))
	t.Cleanup(f.api.Close)
	t.Cleanup(f.cdn.Close)
	return f
}

// URL is the base URL of the fake site (stands in for https://www.amazon.fr).
func (f *Fake) URL() string { return f.api.URL }

// cdnURL uses another host name than the API, so that a client leaking
// cookies across hosts is caught.
func (f *Fake) cdnURL() string {
	return strings.Replace(f.cdn.URL, "127.0.0.1", "localhost", 1)
}

// Cookies returns a cookie set the fake accepts.
func (f *Fake) Cookies() map[string]string {
	return map[string]string{"session-id": "111-2222222-3333333", "ubid-acbfr": "ubid-1", "at-acbfr": f.ValidAT}
}

// AddNode registers a file. date is RFC 3339 (or empty for an item without
// any usable date); it is used as both content date and creation date.
func (f *Fake) AddNode(id, name, date string, content []byte) amazon.Node {
	sum := md5.Sum(content)
	n := amazon.Node{
		ID: id, Name: name, Kind: "FILE", Status: "AVAILABLE", OwnerID: f.OwnerID,
		CreatedDate: date, ModifiedDate: date,
		Content: amazon.ContentProperties{
			MD5: hex.EncodeToString(sum[:]), Size: int64(len(content)),
			ContentType: "image/jpeg", Extension: "jpg", ContentDate: date,
		},
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Nodes = append(f.Nodes, n)
	f.Content[id] = content
	return n
}

// SetContent changes what a node's download returns without touching its metadata.
func (f *Fake) SetContent(id string, content []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Content[id] = content
}

// Update applies fn under the fake's lock.
func (f *Fake) Update(fn func(f *Fake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *Fake) RequestLog() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.Requests...)
}

func (f *Fake) DownloadCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Downloads[id]
}

func (f *Fake) TotalDownloads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, n := range f.Downloads {
		total += n
	}
	return total
}

func (f *Fake) authed(r *http.Request) bool {
	c, err := r.Cookie("at-acbfr")
	return err == nil && c.Value == f.ValidAT
}

func (f *Fake) serveAPI(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests = append(f.Requests, Request{Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone()})

	if r.URL.Path == "/ap/signin" {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html>Sign in</html>")
		return
	}
	if f.RotateSessionID {
		f.rotations++
		http.SetCookie(w, &http.Cookie{Name: "session-id", Value: fmt.Sprintf("rot-%d", f.rotations), Path: "/"})
	}
	if r.URL.Path == "/photos/" {
		if !f.authed(r) {
			http.Redirect(w, r, "/ap/signin?openid.return_to=photos", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html>Amazon Photos</html>")
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/drive/v1/") {
		http.NotFound(w, r)
		return
	}
	if f.Fail503 > 0 {
		f.Fail503--
		http.Error(w, `{"message":"Service Unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	if f.HTMLInsteadOfJSON {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html>Please sign in</html>")
		return
	}
	if !f.authed(r) {
		if f.SigninRedirect {
			http.Redirect(w, r, "/ap/signin", http.StatusFound)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "BadAuthenticationData"})
		return
	}
	q := r.URL.Query()
	for k, v := range map[string]string{"asset": "ALL", "tempLink": "false", "resourceVersion": "V2", "ContentType": "JSON"} {
		if strings.HasSuffix(r.URL.Path, "/contentRedirection") {
			break
		}
		if q.Get(k) != v {
			f.bad(w, "missing base param "+k)
			return
		}
	}
	switch {
	case r.URL.Path == "/drive/v1/nodes":
		if q.Get("filters") != "isRoot:true" {
			f.bad(w, "unsupported filter")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"count": 1, "data": []amazon.Node{{ID: "ROOT", Kind: "FOLDER", OwnerID: f.OwnerID, Status: "AVAILABLE"}}})
	case r.URL.Path == "/drive/v1/search":
		f.search(w, q)
	case strings.HasSuffix(r.URL.Path, "/contentRedirection"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/drive/v1/nodes/"), "/contentRedirection")
		if q.Get("querySuffix") != "?download=true" || q.Get("ownerId") != f.OwnerID {
			f.bad(w, "bad download params")
			return
		}
		if f.Forbidden[id] {
			writeJSON(w, http.StatusForbidden, map[string]any{"message": "Forbidden"})
			return
		}
		if _, ok := f.Content[id]; !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"message": "not found"})
			return
		}
		if f.ExpireAfter > 0 {
			f.ExpireAfter--
			if f.ExpireAfter == 0 {
				f.ValidAT = "revoked-" + f.ValidAT
			}
		}
		http.Redirect(w, r, f.cdnURL()+"/content/"+url.PathEscape(id)+"?sig=abc", http.StatusFound)
	default:
		http.NotFound(w, r)
	}
}

var filterRE = regexp.MustCompile(`^(\w+):\((.*)\)$`)

func (f *Fake) search(w http.ResponseWriter, q url.Values) {
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	if limit <= 0 || limit > MaxLimit {
		f.bad(w, "bad limit")
		return
	}
	if offset+limit > MaxWindow {
		f.bad(w, "Offset + limit cannot be greater than 9999")
		return
	}
	year, month, day := 0, 0, 0
	for _, clause := range strings.Split(q.Get("filters"), " AND ") {
		m := filterRE.FindStringSubmatch(strings.TrimSpace(clause))
		if m == nil {
			f.bad(w, "malformed filter: "+clause)
			return
		}
		n, _ := strconv.Atoi(m[2])
		switch m[1] {
		case "type":
		case "timeYear":
			year = n
		case "timeMonth":
			month = n
		case "timeDay":
			day = n
		default:
			f.bad(w, "unknown filter "+m[1])
			return
		}
	}
	var match []amazon.Node
	for _, n := range f.Nodes {
		if year != 0 || month != 0 || day != 0 {
			d, err := time.Parse(time.RFC3339, n.Content.ContentDate)
			if err != nil {
				continue // undated items match no time filter
			}
			if (year != 0 && d.Year() != year) || (month != 0 && int(d.Month()) != month) || (day != 0 && d.Day() != day) {
				continue
			}
		}
		match = append(match, n)
	}
	sort.SliceStable(match, func(i, j int) bool { return match[i].CreatedDate > match[j].CreatedDate })
	page := []amazon.Node{}
	if offset < len(match) {
		page = match[offset:min(offset+limit, len(match))]
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(match), "data": page})
}

func (f *Fake) serveCDN(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Cookie") != "" {
		f.t.Errorf("session cookies leaked to the content host: %q", r.Header.Get("Cookie"))
	}
	if r.Header.Get("x-amzn-sessionid") != "" {
		f.t.Errorf("x-amzn-sessionid leaked to the content host")
	}
	id, _ := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/content/"))
	content, ok := f.Content[id]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if f.FailDownloads[id] > 0 {
		f.FailDownloads[id]--
		http.Error(w, "slow down", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+id+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	if f.TruncateTimes[id] > 0 {
		f.TruncateTimes[id]--
		w.Write(content[:len(content)/2])
		panic(http.ErrAbortHandler)
	}
	if f.Truncate[id] {
		w.Write(content[:len(content)/2])
		panic(http.ErrAbortHandler) // drop the connection mid-body
	}
	if f.Stall[id] {
		w.Write(content[:len(content)/2])
		w.(http.Flusher).Flush()
		f.mu.Unlock()
		<-r.Context().Done()
		f.mu.Lock()
		return
	}
	w.Write(content)
	f.Downloads[id]++
}

func (f *Fake) bad(w http.ResponseWriter, msg string) {
	f.BadRequests++
	writeJSON(w, http.StatusBadRequest, map[string]any{"message": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
