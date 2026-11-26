package amazon_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amazon-photos-backup/internal/amazon"
	"amazon-photos-backup/internal/amazon/amazontest"
)

func newClient(t *testing.T, f *amazontest.Fake, cookies map[string]string) (*amazon.Client, *amazon.Store) {
	t.Helper()
	store := amazon.NewStore(filepath.Join(t.TempDir(), "cookies.json"))
	store.Replace(cookies)
	c, err := amazon.NewClient(amazon.Options{BaseURL: f.URL(), Store: store, Retries: 3, RetryBase: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return c, store
}

func TestRootOK(t *testing.T) {
	f := amazontest.New(t)
	c, _ := newClient(t, f, f.Cookies())
	root, err := c.Root(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if root.OwnerID != "OWNER1" {
		t.Errorf("root owner = %q, want OWNER1", root.OwnerID)
	}
	reqs := f.RequestLog()
	last := reqs[len(reqs)-1]
	if got := last.Header.Get("x-amzn-sessionid"); got != "111-2222222-3333333" {
		t.Errorf("x-amzn-sessionid = %q", got)
	}
	if ua := last.Header.Get("User-Agent"); !strings.Contains(ua, "Mozilla") {
		t.Errorf("User-Agent = %q, want a browser-like agent", ua)
	}
	// Root is cached: a second call costs no request.
	if _, err := c.Root(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(f.RequestLog()); n != len(reqs) {
		t.Errorf("second Root made %d extra requests", n-len(reqs))
	}
}

func TestAuth401(t *testing.T) {
	f := amazontest.New(t)
	bad := f.Cookies()
	bad["at-acbfr"] = "expired"
	c, _ := newClient(t, f, bad)
	_, err := c.Root(context.Background())
	if !errors.Is(err, amazon.ErrAuth) {
		t.Fatalf("got %v, want ErrAuth", err)
	}
	if n := len(f.RequestLog()); n != 1 {
		t.Errorf("auth failure was retried: %d requests", n)
	}
}

func TestAuthSigninRedirect(t *testing.T) {
	f := amazontest.New(t)
	f.SigninRedirect = true
	bad := f.Cookies()
	bad["at-acbfr"] = "expired"
	c, _ := newClient(t, f, bad)
	if _, err := c.Root(context.Background()); !errors.Is(err, amazon.ErrAuth) {
		t.Fatalf("got %v, want ErrAuth", err)
	}
}

func TestAuthHTMLInsteadOfJSON(t *testing.T) {
	f := amazontest.New(t)
	f.HTMLInsteadOfJSON = true
	c, _ := newClient(t, f, f.Cookies())
	if _, err := c.Root(context.Background()); !errors.Is(err, amazon.ErrAuth) {
		t.Fatalf("got %v, want ErrAuth", err)
	}
}

func TestKeepAliveAuth(t *testing.T) {
	f := amazontest.New(t)
	c, _ := newClient(t, f, f.Cookies())
	if err := c.KeepAlive(context.Background()); err != nil {
		t.Fatalf("valid session: %v", err)
	}
	bad := f.Cookies()
	bad["at-acbfr"] = "expired"
	c2, _ := newClient(t, f, bad)
	if err := c2.KeepAlive(context.Background()); !errors.Is(err, amazon.ErrAuth) {
		t.Fatalf("got %v, want ErrAuth", err)
	}
}

func TestRetryOn503ThenOK(t *testing.T) {
	f := amazontest.New(t)
	f.Fail503 = 2
	c, _ := newClient(t, f, f.Cookies())
	if _, err := c.Root(context.Background()); err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if n := len(f.RequestLog()); n != 3 {
		t.Errorf("made %d requests, want 3", n)
	}
}

func TestRetryGivesUp(t *testing.T) {
	f := amazontest.New(t)
	f.Fail503 = 100
	c, _ := newClient(t, f, f.Cookies())
	_, err := c.Root(context.Background())
	if err == nil || errors.Is(err, amazon.ErrAuth) {
		t.Fatalf("got %v, want a non-auth error", err)
	}
	if n := len(f.RequestLog()); n != 4 {
		t.Errorf("made %d requests, want 1 + 3 retries", n)
	}
}

func TestSetCookieRotationPersisted(t *testing.T) {
	f := amazontest.New(t)
	f.RotateSessionID = true
	c, store := newClient(t, f, f.Cookies())
	if err := c.KeepAlive(context.Background()); err != nil {
		t.Fatal(err)
	}
	rotated := store.Get("session-id")
	if !strings.HasPrefix(rotated, "rot-") {
		t.Fatalf("session-id = %q, want the rotated value", rotated)
	}
	if _, err := c.Root(context.Background()); err != nil {
		t.Fatal(err)
	}
	reqs := f.RequestLog()
	if got := reqs[len(reqs)-1].Header.Get("x-amzn-sessionid"); got != rotated {
		t.Errorf("x-amzn-sessionid = %q, want rotated %q", got, rotated)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	again := amazon.NewStore(filepath.Join(filepath.Dir(storePath(t, store)), "cookies.json"))
	if err := again.Load(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(again.Get("session-id"), "rot-") || again.Get("at-acbfr") != "valid-token" {
		t.Errorf("persisted cookies wrong: %v", again.Snapshot())
	}
}

func storePath(t *testing.T, s *amazon.Store) string {
	t.Helper()
	return s.Path()
}
