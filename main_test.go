package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"amazon-photos-backup/internal/amazon/amazontest"
)

// TestCLIEndToEnd drives the real commands the way the container does:
// login, a backup, status and healthcheck, all configured through the
// environment.
func TestCLIEndToEnd(t *testing.T) {
	f := amazontest.New(t)
	f.AddNode("id-a", "a.jpg", "2023-05-01T10:00:00Z", []byte("AAA"))
	dir := t.TempDir()
	t.Setenv("BACKUP_DIR", filepath.Join(dir, "photos"))
	t.Setenv("STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("AMAZON_BASE_URL", f.URL())
	for _, k := range []string{"SYNC_INTERVAL", "DOWNLOAD_CONCURRENCY", "AMAZON_TLD", "NOTIFY_URL", "USER_AGENT", "LOG_LEVEL"} {
		t.Setenv(k, "")
	}

	if code := run([]string{"healthcheck"}); code == 0 {
		t.Errorf("healthcheck before any run exited 0")
	}
	if code := run([]string{"once"}); code != 1 {
		t.Errorf("once without login exited %d, want 1", code)
	}
	if code := run([]string{"healthcheck"}); code == 0 {
		t.Errorf("healthcheck while logged out exited 0")
	}

	var parts []string
	for k, v := range f.Cookies() {
		parts = append(parts, k+"="+v)
	}
	cookieFile := filepath.Join(dir, "cookies.txt")
	os.WriteFile(cookieFile, []byte(strings.Join(parts, "; ")), 0o600)
	if code := run([]string{"login", "--file", cookieFile}); code != 0 {
		t.Fatalf("login exited %d", code)
	}
	if code := run([]string{"once"}); code != 0 {
		t.Fatalf("once exited %d", code)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "photos", "2023", "05", "a.jpg")); string(b) != "AAA" {
		t.Errorf("photo not backed up")
	}
	if code := run([]string{"status"}); code != 0 {
		t.Errorf("status exited %d", code)
	}
	if code := run([]string{"healthcheck"}); code != 0 {
		t.Errorf("healthcheck after a good backup exited %d", code)
	}
	if code := run([]string{"frobnicate"}); code != 2 {
		t.Errorf("unknown command exited %d, want 2", code)
	}
	t.Setenv("SYNC_INTERVAL", "never")
	if code := run([]string{"once"}); code != 2 {
		t.Errorf("bad configuration exited %d, want 2", code)
	}
}
