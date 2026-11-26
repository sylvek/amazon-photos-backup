package amazon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store keeps the session cookies in memory and on disk. It is deliberately
// flat (name → value): every cookie belongs to one Amazon site, and the jar
// built from it refuses to talk to any other host.
type Store struct {
	path string

	mu      sync.Mutex
	cookies map[string]string
	dirty   bool
	modTime time.Time // mtime of the file as we last read or wrote it
}

type storeFile struct {
	Updated time.Time         `json:"updated"`
	Cookies map[string]string `json:"cookies"`
}

func NewStore(path string) *Store {
	return &Store{path: path, cookies: map[string]string{}}
}

// Load reads the cookie file. A missing file is reported with os.ErrNotExist.
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	fi, err := os.Stat(s.path)
	if err != nil {
		return err
	}
	var f storeFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("%s: %w", s.path, err)
	}
	if f.Cookies == nil {
		f.Cookies = map[string]string{}
	}
	s.cookies = f.Cookies
	s.dirty = false
	s.modTime = fi.ModTime()
	return nil
}

// Replace swaps the whole cookie set, as after a fresh login.
func (s *Store) Replace(cookies map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cookies = map[string]string{}
	for k, v := range cookies {
		s.cookies[k] = v
	}
	s.dirty = true
	s.modTime = time.Time{}
}

// changedOnDiskLocked reports whether someone else rewrote the file since we
// last read or wrote it.
func (s *Store) changedOnDiskLocked() bool {
	fi, err := os.Stat(s.path)
	if err != nil {
		return false
	}
	return !s.modTime.IsZero() && !fi.ModTime().Equal(s.modTime)
}

// ReloadIfChanged picks up cookies written by another process (the login
// command) and reports whether it did.
func (s *Store) ReloadIfChanged() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.modTime.IsZero() {
		// Never loaded: any file that exists now is news.
		if _, err := os.Stat(s.path); err != nil {
			return false, nil
		}
		return true, s.loadLocked()
	}
	if !s.changedOnDiskLocked() {
		return false, nil
	}
	return true, s.loadLocked()
}

// Save writes the cookies atomically with mode 0600. If another process
// replaced the file in the meantime, its cookies win: they come from a newer
// login, and ours are loaded from it instead of overwriting it.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changedOnDiskLocked() {
		return s.loadLocked()
	}
	if !s.dirty {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(storeFile{Updated: time.Now().UTC(), Cookies: s.cookies}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".cookies-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return err
	}
	fi, err := os.Stat(s.path)
	if err != nil {
		return err
	}
	s.modTime = fi.ModTime()
	s.dirty = false
	return nil
}

func (s *Store) Get(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cookies[name]
}

func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cookies)
}

// Snapshot returns a copy of the current cookies.
func (s *Store) Snapshot() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.cookies))
	for k, v := range s.cookies {
		out[k] = v
	}
	return out
}

// Jar returns an http.CookieJar that sends the stored cookies to domain and
// its subdomains only, and records the Set-Cookie headers they send back.
func (s *Store) Jar(domain string) http.CookieJar {
	return &jar{store: s, domain: strings.ToLower(domain)}
}

type jar struct {
	store  *Store
	domain string
}

func (j *jar) allowed(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	return host == j.domain || strings.HasSuffix(host, "."+j.domain)
}

func (j *jar) Cookies(u *url.URL) []*http.Cookie {
	if !j.allowed(u) {
		return nil
	}
	j.store.mu.Lock()
	defer j.store.mu.Unlock()
	names := make([]string, 0, len(j.store.cookies))
	for name := range j.store.cookies {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]*http.Cookie, 0, len(names))
	for _, name := range names {
		c := &http.Cookie{Name: name, Value: j.store.cookies[name]}
		// Amazon uses quoted values; net/http would strip the quotes as
		// invalid bytes unless told the value is quoted.
		if v := c.Value; len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			c.Value, c.Quoted = v[1:len(v)-1], true
		}
		out = append(out, c)
	}
	return out
}

func (j *jar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	if !j.allowed(u) {
		return
	}
	j.store.mu.Lock()
	defer j.store.mu.Unlock()
	now := time.Now()
	for _, c := range cookies {
		if c.Name == "" {
			continue
		}
		if c.MaxAge < 0 || (!c.Expires.IsZero() && c.Expires.Before(now)) {
			if _, ok := j.store.cookies[c.Name]; ok {
				delete(j.store.cookies, c.Name)
				j.store.dirty = true
			}
			continue
		}
		value := c.Value
		if c.Quoted {
			value = `"` + value + `"`
		}
		if j.store.cookies[c.Name] != value {
			j.store.cookies[c.Name] = value
			j.store.dirty = true
		}
	}
}
