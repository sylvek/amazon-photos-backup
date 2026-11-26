package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"amazon-photos-backup/internal/amazon"
	"amazon-photos-backup/internal/amazon/amazontest"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func testConfig(t *testing.T, f *amazontest.Fake) Config {
	dir := t.TempDir()
	return Config{
		BackupDir: filepath.Join(dir, "photos"), StateDir: filepath.Join(dir, "state"),
		BaseURL: f.URL(), Interval: 40 * time.Millisecond, Concurrency: 2,
	}
}

func cookieHeader(m map[string]string) string {
	var parts []string
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	return "Cookie: " + strings.Join(parts, "; ") + "\n"
}

func login(t *testing.T, cfg Config, cookies map[string]string) {
	t.Helper()
	if err := Login(cfg, strings.NewReader(cookieHeader(cookies)), io.Discard); err != nil {
		t.Fatal(err)
	}
}

// waitFor polls until cond is true or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func stateIs(cfg Config, want string) func() bool {
	return func() bool {
		st, err := LoadStatus(cfg)
		return err == nil && st.State == want
	}
}

func startDaemon(t *testing.T, cfg Config) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	d := &Daemon{Config: cfg, Log: quiet, AuthPoll: 10 * time.Millisecond}
	go func() { done <- d.Run(ctx) }()
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("daemon returned %v on shutdown, want nil", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop")
		}
	}
	t.Cleanup(stop)
	return stop
}

func TestConfigFromEnv(t *testing.T) {
	for _, k := range []string{"BACKUP_DIR", "STATE_DIR", "SYNC_INTERVAL", "DOWNLOAD_CONCURRENCY", "AMAZON_TLD", "AMAZON_BASE_URL", "USER_AGENT", "NOTIFY_URL"} {
		t.Setenv(k, "")
	}
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BackupDir != "/data/photos" || cfg.StateDir != "/data/state" || cfg.Interval != 6*time.Hour || cfg.Concurrency != 4 {
		t.Errorf("defaults wrong: %+v", cfg)
	}
	t.Setenv("SYNC_INTERVAL", "90m")
	t.Setenv("DOWNLOAD_CONCURRENCY", "8")
	t.Setenv("AMAZON_TLD", "co.uk")
	cfg, err = ConfigFromEnv()
	if err != nil || cfg.Interval != 90*time.Minute || cfg.Concurrency != 8 || cfg.TLD != "co.uk" {
		t.Errorf("overrides wrong: %+v err=%v", cfg, err)
	}
	bad := map[string]string{
		"SYNC_INTERVAL":        "soon",
		"DOWNLOAD_CONCURRENCY": "0",
		"AMAZON_TLD":           "fr.evil.example/x",
		"NOTIFY_URL":           "ftp://example.com/x",
	}
	for k, v := range bad {
		t.Setenv("SYNC_INTERVAL", "6h")
		t.Setenv("DOWNLOAD_CONCURRENCY", "4")
		t.Setenv("AMAZON_TLD", "")
		t.Setenv("NOTIFY_URL", "")
		t.Setenv(k, v)
		if _, err := ConfigFromEnv(); err == nil {
			t.Errorf("%s=%q accepted, want error", k, v)
		}
	}
	t.Setenv("NOTIFY_URL", "")
	t.Setenv("SYNC_INTERVAL", "10s")
	if _, err := ConfigFromEnv(); err == nil {
		t.Errorf("a 10s interval was accepted; it would hammer Amazon")
	}
}

func TestLoginValidatesAndSaves(t *testing.T) {
	f := amazontest.New(t)
	cfg := testConfig(t, f)
	var out bytes.Buffer
	if err := Login(cfg, strings.NewReader(cookieHeader(f.Cookies())), &out); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(cfg.StateDir, "cookies.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("cookie file mode %v, want 0600", fi.Mode().Perm())
	}
	if strings.Contains(out.String(), "valid-token") {
		t.Errorf("login output echoes the secret token: %q", out.String())
	}
}

func TestLoginRejectsBadCookies(t *testing.T) {
	f := amazontest.New(t)
	cfg := testConfig(t, f)
	cookiePath := filepath.Join(cfg.StateDir, "cookies.json")

	expired := f.Cookies()
	expired["at-acbfr"] = "expired"
	if err := Login(cfg, strings.NewReader(cookieHeader(expired)), io.Discard); !errors.Is(err, amazon.ErrAuth) {
		t.Errorf("expired cookies: got %v, want ErrAuth", err)
	}
	incomplete := f.Cookies()
	delete(incomplete, "at-acbfr")
	err := Login(cfg, strings.NewReader(cookieHeader(incomplete)), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "at-acbfr") {
		t.Errorf("incomplete cookies: got %v, want an error naming at-acbfr", err)
	}
	if err := Login(cfg, strings.NewReader("   "), io.Discard); err == nil {
		t.Errorf("empty input accepted")
	}
	if _, err := os.Stat(cookiePath); err == nil {
		t.Errorf("rejected cookies were saved anyway")
	}
}

func TestLoginKeepsWorkingSessionWhenNewCookiesAreBad(t *testing.T) {
	f := amazontest.New(t)
	cfg := testConfig(t, f)
	login(t, cfg, f.Cookies())
	bad := f.Cookies()
	bad["at-acbfr"] = "typo"
	if err := Login(cfg, strings.NewReader(cookieHeader(bad)), io.Discard); err == nil {
		t.Fatal("bad cookies accepted")
	}
	store := amazon.NewStore(filepath.Join(cfg.StateDir, "cookies.json"))
	if err := store.Load(); err != nil || store.Get("at-acbfr") != "valid-token" {
		t.Errorf("working session was replaced by rejected cookies")
	}
}

func TestOnceBacksUp(t *testing.T) {
	f := amazontest.New(t)
	f.AddNode("id-a", "a.jpg", "2023-05-01T10:00:00Z", []byte("AAA"))
	cfg := testConfig(t, f)
	login(t, cfg, f.Cookies())
	res, err := Once(context.Background(), cfg, quiet)
	if err != nil || res.Downloaded != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if b, _ := os.ReadFile(filepath.Join(cfg.BackupDir, "2023", "05", "a.jpg")); string(b) != "AAA" {
		t.Errorf("file not backed up")
	}
	st, err := LoadStatus(cfg)
	if err != nil || st.State != StateOK || st.LastSuccess.IsZero() || st.LastResult.Downloaded != 1 {
		t.Errorf("status after success: %+v err=%v", st, err)
	}
}

func TestOnceWithoutCookies(t *testing.T) {
	f := amazontest.New(t)
	cfg := testConfig(t, f)
	if _, err := Once(context.Background(), cfg, quiet); !errors.Is(err, amazon.ErrAuth) {
		t.Fatalf("got %v, want ErrAuth", err)
	}
	if st, _ := LoadStatus(cfg); st.State != StateAuthRequired {
		t.Errorf("state = %q, want auth_required", st.State)
	}
}

func TestOncePersistsRotatedCookies(t *testing.T) {
	f := amazontest.New(t)
	f.RotateSessionID = true
	cfg := testConfig(t, f)
	login(t, cfg, f.Cookies())
	if _, err := Once(context.Background(), cfg, quiet); err != nil {
		t.Fatal(err)
	}
	store := amazon.NewStore(filepath.Join(cfg.StateDir, "cookies.json"))
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(store.Get("session-id"), "rot-") {
		t.Errorf("refreshed cookies were not written back: session-id=%q", store.Get("session-id"))
	}
}

func TestOnceReportsDegraded(t *testing.T) {
	f := amazontest.New(t)
	f.AddNode("id-a", "a.jpg", "2023-05-01T10:00:00Z", []byte("AAA"))
	f.AddNode("id-b", "b.jpg", "2023-05-01T10:00:00Z", []byte("BBB"))
	f.Forbidden["id-b"] = true
	cfg := testConfig(t, f)
	login(t, cfg, f.Cookies())
	res, err := Once(context.Background(), cfg, quiet)
	if err != nil || res.Failed != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if st, _ := LoadStatus(cfg); st.State != StateDegraded {
		t.Errorf("state = %q, want degraded", st.State)
	}
}

func TestCorruptIndexIsSetAside(t *testing.T) {
	f := amazontest.New(t)
	f.AddNode("id-a", "a.jpg", "2023-05-01T10:00:00Z", []byte("AAA"))
	cfg := testConfig(t, f)
	login(t, cfg, f.Cookies())
	os.WriteFile(filepath.Join(cfg.StateDir, "index.json"), []byte("{ truncated"), 0o644)
	res, err := Once(context.Background(), cfg, quiet)
	if err != nil || res.Downloaded != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	matches, _ := filepath.Glob(filepath.Join(cfg.StateDir, "index.json.corrupt-*"))
	if len(matches) != 1 {
		t.Errorf("corrupt index not kept aside: %v", matches)
	}
}

func TestRunRecoversAfterNewCookies(t *testing.T) {
	f := amazontest.New(t)
	f.AddNode("id-a", "a.jpg", "2023-05-01T10:00:00Z", []byte("AAA"))
	cfg := testConfig(t, f)

	startDaemon(t, cfg) // no cookies at all yet
	waitFor(t, "auth_required", stateIs(cfg, StateAuthRequired))

	login(t, cfg, f.Cookies())
	waitFor(t, "ok after login", stateIs(cfg, StateOK))
	if b, _ := os.ReadFile(filepath.Join(cfg.BackupDir, "2023", "05", "a.jpg")); string(b) != "AAA" {
		t.Errorf("file not backed up after login")
	}

	// New photos are picked up by later cycles without any intervention.
	f.AddNode("id-b", "b.jpg", "2023-06-01T10:00:00Z", []byte("BBB"))
	waitFor(t, "second photo", func() bool {
		_, err := os.Stat(filepath.Join(cfg.BackupDir, "2023", "06", "b.jpg"))
		return err == nil
	})
}

func TestRunSurvivesRestart(t *testing.T) {
	f := amazontest.New(t)
	f.AddNode("id-a", "a.jpg", "2023-05-01T10:00:00Z", []byte("AAA"))
	cfg := testConfig(t, f)
	login(t, cfg, f.Cookies())

	stop := startDaemon(t, cfg)
	waitFor(t, "first ok", stateIs(cfg, StateOK))
	stop()

	// "Reboot": a brand new daemon with nothing but the state directory.
	f.AddNode("id-b", "b.jpg", "2023-06-01T10:00:00Z", []byte("BBB"))
	startDaemon(t, cfg)
	waitFor(t, "photo added while down", func() bool {
		_, err := os.Stat(filepath.Join(cfg.BackupDir, "2023", "06", "b.jpg"))
		return err == nil
	})
	if n := f.DownloadCount("id-a"); n != 1 {
		t.Errorf("a.jpg downloaded %d times across the restart, want 1", n)
	}
}

func TestNotifyOncePerTransition(t *testing.T) {
	var posts atomic.Int32
	var lastBody atomic.Value
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		lastBody.Store(string(b))
		posts.Add(1)
	}))
	defer hook.Close()

	f := amazontest.New(t)
	cfg := testConfig(t, f)
	cfg.NotifyURL = hook.URL
	bad := f.Cookies()
	login(t, cfg, bad)
	f.Update(func(f *amazontest.Fake) { f.ValidAT = "rotated-by-amazon" }) // session revoked

	startDaemon(t, cfg)
	waitFor(t, "auth_required", stateIs(cfg, StateAuthRequired))
	time.Sleep(150 * time.Millisecond) // many poll rounds
	if n := posts.Load(); n != 1 {
		t.Fatalf("%d notifications for one expiry, want 1", n)
	}
	if body, _ := lastBody.Load().(string); strings.Contains(body, "valid-token") || body == "" {
		t.Errorf("notification body %q is empty or leaks the token", body)
	}

	login(t, cfg, f.Cookies())
	waitFor(t, "ok", stateIs(cfg, StateOK))
	f.Update(func(f *amazontest.Fake) { f.ValidAT = "rotated-again" })
	waitFor(t, "second expiry", stateIs(cfg, StateAuthRequired))
	waitFor(t, "second notification", func() bool { return posts.Load() == 2 })
}

func TestHealthcheck(t *testing.T) {
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		st      Status
		healthy bool
	}{
		{"ok", Status{State: StateOK, LastSuccess: now.Add(-time.Hour)}, true},
		{"degraded", Status{State: StateDegraded, LastSuccess: now.Add(-time.Hour)}, true},
		{"first sync running for days", Status{State: StateSyncing, Started: now.Add(-72 * time.Hour)}, true},
		{"auth required", Status{State: StateAuthRequired, LastSuccess: now.Add(-time.Minute)}, false},
		{"recent error", Status{State: StateError, LastSuccess: now.Add(-7 * time.Hour)}, true},
		{"errors for too long", Status{State: StateError, LastSuccess: now.Add(-19 * time.Hour)}, false},
		{"never worked", Status{State: StateError, Started: now.Add(-19 * time.Hour)}, false},
		{"just started failing", Status{State: StateError, Started: now.Add(-time.Hour)}, true},
	}
	for _, c := range cases {
		f := amazontest.New(t)
		cfg := testConfig(t, f)
		cfg.Interval = 6 * time.Hour
		if err := saveStatus(cfg, c.st); err != nil {
			t.Fatal(err)
		}
		err := Healthcheck(cfg, now)
		if (err == nil) != c.healthy {
			t.Errorf("%s: healthcheck error = %v, want healthy=%v", c.name, err, c.healthy)
		}
	}
	f := amazontest.New(t)
	if err := Healthcheck(testConfig(t, f), now); err == nil {
		t.Errorf("no status file at all reported healthy")
	}
}

func TestPreflightExplainsUnwritableDirs(t *testing.T) {
	f := amazontest.New(t)
	cfg := testConfig(t, f)
	if err := Preflight(cfg); err != nil {
		t.Fatalf("fresh directories: %v", err)
	}
	for _, d := range []string{cfg.BackupDir, cfg.StateDir} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Errorf("%s was not created", d)
		}
	}
	// A path that cannot be a directory stands in for a volume the
	// container user may not write to.
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, nil, 0o644)
	cfg.BackupDir = filepath.Join(blocker, "photos")
	err := Preflight(cfg)
	if err == nil || !strings.Contains(err.Error(), cfg.BackupDir) {
		t.Errorf("got %v, want an error naming %s", err, cfg.BackupDir)
	}
}

func TestNotifyRetriedWhenWebhookDown(t *testing.T) {
	// An alert lost because the webhook hiccuped would make the expiry silent.
	var calls atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			http.Error(w, "down", http.StatusBadGateway)
		}
	}))
	defer hook.Close()
	f := amazontest.New(t)
	cfg := testConfig(t, f)
	cfg.NotifyURL = hook.URL
	startDaemon(t, cfg) // no cookies: auth_required, retried every interval
	waitFor(t, "a delivered notification", func() bool { return calls.Load() >= 3 })
	time.Sleep(200 * time.Millisecond)
	if n := calls.Load(); n != 3 {
		t.Errorf("%d webhook calls, want 3 (two failures, one delivery, then silence)", n)
	}
}

func TestConcurrentBackupRefused(t *testing.T) {
	// `once` started while the daemon is syncing must not write the same files.
	f := amazontest.New(t)
	f.AddNode("id-a", "a.jpg", "2023-05-01T10:00:00Z", bytes.Repeat([]byte("A"), 100000))
	f.Stall["id-a"] = true // keeps the first run busy
	cfg := testConfig(t, f)
	login(t, cfg, f.Cookies())
	startDaemon(t, cfg)
	waitFor(t, "daemon downloading", func() bool { return f.TotalRequests() > 3 })
	_, err := Once(context.Background(), cfg, quiet)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("got %v, want ErrBusy", err)
	}
}
