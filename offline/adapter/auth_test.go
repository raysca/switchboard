package adapter

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireToken(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	for _, tc := range []struct {
		token, header string
		want          int
	}{
		{"", "", 204}, {"", "Bearer anything", 204}, // no token configured: any request passes
		{"s3", "Bearer s3", 204}, {"s3", "Bearer nope", 401}, {"s3", "", 401},
	} {
		r := httptest.NewRequest("GET", "/v1/stt", nil)
		if tc.header != "" {
			r.Header.Set("Authorization", tc.header)
		}
		w := httptest.NewRecorder()
		requireToken(tc.token, ok).ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("token=%q header=%q: got %d want %d", tc.token, tc.header, w.Code, tc.want)
		}
	}
}
