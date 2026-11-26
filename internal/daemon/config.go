// Package daemon runs the backup on a schedule and keeps the session,
// status and notifications in order.
package daemon

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"amazon-photos-backup/internal/amazon"
)

type Config struct {
	BackupDir   string
	StateDir    string
	BaseURL     string // overrides the site derived from TLD; meant for tests
	TLD         string // fr, de, co.uk, com, ...; empty = derive from cookie names
	UserAgent   string
	NotifyURL   string
	Interval    time.Duration
	Concurrency int
}

var tldRE = regexp.MustCompile(`^[a-z]{2,3}(\.[a-z]{2})?$`)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ConfigFromEnv reads the configuration from environment variables.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		BackupDir: env("BACKUP_DIR", "/data/photos"),
		StateDir:  env("STATE_DIR", "/data/state"),
		BaseURL:   os.Getenv("AMAZON_BASE_URL"),
		TLD:       os.Getenv("AMAZON_TLD"),
		UserAgent: os.Getenv("USER_AGENT"),
		NotifyURL: os.Getenv("NOTIFY_URL"),
	}
	var err error
	if cfg.Interval, err = time.ParseDuration(env("SYNC_INTERVAL", "6h")); err != nil {
		return cfg, fmt.Errorf("SYNC_INTERVAL: %w (examples: 6h, 90m)", err)
	}
	if cfg.Interval < 5*time.Minute {
		return cfg, errors.New("SYNC_INTERVAL: must be at least 5m")
	}
	if cfg.Concurrency, err = strconv.Atoi(env("DOWNLOAD_CONCURRENCY", "4")); err != nil || cfg.Concurrency < 1 || cfg.Concurrency > 16 {
		return cfg, errors.New("DOWNLOAD_CONCURRENCY: must be a number between 1 and 16")
	}
	if cfg.TLD != "" && !tldRE.MatchString(cfg.TLD) {
		return cfg, fmt.Errorf("AMAZON_TLD: %q is not a domain suffix such as fr, de or co.uk", cfg.TLD)
	}
	if cfg.NotifyURL != "" {
		u, err := url.Parse(cfg.NotifyURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return cfg, errors.New("NOTIFY_URL: must be an http(s) URL")
		}
	}
	return cfg, nil
}

func (c Config) cookiePath() string { return filepath.Join(c.StateDir, "cookies.json") }
func (c Config) indexPath() string  { return filepath.Join(c.StateDir, "index.json") }
func (c Config) statusPath() string { return filepath.Join(c.StateDir, "status.json") }

// baseURL decides which Amazon site to talk to.
func (c Config) baseURL(cookies map[string]string) (string, error) {
	if c.BaseURL != "" {
		return c.BaseURL, nil
	}
	tld := c.TLD
	if tld == "" {
		detected, ok := amazon.DetectTLD(cookies)
		if !ok || !tldRE.MatchString(detected) {
			return "", errors.New("cannot tell which Amazon site these cookies belong to; set AMAZON_TLD (for example fr)")
		}
		tld = detected
	}
	return "https://www.amazon." + tld, nil
}

// Preflight makes sure both directories exist and can be written to, and
// says which one cannot: in a container that is almost always a volume
// owned by another user.
func Preflight(c Config) error {
	for _, dir := range []string{c.BackupDir, c.StateDir} {
		err := os.MkdirAll(dir, 0o755)
		if err == nil {
			var probe *os.File
			if probe, err = os.CreateTemp(dir, ".write-test-*"); err == nil {
				probe.Close()
				os.Remove(probe.Name())
			}
		}
		if err != nil {
			return fmt.Errorf("cannot write to %s: %w (check that the volume is owned by the user the container runs as, uid %d)", dir, err, os.Getuid())
		}
	}
	return nil
}
