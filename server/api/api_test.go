package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIDecodeJSONRejectsUnknownTrailingAndOversize(t *testing.T) {
	for _, body := range []string{`{"name":"ok","extra":1}`, `{"name":"ok"} {}`, strings.Repeat("x", MaxBodyBytes+1)} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		var v struct {
			Name string `json:"name"`
		}
		if err := DecodeJSON(r, &v); err == nil {
			t.Fatalf("accepted %d-byte invalid body", len(body))
		}
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"ok"}`))
	var v struct {
		Name string `json:"name"`
	}
	if err := DecodeJSON(r, &v); err == nil {
		t.Fatal("accepted missing content type")
	}
}

func TestAPIPaginationBounds(t *testing.T) {
	r := httptest.NewRequest("GET", "/?limit=1000&offset=-1", nil)
	if _, err := ParsePagination(r); err == nil {
		t.Fatal("accepted invalid pagination")
	}
	r = httptest.NewRequest("GET", "/?limit=20&offset=4", nil)
	p, err := ParsePagination(r)
	if err != nil || p.Limit != 20 || p.Offset != 4 {
		t.Fatalf("pagination=%+v err=%v", p, err)
	}
}
