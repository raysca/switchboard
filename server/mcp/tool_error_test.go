package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestErrorCategory(t *testing.T) {
	cases := []struct {
		err               error
		category, message string
	}{
		{&ToolError{Message: "no such order"}, "tool_error", "no such order"},
		{ErrInvalidArguments, "invalid_arguments", ""},
		{context.DeadlineExceeded, "timeout", ""},
		{context.Canceled, "cancelled", ""},
		{ErrResultTooLarge, "result_too_large", ""},
		{ErrRemoteFailure, "unavailable", ""},
		{errors.New("boom"), "unavailable", ""},
	}
	for _, c := range cases {
		if cat, msg := ErrorCategory(c.err); cat != c.category || msg != c.message {
			t.Errorf("%v: got %q %q, want %q %q", c.err, cat, msg, c.category, c.message)
		}
	}
	if strings.Contains((&ToolError{Message: "secret caller data"}).Error(), "secret") {
		t.Error("ToolError.Error() leaks the message")
	}
}

func TestCleanToolMessage(t *testing.T) {
	if got := cleanToolMessage("a\nb\tc\x00d\x1be\u0085f", ""); got != "a b cdef" {
		t.Errorf("control characters: %q", got)
	}
	if got := cleanToolMessage("bad tok-123 here tok-123", "tok-123"); got != "bad [redacted] here [redacted]" {
		t.Errorf("token: %q", got)
	}
	if got := cleanToolMessage(strings.Repeat("x", 10<<10), ""); len(got) != 512 {
		t.Errorf("cap: %d", len(got))
	}
	got := cleanToolMessage(strings.Repeat("é", 400), "") // 2 bytes each: 512 is a rune boundary
	if len(got) > 512 || !utf8.ValidString(got) || len(got) != 512 {
		t.Errorf("multibyte cap: %d valid=%v", len(got), utf8.ValidString(got))
	}
	got = cleanToolMessage("a"+strings.Repeat("é", 400), "") // cut would land mid-rune
	if len(got) != 511 || !utf8.ValidString(got) {           // 1 + 255*2: the 256th rune would end at 513
		t.Errorf("mid-rune cut: %d valid=%v", len(got), utf8.ValidString(got))
	}
	for _, sep := range []string{"\x00", "\x1b", "\u0085"} { // control characters inside the token must not hide it
		if got := cleanToolMessage("key tok"+sep+"-123 end", "tok-123"); strings.Contains(got, "tok-123") || got != "key [redacted] end" {
			t.Errorf("token split by %q: %q", sep, got)
		}
	}
}

func TestSDKClientReturnsToolErrorWithTheToolsText(t *testing.T) {
	remote := mcp.NewServer(&mcp.Implementation{Name: "failing", Version: "1"}, nil)
	mcp.AddTool[map[string]any, map[string]any](remote, &mcp.Tool{Name: "lookup", Description: "x", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, map[string]any, error) {
		return nil, nil, errors.New("no such order")
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, &mcp.StreamableHTTPOptions{Stateless: true})
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	client := &SDKClient{client: &http.Client{Transport: boundedBodyTransport{next: bearerTransport{next: server.Client().Transport}}}, testOnlyAllowPrivate: true}
	_, err := client.CallTool(context.Background(), server.URL, "", "lookup", json.RawMessage(`{}`))
	var te *ToolError
	if !errors.As(err, &te) || te.Message != "no such order" {
		t.Fatalf("err=%v", err)
	}
}

func TestExecuteCleansTheToolErrorMessage(t *testing.T) {
	const credential = "credential-secret-sentinel"
	remote := &testRemote{tools: []RemoteTool{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	svc, _ := newTestService(t, remote, testTokens{"MCP_TOKEN_CRM": credential})
	server := createTestServer(t, svc, "https://mcp.example/rpc", "MCP_TOKEN_CRM")
	if _, err := svc.Refresh(context.Background(), server.ID); err != nil {
		t.Fatal(err)
	}
	remote.call = func(context.Context, string, json.RawMessage) (RemoteResult, error) {
		return RemoteResult{}, &ToolError{Message: "bad key " + credential + "\nline two"}
	}
	_, err := svc.Execute(context.Background(), server.ID, "lookup", json.RawMessage(`{}`))
	cat, msg := ErrorCategory(err)
	if cat != "tool_error" || msg != "bad key [redacted] line two" || strings.Contains(err.Error(), "bad key") {
		t.Fatalf("cat=%q msg=%q err=%v", cat, msg, err)
	}
	remote.call = func(ctx context.Context, _ string, _ json.RawMessage) (RemoteResult, error) {
		return RemoteResult{}, context.DeadlineExceeded
	}
	_, err = svc.Execute(context.Background(), server.ID, "lookup", json.RawMessage(`{}`))
	if cat, _ := ErrorCategory(err); cat != "timeout" || !errors.Is(err, ErrRemoteFailure) {
		t.Fatalf("cat=%q err=%v", cat, err)
	}
}
