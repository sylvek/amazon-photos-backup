package amazon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// Amazon's HTTP/2 servers send frames after closing streams, which Go logs
// as protocol errors. The client therefore speaks HTTP/1.1, as the
// reference implementation of this API does.
func TestClientUsesHTTP1(t *testing.T) {
	var proto string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proto = r.Proto
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"count":1,"data":[{"id":"ROOT","ownerId":"O"}]}`))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	store := NewStore(filepath.Join(t.TempDir(), "c.json"))
	c, err := NewClient(Options{BaseURL: srv.URL, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	tr := c.http.Transport.(*http.Transport)
	tr.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	if _, err := c.Root(context.Background()); err != nil {
		t.Fatal(err)
	}
	if proto != "HTTP/1.1" {
		t.Errorf("client spoke %s, want HTTP/1.1", proto)
	}
}
