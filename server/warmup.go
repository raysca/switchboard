package server

import (
	"context"
	"errors"
	"sync"
	"time"

	"govoice/core"
	"govoice/core/netpool"
	"govoice/providers/cartesia"
	"govoice/providers/deepgram"
	"govoice/providers/elevenlabs"
	"govoice/providers/openai"
	"govoice/providers/xai"
)

// WarmupConfig (yaml `warmup`) controls connection reuse and pre-dialing.
//
//	enabled:   share one tuned transport per provider across sessions (default true)
//	predial:   at session setup, asynchronously pre-dial one TCP/TLS connection
//	           per candidate provider (default false until measured live)
//	on_start:  also pre-dial/prime TLS session tickets for the default provider
//	           endpoints at process start (default false; needs predial)
//	timeout:   bound for one warm-up (default 3s)
//	spare_ttl: how long an untaken pre-dialed connection lives (default 10s)
//
// Warm-up never bills, never touches circuit breakers, and its errors are
// only counted (voice_provider_warmups_total).
type WarmupConfig struct {
	Enabled  *bool         `yaml:"enabled"`
	Predial  bool          `yaml:"predial"`
	OnStart  bool          `yaml:"on_start"`
	Timeout  time.Duration `yaml:"timeout"`
	SpareTTL time.Duration `yaml:"spare_ttl"`
}

func (w WarmupConfig) shared() bool { return w.Enabled == nil || *w.Enabled }

// WarmTimeout is the per-warm-up bound.
func (w WarmupConfig) WarmTimeout() time.Duration {
	if w.Timeout <= 0 {
		return 3 * time.Second
	}
	return w.Timeout
}

func (w WarmupConfig) Validate() error {
	if !w.shared() && (w.Predial || w.OnStart) {
		return errors.New("warmup.predial and warmup.on_start need warmup.enabled")
	}
	if w.OnStart && !w.Predial {
		return errors.New("warmup.on_start needs warmup.predial: true")
	}
	if w.Timeout < 0 || w.Timeout > 30*time.Second {
		return errors.New("warmup.timeout must be in (0, 30s]")
	}
	if w.SpareTTL < 0 || (w.SpareTTL != 0 && (w.SpareTTL < time.Second || w.SpareTTL > time.Minute)) {
		return errors.New("warmup.spare_ttl must be between 1s and 60s")
	}
	return nil
}

// Apply installs the process-wide netpool settings.
func (w WarmupConfig) Apply() {
	netpool.Configure(netpool.Settings{Shared: w.shared(), Predial: w.shared() && w.Predial, SpareTTL: w.SpareTTL})
}

// warm fires an asynchronous, bounded warm-up for a freshly built stage
// processor. It is a no-op unless predial is on and the processor is a
// netpool.Warmer. Providers skip themselves while their breaker is open.
func (r *Registry) warm(cfg *Config, p core.Processor) {
	w, ok := p.(netpool.Warmer)
	if !ok || !cfg.Warmup.shared() || !cfg.Warmup.Predial {
		return
	}
	timeout := cfg.Warmup.WarmTimeout()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		_ = w.Warm(ctx) // counted by the provider; never affects the call
	}()
}

// WarmOnStart pre-dials the default public endpoint of every connection
// provider through the same code path calls use (placeholder key: no request
// is ever sent). Besides the short-lived spare it primes DNS and the TLS
// session-resumption cache. Google (per-session gRPC channels) and the optional xAI
// speech build are not warmed here.
func (r *Registry) WarmOnStart(ctx context.Context, timeout time.Duration) {
	const k = "warmup-placeholder"
	var ws []netpool.Warmer
	if p, err := xai.NewProcessor(xai.Config{APIKey: k}); err == nil {
		ws = append(ws, p)
	}
	if p, err := openai.NewProcessor(openai.Config{APIKey: k}); err == nil {
		ws = append(ws, p)
	}
	if p, err := deepgram.NewSTTProcessor(func() deepgram.STTConfig { c := deepgram.DefaultSTTConfig(); c.APIKey = k; return c }()); err == nil {
		ws = append(ws, p)
	}
	if p, err := cartesia.NewSTTProcessor(func() cartesia.STTConfig { c := cartesia.DefaultSTTConfig(); c.APIKey = k; return c }()); err == nil {
		ws = append(ws, p)
	}
	if p, err := cartesia.NewTTSProcessor(func() cartesia.TTSConfig { c := cartesia.DefaultTTSConfig(); c.APIKey = k; return c }()); err == nil {
		ws = append(ws, p)
	}
	if p, err := elevenlabs.NewSTTProcessor(func() elevenlabs.STTConfig { c := elevenlabs.DefaultSTTConfig(); c.APIKey = k; return c }()); err == nil {
		ws = append(ws, p)
	}
	if p, err := elevenlabs.NewTTSProcessor(func() elevenlabs.TTSConfig { c := elevenlabs.DefaultTTSConfig(); c.APIKey = k; return c }()); err == nil {
		ws = append(ws, p)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, w := range ws {
		wg.Add(1)
		go func() { defer wg.Done(); _ = w.Warm(ctx) }()
	}
	wg.Wait()
}
