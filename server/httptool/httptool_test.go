package httptool

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"govoice/server/egress"
	"govoice/server/mcp"
)

const orderSchema = `{"type":"object","properties":{"order_id":{"type":"string"},"verbose":{"type":"boolean"},"tags":{"type":"array","items":{"type":"string"}}},"required":["order_id"]}`

func def(url string) Definition {
	return Definition{Name: "get_order", Description: "Look up an order by id.", Method: "GET", URL: url + "/orders/{order_id}", InputSchema: json.RawMessage(orderSchema)}
}

func normalized(t *testing.T, d Definition) Definition {
	t.Helper()
	n, err := d.Normalize(t.Context(), Options{AllowLocalHTTP: true})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return n
}

func TestNormalizeRejectsUnsafeDefinitions(t *testing.T) {
	base := func() Definition { return def("https://api.example.com") }
	cases := map[string]func(*Definition){
		"name":               func(d *Definition) { d.Name = "Get Order" },
		"no description":     func(d *Definition) { d.Description = " " },
		"method":             func(d *Definition) { d.Method = "TRACE" },
		"http":               func(d *Definition) { d.URL = "http://api.example.com/orders/{order_id}" },
		"userinfo":           func(d *Definition) { d.URL = "https://u:p@api.example.com/orders/{order_id}" },
		"query":              func(d *Definition) { d.URL = "https://api.example.com/orders/{order_id}?k=v" },
		"fragment":           func(d *Definition) { d.URL = "https://api.example.com/orders/{order_id}#x" },
		"host placeholder":   func(d *Definition) { d.URL = "https://{order_id}.example.com/orders" },
		"stray brace":        func(d *Definition) { d.URL = "https://api.example.com/orders/{Order}" },
		"duplicate":          func(d *Definition) { d.URL = "https://api.example.com/{order_id}/{order_id}" },
		"placeholder prop":   func(d *Definition) { d.URL = "https://api.example.com/{missing}" },
		"placeholder type":   func(d *Definition) { d.URL = "https://api.example.com/{verbose}" },
		"placeholder opt":    func(d *Definition) { d.URL = "https://api.example.com/{tags}" },
		"static auth header": func(d *Definition) { d.Headers = map[string]string{"authorization": "Bearer x"} },
		"cookie":             func(d *Definition) { d.Headers = map[string]string{"Cookie": "a=b"} },
		"host header":        func(d *Definition) { d.Headers = map[string]string{"Host": "evil"} },
		"header newline":     func(d *Definition) { d.Headers = map[string]string{"X-A": "a\r\nX-B: b"} },
		"auth mcp secret":    func(d *Definition) { d.Auth = &Auth{Secret: "MCP_TOKEN_CRM"} },
		"auth bad secret":    func(d *Definition) { d.Auth = &Auth{Secret: "GOVOICE_MANAGEMENT_API_KEY"} },
		"auth host header":   func(d *Definition) { d.Auth = &Auth{Header: "Host", Secret: "crm-api"} },
		"auth clash": func(d *Definition) {
			d.Auth = &Auth{Header: "X-Key", Secret: "crm-api"}
			d.Headers = map[string]string{"x-key": "v"}
		},
		"schema not object": func(d *Definition) {
			d.URL = "https://api.example.com/x"
			d.InputSchema = json.RawMessage(`{"type":"string"}`)
		},
		"schema missing": func(d *Definition) { d.URL = "https://api.example.com/x"; d.InputSchema = nil },
		"timeout":        func(d *Definition) { d.TimeoutMS = 999999 },
		"response size":  func(d *Definition) { d.MaxResponseBytes = 1 << 30 },
		"pick":           func(d *Definition) { d.Pick = []string{"data"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := base()
			mutate(&d)
			if _, err := d.Normalize(t.Context(), Options{}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("accepted: %v", err)
			}
		})
	}
	if _, err := base().Normalize(t.Context(), Options{}); err != nil {
		t.Fatalf("the base definition is valid: %v", err)
	}
	local := def("http://127.0.0.1:9")
	if _, err := local.Normalize(t.Context(), Options{}); err == nil {
		t.Fatal("http accepted without the development switch")
	}
	if _, err := local.Normalize(t.Context(), Options{AllowLocalHTTP: true}); err != nil {
		t.Fatalf("http with the switch: %v", err)
	}
}

func TestHashIsStableAndSensitiveToEveryField(t *testing.T) {
	a := normalized(t, def("https://api.example.com"))
	h1, _, _ := a.Hash()
	h2, _, _ := normalized(t, def("https://api.example.com")).Hash()
	if h1 != h2 {
		t.Fatal("same definition, different hash")
	}
	for name, mutate := range map[string]func(*Definition){
		"description": func(d *Definition) { d.Description += "!" },
		"timeout":     func(d *Definition) { d.TimeoutMS = 5000 },
		"header":      func(d *Definition) { d.Headers = map[string]string{"X-A": "1"} },
		"auth":        func(d *Definition) { d.Auth = &Auth{Secret: "crm-api"} },
		"pick":        func(d *Definition) { d.Pick = []string{"/a"} },
	} {
		d := def("https://api.example.com")
		mutate(&d)
		h, _, _ := normalized(t, d).Hash()
		if h == h1 {
			t.Errorf("%s did not change the hash", name)
		}
	}
}

type fixture struct {
	srv  *httptest.Server
	last *http.Request
	body string
}

func newFixture(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) *fixture {
	f := &fixture{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.last, f.body = r, string(b)
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func exec(t *testing.T, d Definition, args string, secrets map[string]string) (json.RawMessage, error) {
	t.Helper()
	e := &Executor{Client: egress.NewClient(true), Secret: func(_ context.Context, n string) (string, bool) { v, ok := secrets[n]; return v, ok }}
	return e.Execute(t.Context(), normalized(t, d), []byte(args))
}

func jsonReply(v string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, v)
	}
}

func TestGetFillsPathAndQueryAndSendsAuth(t *testing.T) {
	f := newFixture(t, jsonReply(`{"id":"A-1","status":"shipped"}`))
	d := def(f.srv.URL)
	d.Auth = &Auth{Secret: "crm-api"}
	d.Headers = map[string]string{"x-tenant": "acme"}
	out, err := exec(t, d, `{"order_id":"A 1","verbose":true,"tags":["x","y"]}`, map[string]string{"crm-api": "tok-123"})
	if err != nil {
		t.Fatal(err)
	}
	if f.last.URL.EscapedPath() != "/orders/A%201" || f.last.URL.Query().Get("verbose") != "true" || len(f.last.URL.Query()["tags"]) != 2 {
		t.Fatalf("request = %s", f.last.URL)
	}
	if f.last.Header.Get("Authorization") != "Bearer tok-123" || f.last.Header.Get("X-Tenant") != "acme" || f.last.Method != "GET" {
		t.Fatalf("headers = %v", f.last.Header)
	}
	var got struct {
		Status int
		Body   map[string]any
	}
	_ = json.Unmarshal(out, &got)
	if got.Status != 200 || got.Body["status"] != "shipped" {
		t.Fatalf("result = %s", out)
	}
}

func TestPostSendsTheRemainingArgumentsAsJSONAndACustomAuthHeader(t *testing.T) {
	f := newFixture(t, jsonReply(`{"ok":true}`))
	d := Definition{Name: "add_note", Description: "Add a note.", Method: "POST", URL: f.srv.URL + "/orders/{order_id}/notes", InputSchema: json.RawMessage(`{"type":"object","properties":{"order_id":{"type":"integer"},"text":{"type":"string"}},"required":["order_id","text"]}`), Auth: &Auth{Header: "X-Api-Key", Secret: "HTTP_TOKEN_NOTES"}}
	if _, err := exec(t, d, `{"order_id":42,"text":"call back"}`, map[string]string{"HTTP_TOKEN_NOTES": "key-9"}); err != nil {
		t.Fatal(err)
	}
	if f.last.URL.Path != "/orders/42/notes" || f.last.Header.Get("X-Api-Key") != "key-9" || f.last.Header.Get("Authorization") != "" || f.last.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("request = %s %v", f.last.URL, f.last.Header)
	}
	if f.body != `{"text":"call back"}` {
		t.Fatalf("body = %s", f.body)
	}
}

func TestPathValuesCannotChangeTheShapeOfThePath(t *testing.T) {
	f := newFixture(t, jsonReply(`{}`))
	for _, bad := range []string{"..", ".", "a/b", `a\\b`, "", "a\nb"} {
		args, _ := json.Marshal(map[string]string{"order_id": bad})
		if _, err := exec(t, def(f.srv.URL), string(args), nil); !errors.Is(err, mcp.ErrInvalidArguments) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if f.last != nil {
		t.Fatalf("a rejected value still reached the service: %s", f.last.URL)
	}
}

func TestInvalidArgumentsAreRefusedBeforeAnyRequest(t *testing.T) {
	f := newFixture(t, jsonReply(`{}`))
	for _, args := range []string{`{}`, `{"order_id":1}`, `{"order_id":"a","verbose":"yes"}`, `[]`, `not json`} {
		if _, err := exec(t, def(f.srv.URL), args, nil); !errors.Is(err, mcp.ErrInvalidArguments) {
			t.Errorf("%s: %v", args, err)
		}
	}
	if f.last != nil {
		t.Fatal("a request was made")
	}
}

func TestStatusContentTypeAndSizeFailures(t *testing.T) {
	cases := []struct {
		name  string
		reply func(http.ResponseWriter, *http.Request)
		check func(error) bool
	}{
		{"404", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "secret internals", 404) }, func(err error) bool {
			var te *mcp.ToolError
			return errors.As(err, &te) && te.Message == "the service answered HTTP 404"
		}},
		{"redirect not followed", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "http://127.0.0.1:1/x", 302) }, func(err error) bool {
			var te *mcp.ToolError
			return errors.As(err, &te) && strings.Contains(te.Message, "302")
		}},
		{"html", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<b>x</b>")
		}, func(err error) bool {
			var te *mcp.ToolError
			return errors.As(err, &te) && strings.Contains(te.Message, "content type")
		}},
		{"bad json", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "{")
		}, func(err error) bool {
			var te *mcp.ToolError
			return errors.As(err, &te) && strings.Contains(te.Message, "invalid JSON")
		}},
		{"too large", jsonReply(`{"pad":"` + strings.Repeat("x", 600) + `"}`), func(err error) bool { return errors.Is(err, mcp.ErrResultTooLarge) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, c.reply)
			d := def(f.srv.URL)
			d.MaxResponseBytes = 256
			if _, err := exec(t, d, `{"order_id":"1"}`, nil); !c.check(err) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestTimeoutAndCancellation(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	d := def(f.srv.URL)
	d.TimeoutMS = 150
	started := time.Now()
	if _, err := exec(t, d, `{"order_id":"1"}`, nil); !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 2*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(started))
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	e := &Executor{Client: egress.NewClient(true), Secret: func(context.Context, string) (string, bool) { return "", false }}
	if _, err := e.Execute(ctx, normalized(t, def(f.srv.URL)), []byte(`{"order_id":"1"}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

func TestMissingSecretAndUnreachableHostNeverLeakDetails(t *testing.T) {
	d := def("http://127.0.0.1:1")
	d.Auth = &Auth{Secret: "crm-api"}
	if _, err := exec(t, d, `{"order_id":"1"}`, nil); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("missing secret: %v", err)
	}
	_, err := exec(t, d, `{"order_id":"1"}`, map[string]string{"crm-api": "tok"})
	if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("unreachable: %v", err)
	}
	if cat, msg := mcp.ErrorCategory(err); cat != "unavailable" || msg != "" {
		t.Fatalf("category = %q %q", cat, msg)
	}
}

func TestResultHasTheCredentialRedactedAndPickKeepsOnlyNamedParts(t *testing.T) {
	f := newFixture(t, jsonReply(`{"echo":"Bearer tok-777","data":{"status":"open","internal":"x"},"items":[{"n":1},{"n":2}]}`))
	d := def(f.srv.URL)
	d.Auth = &Auth{Secret: "crm-api"}
	out, err := exec(t, d, `{"order_id":"1"}`, map[string]string{"crm-api": "tok-777"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "tok-777") {
		t.Fatalf("credential in result: %s", out)
	}
	d.Pick = []string{"/data/status", "/items/1/n", "/missing"}
	out, err = exec(t, d, `{"order_id":"1"}`, map[string]string{"crm-api": "tok-777"})
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Body map[string]any }
	_ = json.Unmarshal(out, &got)
	if len(got.Body) != 2 || got.Body["/data/status"] != "open" || got.Body["/items/1/n"].(float64) != 2 {
		t.Fatalf("pick = %s", out)
	}
}

func TestEscapedCharactersDoNotCountAgainstTheResponseLimit(t *testing.T) {
	body := `{"t":"` + strings.Repeat("<", 150) + `"}` // 158 bytes on the wire, 6 bytes each once escaped
	f := newFixture(t, jsonReply(body))
	d := def(f.srv.URL)
	d.MaxResponseBytes = 256
	if _, err := exec(t, d, `{"order_id":"1"}`, nil); err != nil {
		t.Fatalf("an answer under the limit failed: %v", err)
	}
}

func TestAnMCPSecretIsNeverSentEvenFromAnOldRow(t *testing.T) {
	f := newFixture(t, jsonReply(`{}`))
	d := normalized(t, def(f.srv.URL))
	d.Auth = &Auth{Header: "Authorization", Secret: "MCP_TOKEN_CRM"} // as if written without Normalize
	e := &Executor{Client: egress.NewClient(true), Secret: func(context.Context, string) (string, bool) { return "tok", true }}
	if _, err := e.Execute(t.Context(), d, []byte(`{"order_id":"1"}`)); !errors.Is(err, ErrSecretUnavailable) || f.last != nil {
		t.Fatalf("err = %v, request made = %v", err, f.last != nil)
	}
}

func TestTextBodiesAndEmptyBodies(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/orders/empty" {
			w.WriteHeader(204)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "plain answer")
	})
	out, err := exec(t, def(f.srv.URL), `{"order_id":"t"}`, nil)
	if err != nil || !strings.Contains(string(out), `"body":"plain answer"`) {
		t.Fatalf("text: %s %v", out, err)
	}
	out, err = exec(t, def(f.srv.URL), `{"order_id":"empty"}`, nil)
	if err != nil || !strings.Contains(string(out), `"body":null`) || !strings.Contains(string(out), `"status":204`) {
		t.Fatalf("empty: %s %v", out, err)
	}
}
