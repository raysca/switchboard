package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"govoice/server/egress"
)

const (
	sdkResponseLimit = defaultMaxResultBytes + (64 << 10)
	sdkCloseBudget   = 300 * time.Millisecond
)

// SDKClient is the production remote Streamable HTTP adapter. It opens one
// bounded request-scoped session per operation and forwards only a resolved
// server-side bearer credential. Local/process execution is deliberately not
// implemented.
type SDKClient struct {
	client               *http.Client
	testOnlyAllowPrivate bool // Explicit loopback seam used only by the simulated-server tests.
	allowLoopbackHTTP    bool // mcp.allow_local_http: development and tests only.
}

// SDKOption configures NewSDKClient.
type SDKOption func(*SDKClient)

// AllowLoopbackHTTP lets the client reach http or https endpoints on loopback
// (config mcp.allow_local_http, refused in production). Other destinations keep
// the public-HTTPS rules, checked again when dialing.
func AllowLoopbackHTTP() SDKOption { return func(c *SDKClient) { c.allowLoopbackHTTP = true } }

func NewSDKClient(opts ...SDKOption) *SDKClient {
	c := &SDKClient{}
	for _, o := range opts {
		o(c)
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil // Explicit remote destination policy; do not inherit ambient proxy routing.
	base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
		return egress.Dial(ctx, network, address, c.allowLoopbackHTTP, net.DefaultResolver.LookupIPAddr, dialer.DialContext)
	}
	transport := closeBudgetTransport{next: boundedBodyTransport{next: classifiedStatusTransport{next: bearerTransport{next: base}}}}
	c.client = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return c
}

type bearerContextKey struct{}
type bearerTransport struct{ next http.RoundTripper }

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if bearer, ok := req.Context().Value(bearerContextKey{}).(string); ok && bearer != "" {
		clone := req.Clone(req.Context())
		clone.Header = clone.Header.Clone()
		clone.Header.Set("Authorization", "Bearer "+bearer)
		req = clone
	}
	return t.next.RoundTrip(req)
}

type boundedBodyTransport struct{ next http.RoundTripper }

func (t boundedBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rsp, err := t.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if rsp.Body != nil {
		rsp.Body = &limitedReadCloser{Reader: io.LimitReader(rsp.Body, sdkResponseLimit+1), Closer: rsp.Body}
	}
	return rsp, nil
}

type classifiedStatusTransport struct{ next http.RoundTripper }

func (t classifiedStatusTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rsp, err := t.next.RoundTrip(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, remoteError(ErrConnectionFailed)
	}
	if rsp.StatusCode == http.StatusUnauthorized || rsp.StatusCode == http.StatusForbidden {
		if rsp.Body != nil {
			rsp.Body.Close()
		}
		return nil, remoteError(ErrAuthenticationFailed)
	}
	if rsp.StatusCode == http.StatusNotFound {
		if rsp.Body != nil {
			rsp.Body.Close()
		}
		return nil, remoteError(ErrInitializeFailed)
	}
	if rsp.StatusCode >= 500 {
		if rsp.Body != nil {
			rsp.Body.Close()
		}
		return nil, remoteError(ErrConnectionFailed)
	}
	return rsp, nil
}

type closeBudgetTransport struct{ next http.RoundTripper }

func (t closeBudgetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch {
	case req.Method == http.MethodDelete:
		// Detached from the call: when a call times out or is cancelled, its context is
		// already done, and the DELETE must still reach the server so the session closes
		// and the tool running there stops. The small budget keeps teardown bounded.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), sdkCloseBudget)
		defer cancel()
		return t.next.RoundTrip(req.Clone(ctx))
	case isCancelNotification(req):
		// The SDK sends this after a timeout on a context of its own (seconds long);
		// against a server that never answers it would hold teardown that long.
		ctx, cancel := context.WithTimeout(req.Context(), sdkCloseBudget)
		defer cancel()
		return t.next.RoundTrip(req.Clone(ctx))
	}
	return t.next.RoundTrip(req)
}

// isCancelNotification reports whether req posts a notifications/cancelled message.
func isCancelNotification(req *http.Request) bool {
	if req.Method != http.MethodPost || req.GetBody == nil || req.ContentLength > 4096 {
		return false
	}
	body, err := req.GetBody()
	if err != nil {
		return false
	}
	defer body.Close()
	var msg struct {
		Method string `json:"method"`
	}
	return json.NewDecoder(io.LimitReader(body, 4096)).Decode(&msg) == nil && msg.Method == "notifications/cancelled"
}

type limitedReadCloser struct {
	io.Reader
	io.Closer
}

func (c *SDKClient) requestContext(ctx context.Context, token string, endpoint string) (context.Context, *mcp.ClientSession, error) {
	u, err := url.ParseRequestURI(endpoint)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, nil, errors.New("invalid remote MCP endpoint")
	}
	loopback := c.allowLoopbackHTTP && egress.LoopbackOnly(ctx, u.Hostname(), net.DefaultResolver.LookupIPAddr)
	if u.Scheme != "https" && !(loopback && u.Scheme == "http") {
		return nil, nil, errors.New("invalid remote MCP endpoint")
	}
	if !c.testOnlyAllowPrivate && !loopback {
		if err = egress.ValidatePublicHost(ctx, u.Hostname()); err != nil {
			return nil, nil, err
		}
	}
	requestCtx := context.WithValue(ctx, bearerContextKey{}, token)
	client := mcp.NewClient(&mcp.Implementation{Name: "go-voice", Version: "2"}, nil)
	transport := &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: c.client, MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: sdkResponseLimit}
	session, err := client.Connect(requestCtx, transport, nil)
	if err != nil {
		if errors.Is(err, ErrAuthenticationFailed) || errors.Is(err, ErrConnectionFailed) || errors.Is(err, ErrInitializeFailed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, nil, err
		}
		return nil, nil, remoteError(ErrInitializeFailed)
	}
	return requestCtx, session, nil
}

func (c *SDKClient) ListTools(ctx context.Context, endpoint, token string) ([]RemoteTool, error) {
	requestCtx, session, err := c.requestContext(ctx, token, endpoint)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	var result []RemoteTool
	for tool, err := range session.Tools(requestCtx, nil) {
		if err != nil {
			if errors.Is(err, ErrAuthenticationFailed) || errors.Is(err, ErrConnectionFailed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			return nil, remoteError(ErrDiscoveryFailed)
		}
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			return nil, errors.New("MCP schema decode failed")
		}
		result = append(result, RemoteTool{Name: tool.Name, Description: tool.Description, InputSchema: schema})
		if len(result) > maxToolsPerDiscovery {
			return nil, errors.New("MCP tool count exceeds limit")
		}
	}
	return result, nil
}

func (c *SDKClient) CallTool(ctx context.Context, endpoint, token, name string, args json.RawMessage) (RemoteResult, error) {
	requestCtx, session, err := c.requestContext(ctx, token, endpoint)
	if err != nil {
		return RemoteResult{}, err
	}
	defer session.Close()
	value, err := decodeJSON(args)
	arguments, ok := value.(map[string]any)
	if err != nil || !ok {
		return RemoteResult{}, errors.New("invalid tool arguments")
	}
	result, err := session.CallTool(requestCtx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		if errors.Is(err, ErrAuthenticationFailed) || errors.Is(err, ErrConnectionFailed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return RemoteResult{}, err
		}
		return RemoteResult{}, remoteError(ErrExecutionFailed)
	}
	if result == nil {
		return RemoteResult{}, errors.New("MCP returned an empty result")
	}
	if result.IsError {
		var texts []string
		for _, c := range result.Content {
			if t, ok := c.(*mcp.TextContent); ok {
				texts = append(texts, t.Text)
			}
		}
		return RemoteResult{}, &ToolError{Message: strings.Join(texts, "\n")}
	}
	if result.StructuredContent != nil {
		raw, err := json.Marshal(result.StructuredContent)
		if err != nil {
			return RemoteResult{}, errors.New("MCP result encoding failed")
		}
		return RemoteResult{JSON: raw}, nil
	}
	raw, err := json.Marshal(result.Content)
	if err != nil {
		return RemoteResult{}, errors.New("MCP result encoding failed")
	}
	return RemoteResult{JSON: raw}, nil
}
