// Package nvidia holds the NVIDIA Riva gRPC adapters (streaming ASR; TTS is
// added beside it). NIM chat completions need no code here: the server reuses
// providers/openai with NVIDIA's base URL. dial.go, errors.go are shared by
// every Riva stage: one gRPC channel per processor, per-RPC auth metadata,
// SSRF-guarded dialing, warm-up.
package nvidia

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"

	"govoice/core/netpool"
)

// DefaultEndpoint is NVIDIA's hosted (NVCF) gRPC front end. It needs a
// function ID and an API key; self-hosted Riva is "host:50051" without TLS.
const DefaultEndpoint = "grpc.nvcf.nvidia.com:443"

// ErrBlockedAddress: the endpoint resolved to an address the dialer refuses
// (SSRF guard). A client error: no retry, no breaker trip.
var ErrBlockedAddress = errors.New("nvidia: endpoint resolves to a blocked address")

// ConnConfig describes one Riva channel.
type ConnConfig struct {
	Endpoint   string // host:port; default DefaultEndpoint
	Insecure   bool   // plaintext gRPC (self-hosted Riva). Zero value = TLS.
	APIKey     string // NVCF bearer; sent only over TLS
	FunctionID string // NVCF function-id header
	// AllowPrivate permits loopback/private endpoints (self-hosted Riva).
	// Off, the endpoint must resolve to a public address. Link-local and
	// cloud-metadata addresses stay blocked either way. Plaintext (Insecure)
	// needs it and may only reach loopback/private addresses.
	AllowPrivate bool
	// Conn is an already-built channel (tests); it is not closed by Close.
	Conn *grpc.ClientConn
}

// Conn is a lazily-connecting gRPC channel plus the metadata every RPC needs.
type Conn struct {
	cc         *grpc.ClientConn
	own        bool
	closeOnce  sync.Once
	closeErr   error
	apiKey     string
	functionID string
	provider   string
}

// Dial builds the channel. It does not touch the network (gRPC connects on
// first use or on Warm).
func Dial(cfg ConnConfig) (*Conn, error) {
	c := &Conn{cc: cfg.Conn, functionID: cfg.FunctionID, provider: "nvidia"}
	if !cfg.Insecure {
		c.apiKey = cfg.APIKey // never put a bearer on a plaintext channel
	}
	if c.cc != nil {
		return c, nil
	}
	ep := strings.TrimSpace(cfg.Endpoint)
	if ep == "" {
		ep = DefaultEndpoint
	}
	if strings.Contains(ep, "://") || strings.ContainsAny(ep, "/ \t") {
		return nil, errors.New("nvidia: endpoint must be host:port")
	}
	creds := credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	if cfg.Insecure {
		if !cfg.AllowPrivate {
			return nil, errors.New("nvidia: plaintext endpoint requires providers.nvidia.allow_private")
		}
		creds = insecure.NewCredentials()
	}
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !dialAllowed(ap.Addr(), cfg.AllowPrivate, cfg.Insecure) {
				return ErrBlockedAddress
			}
			return nil
		}}
	cc, err := grpc.NewClient(ep,
		grpc.WithTransportCredentials(creds),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) }),
		// Ping only while an RPC is in flight (no PermitWithoutStream): a
		// half-dead connection mid-utterance fails in ~80 s instead of hanging.
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 60 * time.Second, Timeout: 20 * time.Second}))
	if err != nil {
		return nil, MapError("dial", err)
	}
	c.cc, c.own = cc, true
	return c, nil
}

// Channel is the underlying client connection (for generated clients).
func (c *Conn) Channel() *grpc.ClientConn { return c.cc }

// Close releases a channel this Conn created; idempotent.
func (c *Conn) Close() error {
	if c == nil || !c.own || c.cc == nil {
		return nil
	}
	c.closeOnce.Do(func() { c.closeErr = c.cc.Close() })
	return c.closeErr
}

// Context adds the NVCF auth metadata to ctx. Values are per-RPC, never logged.
func (c *Conn) Context(ctx context.Context) context.Context {
	var kv []string
	if c.functionID != "" {
		kv = append(kv, "function-id", c.functionID)
	}
	if c.apiKey != "" {
		kv = append(kv, "authorization", "Bearer "+c.apiKey)
	}
	if len(kv) == 0 {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, kv...)
}

// Warm starts the lazy channel (DNS + TCP + TLS + HTTP/2 settings) and waits
// for READY or ctx. No RPC is sent: no auth, no billing.
func (c *Conn) Warm(ctx context.Context, stage string) error {
	if !netpool.PredialEnabled() {
		netpool.NoteSkipped(c.provider, stage)
		return nil
	}
	start := time.Now()
	c.cc.Connect()
	var err error
	for s := c.cc.GetState(); s != connectivity.Ready; s = c.cc.GetState() {
		if s == connectivity.Shutdown {
			err = errors.New("nvidia: channel closed")
			break
		}
		if s == connectivity.TransientFailure {
			err = errors.New("nvidia: channel connect failed")
			break
		}
		if !c.cc.WaitForStateChange(ctx, s) {
			err = ctx.Err()
			break
		}
	}
	netpool.Observe(c.provider, stage, time.Since(start), err)
	return err
}

var (
	metadataV6 = netip.MustParseAddr("fd00:ec2::254") // AWS IMDS over IPv6
	cgnat      = netip.MustParsePrefix("100.64.0.0/10")
)

// allowedIP mirrors providers/openai's SSRF rule (separate module, so copied):
// link-local (incl. 169.254.169.254), multicast and unspecified are always
// refused; loopback and private ranges only with allowPrivate.
func allowedIP(ip netip.Addr, allowPrivate bool) bool {
	ip = ip.Unmap()
	switch {
	case !ip.IsValid(), ip.IsUnspecified(), ip.IsMulticast(), ip.IsInterfaceLocalMulticast(),
		ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(), ip == metadataV6:
		return false
	case ip.IsLoopback(), ip.IsPrivate(), cgnat.Contains(ip):
		return allowPrivate
	}
	return true
}

// dialAllowed adds to allowedIP: plaintext never leaves the private network
// (allowedIP(ip, false) means ip is public).
func dialAllowed(ip netip.Addr, allowPrivate, plaintext bool) bool {
	return allowedIP(ip, allowPrivate) && !(plaintext && allowedIP(ip, false))
}

func skipWarm(stage string) { netpool.NoteSkipped("nvidia", stage) }
