package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
}

func NewSDKClient() *SDKClient {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil // Explicit remote destination policy; do not inherit ambient proxy routing.
	base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
		return dialPublicEgress(ctx, network, address, net.DefaultResolver.LookupIPAddr, dialer.DialContext)
	}
	transport := closeBudgetTransport{next: boundedBodyTransport{next: classifiedStatusTransport{next: bearerTransport{next: base}}}}
	return &SDKClient{client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func dialPublicEgress(ctx context.Context, network, address string, lookup func(context.Context, string) ([]net.IPAddr, error), dial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("invalid MCP destination")
	}
	ips, err := publicEgressIPs(ctx, host, lookup)
	if err != nil {
		return nil, err
	}
	for _, entry := range ips {
		conn, err := dial(ctx, network, net.JoinHostPort(entry.IP.String(), port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, errors.New("MCP destination connection failed")
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
	if req.Method != http.MethodDelete {
		return t.next.RoundTrip(req)
	}
	ctx, cancel := context.WithTimeout(req.Context(), sdkCloseBudget)
	defer cancel()
	clone := req.Clone(ctx)
	return t.next.RoundTrip(clone)
}

type limitedReadCloser struct {
	io.Reader
	io.Closer
}

func (c *SDKClient) requestContext(ctx context.Context, token string, endpoint string) (context.Context, *mcp.ClientSession, error) {
	u, err := url.ParseRequestURI(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, nil, errors.New("invalid remote MCP endpoint")
	}
	if !c.testOnlyAllowPrivate {
		if err = validatePublicIPs(ctx, u.Hostname()); err != nil {
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
		return RemoteResult{}, errors.New("MCP tool returned an error")
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

// URL parsing helper used by tests and validators. It intentionally refuses
// non-HTTPS endpoints, user-info, and explicit unusual ports.
func validateSDKURL(endpoint string) error {
	u, err := url.ParseRequestURI(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" {
		return errors.New("invalid remote MCP endpoint")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("invalid remote MCP port")
		}
	}
	if strings.ContainsAny(u.Host, "\r\n\t") {
		return errors.New("invalid remote MCP endpoint")
	}
	return nil
}
