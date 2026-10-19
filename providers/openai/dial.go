package openai

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"syscall"

	"govoice/core/netpool"
)

// ErrBlockedAddress is returned when BaseURL resolves to an address the
// processor refuses to dial (SSRF guard). It is a client error: no retry, no
// breaker trip.
var ErrBlockedAddress = errors.New("openai: base_url resolves to a blocked address")

// guardedPool is the process-wide pool for user-supplied base URLs: it checks
// every dialled IP after DNS resolution (so DNS rebinding cannot slip past),
// including pre-dialed spares, and is shared across sessions so HTTP/2
// connections are reused. One pool per allowPrivate mode.
// ponytail: Proxy is nil on purpose (netpool drops it when Control is set);
// through an egress proxy the dial check would only see the proxy. Add proxy
// support with host checks if a deployment needs one.
// provider is the metric label ("openai", "nvidia"): set by server code, never user input.
func guardedPool(provider string, allowPrivate bool) *netpool.Pool {
	return netpool.Get(netpool.Options{Provider: provider, Stage: "llm", Variant: fmt.Sprintf("private=%t", allowPrivate),
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !netpool.AllowedIP(ap.Addr(), allowPrivate) {
				return ErrBlockedAddress
			}
			return nil
		}})
}

// guardedClient never follows redirects (a 3xx becomes a non-retryable
// ProviderError).
func guardedClient(p *netpool.Pool) *http.Client {
	c := p.Client(0)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}
