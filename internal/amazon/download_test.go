package amazon_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"amazon-photos-backup/internal/amazon"
	"amazon-photos-backup/internal/amazon/amazontest"
)

func TestDownloadFollowsRedirectWithoutCookies(t *testing.T) {
	f := amazontest.New(t)
	want := bytes.Repeat([]byte("photo-bytes "), 5000)
	n := f.AddNode("abc", "a.jpg", "2023-05-01T10:00:00Z", want)
	c, _ := newClient(t, f, f.Cookies())
	var buf bytes.Buffer
	written, err := c.Download(context.Background(), n, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if written != int64(len(want)) || !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("downloaded %d bytes, want %d identical bytes", written, len(want))
	}
	// The fake fails the test itself if cookies reached the content host.
}

func TestDownloadUsesRootOwnerWhenNodeHasNone(t *testing.T) {
	f := amazontest.New(t)
	n := f.AddNode("abc", "a.jpg", "2023-05-01T10:00:00Z", []byte("x"))
	n.OwnerID = ""
	c, _ := newClient(t, f, f.Cookies())
	var buf bytes.Buffer
	if _, err := c.Download(context.Background(), n, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "x" {
		t.Errorf("got %q", buf.String())
	}
}

func TestDownloadAuthError(t *testing.T) {
	f := amazontest.New(t)
	n := f.AddNode("abc", "a.jpg", "2023-05-01T10:00:00Z", []byte("x"))
	bad := f.Cookies()
	bad["at-acbfr"] = "expired"
	c, _ := newClient(t, f, bad)
	if _, err := c.Download(context.Background(), n, &bytes.Buffer{}); !errors.Is(err, amazon.ErrAuth) {
		t.Fatalf("got %v, want ErrAuth", err)
	}
}

func TestDownload403IsNotAuth(t *testing.T) {
	f := amazontest.New(t)
	n := f.AddNode("abc", "a.jpg", "2023-05-01T10:00:00Z", []byte("x"))
	f.Forbidden["abc"] = true
	c, _ := newClient(t, f, f.Cookies())
	_, err := c.Download(context.Background(), n, &bytes.Buffer{})
	if err == nil || errors.Is(err, amazon.ErrAuth) {
		t.Fatalf("got %v, want a plain per-file error", err)
	}
}

func TestDownloadTruncatedIsError(t *testing.T) {
	f := amazontest.New(t)
	n := f.AddNode("abc", "a.jpg", "2023-05-01T10:00:00Z", bytes.Repeat([]byte("z"), 100000))
	f.Truncate["abc"] = true
	c, _ := newClient(t, f, f.Cookies())
	if _, err := c.Download(context.Background(), n, &bytes.Buffer{}); err == nil {
		t.Fatal("truncated body was reported as a successful download")
	}
}

func TestDownloadStallTimesOut(t *testing.T) {
	f := amazontest.New(t)
	n := f.AddNode("abc", "a.jpg", "2023-05-01T10:00:00Z", bytes.Repeat([]byte("z"), 100000))
	f.Stall["abc"] = true
	store := amazon.NewStore(filepath.Join(t.TempDir(), "cookies.json"))
	store.Replace(f.Cookies())
	c, err := amazon.NewClient(amazon.Options{BaseURL: f.URL(), Store: store, IdleTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.Download(context.Background(), n, &bytes.Buffer{})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled download reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled download was never cut")
	}
}
