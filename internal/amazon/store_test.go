package amazon

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func cookieMap(cs []*http.Cookie) map[string]string {
	m := map[string]string{}
	for _, c := range cs {
		v := c.Value
		if c.Quoted {
			v = `"` + v + `"`
		}
		m[c.Name] = v
	}
	return m
}

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "cookies.json")
	s := NewStore(path)
	if err := s.Load(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load on missing file: got %v, want os.ErrNotExist", err)
	}
	s.Replace(map[string]string{"session-id": "1", "at-acbfr": "tok"})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("cookie file mode = %v, want 0600", fi.Mode().Perm())
	}
	s2 := NewStore(path)
	if err := s2.Load(); err != nil {
		t.Fatal(err)
	}
	if s2.Get("at-acbfr") != "tok" || s2.Len() != 2 {
		t.Errorf("reloaded store: at=%q len=%d", s2.Get("at-acbfr"), s2.Len())
	}
}

func TestJarOnlySendsToDomain(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "c.json"))
	s.Replace(map[string]string{"session-id": "1", "at-acbfr": "tok"})
	jar := s.Jar("amazon.fr")
	for _, ok := range []string{"https://www.amazon.fr/drive/v1/search", "https://amazon.fr/", "https://thumbnails-photos.amazon.fr/x"} {
		if got := cookieMap(jar.Cookies(mustURL(t, ok))); got["at-acbfr"] != "tok" || len(got) != 2 {
			t.Errorf("%s: got cookies %v", ok, got)
		}
	}
	for _, bad := range []string{"https://evil.example/", "https://notamazon.fr/", "https://amazon.fr.evil.example/", "https://content-eu.drive.amazonaws.com/x"} {
		if got := jar.Cookies(mustURL(t, bad)); len(got) != 0 {
			t.Errorf("%s: leaked cookies %v", bad, got)
		}
	}
}

func TestJarAppliesSetCookie(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "c.json"))
	s.Replace(map[string]string{"session-id": "old", "at-acbfr": "tok", "gone": "x", "expired": "y"})
	jar := s.Jar("amazon.fr")
	u := mustURL(t, "https://www.amazon.fr/photos/")
	jar.SetCookies(u, []*http.Cookie{
		{Name: "session-id", Value: "new"},
		{Name: "fresh", Value: "1"},
		{Name: "gone", Value: "", MaxAge: -1},
		{Name: "expired", Value: "deleted", Expires: time.Now().Add(-time.Hour)},
	})
	if s.Get("session-id") != "new" || s.Get("fresh") != "1" {
		t.Errorf("updates not applied: session-id=%q fresh=%q", s.Get("session-id"), s.Get("fresh"))
	}
	if s.Get("gone") != "" || s.Get("expired") != "" {
		t.Errorf("deletions not applied: gone=%q expired=%q", s.Get("gone"), s.Get("expired"))
	}
	// A foreign host must not be able to plant or change cookies.
	jar.SetCookies(mustURL(t, "https://evil.example/"), []*http.Cookie{{Name: "at-acbfr", Value: "hijack"}})
	if s.Get("at-acbfr") != "tok" {
		t.Errorf("foreign host changed at-acbfr to %q", s.Get("at-acbfr"))
	}
}

func TestJarPreservesQuotedValues(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "c.json"))
	s.Replace(map[string]string{"x-acbfr": `"abc@def"`})
	jar := s.Jar("amazon.fr")
	u := mustURL(t, "https://www.amazon.fr/")
	req, _ := http.NewRequest("GET", u.String(), nil)
	for _, c := range jar.Cookies(u) {
		req.AddCookie(c)
	}
	if got := req.Header.Get("Cookie"); got != `x-acbfr="abc@def"` {
		t.Errorf("Cookie header = %q, want quoted value preserved", got)
	}
	jar.SetCookies(u, []*http.Cookie{{Name: "x-acbfr", Value: "new", Quoted: true}})
	if s.Get("x-acbfr") != `"new"` {
		t.Errorf("quoted Set-Cookie stored as %q", s.Get("x-acbfr"))
	}
}

func TestStoreReloadIfChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	a := NewStore(path)
	a.Replace(map[string]string{"at-acbfr": "one"})
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	if changed, err := a.ReloadIfChanged(); err != nil || changed {
		t.Fatalf("no change expected, got changed=%v err=%v", changed, err)
	}
	b := NewStore(path)
	b.Replace(map[string]string{"at-acbfr": "two"})
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(path, time.Now().Add(2*time.Second), time.Now().Add(2*time.Second))
	if changed, err := a.ReloadIfChanged(); err != nil || !changed {
		t.Fatalf("change expected, got changed=%v err=%v", changed, err)
	}
	if a.Get("at-acbfr") != "two" {
		t.Errorf("after reload at=%q, want two", a.Get("at-acbfr"))
	}
}

func TestStoreSaveDoesNotClobberNewerFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	daemon := NewStore(path)
	daemon.Replace(map[string]string{"at-acbfr": "stale"})
	if err := daemon.Save(); err != nil {
		t.Fatal(err)
	}
	login := NewStore(path)
	login.Replace(map[string]string{"at-acbfr": "fresh"})
	if err := login.Save(); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(path, time.Now().Add(2*time.Second), time.Now().Add(2*time.Second))
	// The daemon saw a rotated cookie and wants to persist, unaware of the new login.
	daemon.Jar("amazon.fr").SetCookies(mustURL(t, "https://www.amazon.fr/"), []*http.Cookie{{Name: "session-id", Value: "rot"}})
	if err := daemon.Save(); err != nil {
		t.Fatal(err)
	}
	check := NewStore(path)
	if err := check.Load(); err != nil {
		t.Fatal(err)
	}
	if check.Get("at-acbfr") != "fresh" {
		t.Errorf("login cookies were overwritten: at=%q", check.Get("at-acbfr"))
	}
	if daemon.Get("at-acbfr") != "fresh" {
		t.Errorf("daemon did not pick up newer cookies: at=%q", daemon.Get("at-acbfr"))
	}
}
