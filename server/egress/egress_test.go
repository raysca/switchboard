package egress_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"govoice/server/egress"
)

func TestClientRefusesPrivateAddressesUnlessLoopbackIsAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	if _, err := egress.NewClient(false).Get(srv.URL); err == nil {
		t.Fatal("a loopback server was reachable without the development switch")
	}
	resp, err := egress.NewClient(true).Get(srv.URL)
	if err != nil {
		t.Fatalf("loopback with the switch: %v", err)
	}
	resp.Body.Close()
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	resp, err := egress.NewClient(true).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || hits != 1 {
		t.Fatalf("status %d after %d requests", resp.StatusCode, hits)
	}
}
