package google

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
	"govoice/core/netpool"
	"govoice/core/resilience"

	speech "cloud.google.com/go/speech/apiv2"
	speechpb "cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/api/option"
)

// STTConfig configures the Google Speech-to-Text V2 processor.
type STTConfig struct {
	Endpoint                string        // Pinned: "speech.googleapis.com"
	Model                   string        // Pinned: "chirp_2"
	LanguageCode            string        // Default: "en-US"
	StreamRotationThreshold time.Duration // Pinned: 240s (shorter for tests)
	StreamOverlap           time.Duration // Pinned: 500ms
	MaxRequestBytes         int           // Pinned: 25600
	ProjectID               string        // Google Cloud project ID
	Location                string        // Speech v2 location (chirp_2 is not in "global")
	CredentialsFile         string        // Path to service account key JSON
	CredentialsJSON         []byte        // In-memory service account JSON; preferred over CredentialsFile/APIKey
	APIKey                  string        // Fallback API key if supported
	StreamOpener            STTStreamOpener
	Provider                string            // label on frames, usage and traces; empty means "google"
	RetryPolicy             resilience.Policy // bounds opening a stream: MaxAttempts tries of AttemptTimeout each
	CircuitBreaker          *resilience.Breaker
	Now                     func() time.Time // Receipt clock; policy time is carried in candidate frames.
}

// DefaultSTTConfig returns the configuration matching v1-lock.json.
func DefaultSTTConfig() STTConfig {
	return STTConfig{
		Endpoint:                "speech.googleapis.com",
		Model:                   "chirp_2",
		LanguageCode:            "en-US",
		StreamRotationThreshold: 240 * time.Second,
		StreamOverlap:           500 * time.Millisecond,
		MaxRequestBytes:         25600,
		RetryPolicy:             DefaultSTTOpenPolicy(),
		CircuitBreaker:          resilience.NewBreaker(resilience.DefaultConfig()),
	}
}

// DefaultSTTOpenPolicy is two tries of 5 s to open a recognition stream: the
// open must not hang on the TCP dial's 10 s alone while a live caller waits.
func DefaultSTTOpenPolicy() resilience.Policy {
	return resilience.Policy{MaxAttempts: 2, AttemptTimeout: 5 * time.Second, InitialBackoff: 100 * time.Millisecond}
}

// STTResult is a provider-neutral recognition result.
type STTResult struct {
	Transcript      string
	IsFinal         bool
	Stability       float32
	ResultEndOffset time.Duration
}

// STTStream abstracts the bidirectional recognition stream.
type STTStream interface {
	Send(audio []byte) error
	Recv() ([]*STTResult, error)
	CloseSend() error
	Close() error
}

// STTStreamOpener abstracts stream creation.
type STTStreamOpener interface {
	OpenStream(ctx context.Context, cfg STTConfig) (STTStream, error)
}

// RealGoogleSTTOpener creates real Cloud Speech V2 streams.
type RealGoogleSTTOpener struct {
	client *speech.Client
}

func speechEndpoint(cfg STTConfig) string {
	loc := cfg.Location
	if loc == "" {
		loc = "us-central1"
	}
	if cfg.Endpoint != "" && cfg.Endpoint != "speech.googleapis.com" {
		return cfg.Endpoint
	}
	if loc == "global" {
		return "speech.googleapis.com"
	}
	return loc + "-speech.googleapis.com"
}

// sttProjectID returns the recognizer project: explicit, else from in-memory JSON, else "_".
func sttProjectID(cfg STTConfig) string {
	if cfg.ProjectID != "" {
		return cfg.ProjectID
	}
	if len(cfg.CredentialsJSON) > 0 {
		if sa, err := parseServiceAccountJSON(cfg.CredentialsJSON); err == nil && sa.ProjectID != "" {
			return sa.ProjectID
		}
	}
	return "_"
}

func NewRealGoogleSTTOpener(ctx context.Context, cfg STTConfig) (*RealGoogleSTTOpener, error) {
	opts := append([]option.ClientOption{
		option.WithEndpoint(speechEndpoint(cfg) + ":443"),
	}, clientAuthOptions(cfg.CredentialsJSON, cfg.CredentialsFile, cfg.APIKey)...)
	opts = append(opts, grpcKeepalive())

	c, err := speech.NewClient(ctx, opts...)
	if err != nil {
		return nil, MapError("stt", err)
	}
	return &RealGoogleSTTOpener{client: c}, nil
}

func (o *RealGoogleSTTOpener) Close() error {
	if o.client != nil {
		return o.client.Close()
	}
	return nil
}

func (o *RealGoogleSTTOpener) OpenStream(ctx context.Context, cfg STTConfig) (STTStream, error) {
	pol := cfg.RetryPolicy
	if pol.MaxAttempts <= 0 {
		pol = DefaultSTTOpenPolicy()
	}
	st, err := resilience.OpenBounded(ctx, pol, classify, func(ctx context.Context) (STTStream, error) {
		return o.openOnce(ctx, cfg)
	})
	if err != nil {
		return nil, err
	}
	return st, nil
}

func (o *RealGoogleSTTOpener) openOnce(ctx context.Context, cfg STTConfig) (STTStream, error) {
	netpool.CountRequest("google", "stt")
	stream, err := o.client.StreamingRecognize(ctx)
	if err != nil {
		return nil, MapError("stt", err)
	}

	project := sttProjectID(cfg)
	location := cfg.Location
	if location == "" {
		location = "us-central1"
	}
	recognizer := fmt.Sprintf("projects/%s/locations/%s/recognizers/_", project, location)

	// Send initial streaming configuration
	initReq := &speechpb.StreamingRecognizeRequest{
		Recognizer: recognizer,
		StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: &speechpb.StreamingRecognitionConfig{
				Config: &speechpb.RecognitionConfig{
					Model:         cfg.Model,
					LanguageCodes: []string{cfg.LanguageCode},
					DecodingConfig: &speechpb.RecognitionConfig_ExplicitDecodingConfig{
						ExplicitDecodingConfig: &speechpb.ExplicitDecodingConfig{
							Encoding:          speechpb.ExplicitDecodingConfig_LINEAR16,
							SampleRateHertz:   16000,
							AudioChannelCount: 1,
						},
					},
				},
				StreamingFeatures: &speechpb.StreamingRecognitionFeatures{
					InterimResults: true,
				},
			},
		},
	}

	if err := stream.Send(initReq); err != nil {
		_ = stream.CloseSend()
		return nil, MapError("stt", err)
	}

	return &grpcSTTStream{stream: stream}, nil
}

type grpcSTTStream struct {
	stream speechpb.Speech_StreamingRecognizeClient
}

func (s *grpcSTTStream) Send(audio []byte) error {
	req := &speechpb.StreamingRecognizeRequest{
		StreamingRequest: &speechpb.StreamingRecognizeRequest_Audio{
			Audio: audio,
		},
	}
	return s.stream.Send(req)
}

func (s *grpcSTTStream) Recv() ([]*STTResult, error) {
	resp, err := s.stream.Recv()
	if err != nil {
		return nil, err
	}
	results := make([]*STTResult, 0, len(resp.Results))
	for _, res := range resp.Results {
		if len(res.Alternatives) > 0 {
			result := &STTResult{
				Transcript: res.Alternatives[0].Transcript,
				IsFinal:    res.IsFinal,
				Stability:  res.Stability,
			}
			if res.ResultEndOffset != nil {
				result.ResultEndOffset = res.ResultEndOffset.AsDuration()
			}
			results = append(results, result)
		}
	}
	return results, nil
}

func (s *grpcSTTStream) CloseSend() error {
	return s.stream.CloseSend()
}

func (s *grpcSTTStream) Close() error {
	return s.stream.CloseSend()
}

// activeStream holds the state of an open STT stream.
type activeStream struct {
	stream            STTStream
	cancel            context.CancelFunc
	ctx               context.Context
	createdAt         time.Time
	id                int
	turnID            string
	gen               uint64
	offsetBase        time.Duration
	lastFinalOffset   time.Duration
	lastInterimOffset time.Duration
	traceSpan         core.Span
	tracePolicy       core.TracePolicy
	traceSession      core.SessionID
	traceEnd          sync.Once
	traceFinal        sync.Once
}

func (st *activeStream) endTrace(turn core.TurnID, gen core.Generation, err error) {
	st.traceEnd.Do(func() {
		if st.traceSpan == nil {
			return
		}
		name := "stt.complete"
		if err != nil {
			name = "stt.failure"
		}
		st.traceSpan.AddEvent(core.NewTraceEvent(name, time.Now(), st.traceSession, turn, gen, st.tracePolicy, nil))
		st.traceSpan.End(err)
	})
}

func (p *STTProcessor) traceSessionID() core.SessionID {
	if p.session == nil {
		return ""
	}
	return core.SessionID(strconv.FormatUint(p.session.ID(), 10))
}

func (p *STTProcessor) startStreamTrace(ctx context.Context, turn string, gen uint64) (core.Span, core.TracePolicy) {
	policy := core.TraceContextFromContext(ctx).Policy
	if p.session == nil {
		return nil, policy
	}
	return core.StartContextSpan(ctx, "stt.stream", core.SessionID(strconv.FormatUint(p.session.ID(), 10)), core.TurnID(turn), core.Generation(gen), map[string]any{"provider": p.cfg.Provider}), policy
}

type googleSegment struct {
	start    time.Duration
	end      time.Duration
	streamID int
	text     string
}

type googleAssembly struct {
	segments []googleSegment
	interims []googleSegment
	prefix   string
}

type googleAssemblyKey struct {
	turn string
	gen  uint64
}

const (
	maxGoogleAssemblies = 128
	maxGoogleSegments   = 256
)

// STTProcessor implements core.Processor for Google Speech-to-Text V2.
type STTProcessor struct {
	cfg     STTConfig
	session core.SessionController
	index   int

	mu             sync.Mutex
	active         *activeStream
	streamID       int
	closed         bool
	turnID         string
	streamGen      uint64
	assemblies     map[googleAssemblyKey]*googleAssembly
	assemblyGen    uint64
	turnAudioBytes int64

	// Audio overlap buffer: keeps last 500ms (16,000 bytes for 16kHz 16-bit mono)
	audioBuffer  []byte
	overlapBytes int

	// Metrics
	RotationsCount      atomic.Uint64
	RotationFailures    atomic.Uint64
	DeduplicatedResults atomic.Uint64
	Errors              atomic.Uint64
}

// STTCounts implements core.STTCounters.
func (p *STTProcessor) STTCounts() (rot, rotFail, dedup, errs uint64) {
	return p.RotationsCount.Load(), p.RotationFailures.Load(), p.DeduplicatedResults.Load(), p.Errors.Load()
}

// NewSTTProcessor creates a new STTProcessor with the given config.
func NewSTTProcessor(cfg STTConfig) (*STTProcessor, error) {
	if cfg.Provider == "" {
		cfg.Provider = "google"
	}
	if cfg.Model == "" {
		cfg.Model = "chirp_2"
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "speech.googleapis.com"
	}
	if cfg.StreamRotationThreshold <= 0 {
		cfg.StreamRotationThreshold = 240 * time.Second
	}
	if cfg.StreamOverlap <= 0 {
		cfg.StreamOverlap = 500 * time.Millisecond
	}
	if cfg.MaxRequestBytes <= 0 {
		cfg.MaxRequestBytes = 25600
	}
	if cfg.CircuitBreaker == nil {
		cfg.CircuitBreaker = resilience.NewBreaker(resilience.DefaultConfig())
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	// 16000 Hz * 2 bytes/sample * overlap_seconds
	overlapBytes := int(16000 * 2 * cfg.StreamOverlap.Seconds())
	if overlapBytes <= 0 {
		overlapBytes = 16000
	}

	return &STTProcessor{
		cfg:          cfg,
		overlapBytes: overlapBytes,
		audioBuffer:  make([]byte, 0, overlapBytes*2),
		assemblies:   make(map[googleAssemblyKey]*googleAssembly),
	}, nil
}

// Attach registers the session controller and processor index.
func (p *STTProcessor) Attach(s core.SessionController, index int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.session = s
	p.index = index
	return nil
}

// Close gracefully terminates the processor and active streams.
func (p *STTProcessor) Close() error {
	p.mu.Lock()
	p.closed = true
	active := p.active
	p.active = nil
	p.mu.Unlock()
	if active != nil {
		active.cancel()
		_ = active.stream.Close()
	}
	if closer, ok := p.cfg.StreamOpener.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
	return nil
}

// ProcessFrame handles incoming pipeline frames.
func (p *STTProcessor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch frame := f.(type) {
	case *core.StartFrame:
		return e.PushDirection(f, dir)

	case *core.UserStartedSpeakingFrame:
		p.mu.Lock()
		old := p.setPendingOwnershipLocked(fmt.Sprintf("turn-%d-%d", frame.Gen, frame.Seq), frame.Gen)
		p.mu.Unlock()
		if old != nil {
			p.halfClose(old)
		}
		return e.Push(f)

	case *core.TurnEventFrame:
		if frame.Kind == core.TurnEventStarted {
			p.mu.Lock()
			old := p.setPendingOwnershipLocked(string(frame.Turn), uint64(frame.Gen))
			p.mu.Unlock()
			if old != nil {
				p.halfClose(old)
			}
		}
		return e.PushDirection(f, dir)

	case *core.InputAudioFrame:
		return p.handleAudio(ctx, frame, e)

	case *core.UserStoppedSpeakingFrame:
		// Detach the finished utterance's stream so the next one opens unbound:
		// VAD reports speech start only after the onset audio was sent, and that
		// audio must reach the next utterance's stream, not this one.
		p.mu.Lock()
		old := p.active
		p.active = nil
		p.turnID = ""
		p.mu.Unlock()
		if old != nil {
			p.halfClose(old)
		}
		return e.Push(f)

	case *core.InterruptionFrame:
		p.mu.Lock()
		var old *activeStream
		generation := uint64(frame.Gen)
		if p.session != nil && p.session.CurrentGeneration() > generation {
			generation = p.session.CurrentGeneration()
		}
		if generation > p.streamGen {
			p.streamGen = generation
			p.turnAudioBytes = 0
			p.audioBuffer = p.audioBuffer[:0]
			if p.active != nil && p.active.gen < generation {
				old = p.active
				p.active = nil
			}
		}
		p.mu.Unlock()
		if old != nil {
			old.cancel()
			_ = old.stream.Close()
		}
		return e.PushDirection(f, dir)

	case *core.CancelFrame:
		return e.PushDirection(f, dir)

	default:
		return e.PushDirection(f, dir)
	}
}

func (p *STTProcessor) halfClose(st *activeStream) {
	if st == nil {
		return
	}
	if err := st.stream.CloseSend(); err != nil {
		st.cancel()
		if p.session != nil {
			_ = p.session.Reenter(&core.ErrorFrame{
				Err:  MapError("stt", err),
				Turn: core.TurnID(st.turnID),
				Gen:  core.Generation(st.gen),
			}, core.Downstream, p.index+1, st.gen)
		}
	}
}

func (p *STTProcessor) handleAudio(ctx context.Context, f *core.InputAudioFrame, e core.Emitter) error {
	// Request size constraint: check bytes limit
	if len(f.Samples) > p.cfg.MaxRequestBytes {
		return fmt.Errorf("%w: chunk size %d exceeds limit %d", ErrRequestTooLarge, len(f.Samples), p.cfg.MaxRequestBytes)
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}

	// Rotate from audio already sent. The current chunk is appended only after
	// replay, so it cannot be both replayed and sent again.
	shouldRotate := false
	if p.active != nil && p.cfg.Now().Sub(p.active.createdAt) >= p.cfg.StreamRotationThreshold {
		shouldRotate = true
	}
	p.mu.Unlock()

	if shouldRotate {
		if err := p.rotateStream(ctx); err != nil {
			p.RotationFailures.Add(1)
			// Log / record error but don't crash session immediately if primary can still transmit
		}
	}

	// Ensure stream is open
	if err := p.ensureStream(ctx); err != nil {
		p.Errors.Add(1)
		return err
	}

	p.mu.Lock()
	p.audioBuffer = append(p.audioBuffer, f.Samples...)
	p.turnAudioBytes += int64(len(f.Samples))
	if len(p.audioBuffer) > p.overlapBytes {
		overflow := len(p.audioBuffer) - p.overlapBytes
		p.audioBuffer = p.audioBuffer[overflow:]
	}
	st := p.active
	p.mu.Unlock()

	if st == nil {
		return ErrProviderUnavailable
	}

	// Send audio chunk
	if err := st.stream.Send(f.Samples); err != nil {
		if errors.Is(err, io.EOF) {
			// gRPC: the stream already ended (the server closed it, or call
			// teardown cancelled it); its status is the receiver's to report.
			// Not a session failure: drop this frame, the next opens a new stream.
			p.mu.Lock()
			if p.active == st {
				p.active = nil
			}
			p.mu.Unlock()
			if p.session == nil { // no receiver owns the stream's context
				st.cancel()
			}
			return e.Push(f)
		}
		p.mu.Lock()
		turn, turnGen := st.turnID, st.gen
		if p.active == st {
			p.active = nil
		}
		p.mu.Unlock()
		st.endTrace(core.TurnID(turn), core.Generation(turnGen), errors.New("provider_failure"))
		st.cancel()
		_ = st.stream.Close()
		provErr := MapError("stt", err)
		if p.cfg.CircuitBreaker != nil {
			p.cfg.CircuitBreaker.Report(provErr, classify)
		}
		return provErr
	}

	return e.Push(f)
}

func (p *STTProcessor) ensureStream(ctx context.Context) error {
	p.mu.Lock()
	if p.active != nil || p.closed {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()

	return p.openPrimaryStream(ctx)
}

func (p *STTProcessor) ioParent(ctx context.Context) context.Context {
	if p.session != nil {
		return p.session.Context()
	}
	if ctx != nil {
		return ctx
	}
	return context.Background()
}

func (p *STTProcessor) openPrimaryStream(ctx context.Context) error {
	if p.cfg.StreamOpener == nil {
		return fmt.Errorf("no STTStreamOpener configured")
	}

	p.mu.Lock()
	turn, gen := p.turnID, p.streamGen
	p.mu.Unlock()
	span, policy := p.startStreamTrace(ctx, turn, gen)
	streamCtx, cancel := context.WithCancel(p.ioParent(ctx))
	st, err := func() (STTStream, error) {
		defer func() {
			if v := recover(); v != nil {
				cancel()
				if span != nil {
					span.AddEvent(core.NewTraceEvent("stt.failure", time.Now(), p.traceSessionID(), core.TurnID(turn), core.Generation(gen), policy, nil))
					span.End(errors.New("provider_failure"))
				}
				panic(v)
			}
		}()
		return p.cfg.StreamOpener.OpenStream(streamCtx, p.cfg)
	}()
	if err != nil {
		if span != nil {
			span.AddEvent(core.NewTraceEvent("stt.failure", time.Now(), p.traceSessionID(), core.TurnID(turn), core.Generation(gen), policy, nil))
			span.End(errors.New("provider_failure"))
		}
		cancel()
		provErr := MapError("stt", err)
		if p.cfg.CircuitBreaker != nil {
			p.cfg.CircuitBreaker.Report(provErr, classify)
		}
		return provErr
	}

	if p.cfg.CircuitBreaker != nil {
		p.cfg.CircuitBreaker.RecordSuccess()
	}

	p.mu.Lock()
	p.streamID++
	newActive := &activeStream{
		stream:       st,
		cancel:       cancel,
		ctx:          streamCtx,
		createdAt:    p.cfg.Now(),
		id:           p.streamID,
		turnID:       p.turnID,
		gen:          p.streamGen,
		traceSpan:    span,
		tracePolicy:  policy,
		traceSession: p.traceSessionID(),
	}
	p.active = newActive
	p.mu.Unlock()
	if span != nil {
		span.AddEvent(core.NewTraceEvent("stt.start", time.Now(), newActive.traceSession, core.TurnID(newActive.turnID), core.Generation(newActive.gen), policy, nil))
	}

	p.spawnReceiver(newActive)
	return nil
}

// rotateStream performs pre-limit stream rotation with bounded overlap and transcript deduplication.
func (p *STTProcessor) rotateStream(ctx context.Context) error {
	p.mu.Lock()
	if p.closed || p.active == nil {
		p.mu.Unlock()
		return nil
	}
	oldActive := p.active
	overlapData := make([]byte, len(p.audioBuffer))
	copy(overlapData, p.audioBuffer)
	p.mu.Unlock()

	// Open replacement stream
	span, policy := p.startStreamTrace(ctx, oldActive.turnID, oldActive.gen)
	streamCtx, cancel := context.WithCancel(p.ioParent(ctx))
	newStream, err := func() (STTStream, error) {
		defer func() {
			if v := recover(); v != nil {
				cancel()
				if span != nil {
					span.AddEvent(core.NewTraceEvent("stt.failure", time.Now(), p.traceSessionID(), core.TurnID(oldActive.turnID), core.Generation(oldActive.gen), policy, nil))
					span.End(errors.New("provider_failure"))
				}
				panic(v)
			}
		}()
		return p.cfg.StreamOpener.OpenStream(streamCtx, p.cfg)
	}()
	if err != nil {
		if span != nil {
			span.AddEvent(core.NewTraceEvent("stt.failure", time.Now(), p.traceSessionID(), core.TurnID(oldActive.turnID), core.Generation(oldActive.gen), policy, nil))
			span.End(errors.New("provider_failure"))
		}
		cancel()
		return MapError("stt", err)
	}

	// Bridge the bounded overlap audio (500ms) into the replacement stream
	if len(overlapData) > 0 {
		// Send in chunks <= MaxRequestBytes
		chunkSize := p.cfg.MaxRequestBytes
		for i := 0; i < len(overlapData); i += chunkSize {
			end := i + chunkSize
			if end > len(overlapData) {
				end = len(overlapData)
			}
			if err := newStream.Send(overlapData[i:end]); err != nil {
				if span != nil {
					span.AddEvent(core.NewTraceEvent("stt.failure", time.Now(), p.traceSessionID(), core.TurnID(oldActive.turnID), core.Generation(oldActive.gen), policy, nil))
					span.End(errors.New("provider_failure"))
				}
				cancel()
				_ = newStream.Close()
				return MapError("stt", err)
			}
		}
	}

	p.mu.Lock()
	p.streamID++
	newActive := &activeStream{
		stream:       newStream,
		cancel:       cancel,
		ctx:          streamCtx,
		createdAt:    p.cfg.Now(),
		id:           p.streamID,
		turnID:       oldActive.turnID,
		gen:          oldActive.gen,
		traceSpan:    span,
		tracePolicy:  policy,
		traceSession: p.traceSessionID(),
		offsetBase:   time.Duration(p.turnAudioBytes*int64(time.Second)/32000) - time.Duration(len(overlapData))*time.Second/32000,
	}
	if newActive.offsetBase < 0 {
		newActive.offsetBase = 0
	}
	p.active = newActive
	p.RotationsCount.Add(1)
	p.mu.Unlock()
	if span != nil {
		span.AddEvent(core.NewTraceEvent("stt.start", time.Now(), newActive.traceSession, core.TurnID(newActive.turnID), core.Generation(newActive.gen), policy, nil))
	}

	// Spawn receiver for the new stream
	p.spawnReceiver(newActive)

	// Gracefully close the old stream as supervised session work so teardown
	// waits for it (or cancels the grace period) and a panic fails only this call.
	closeOld := func(ctx context.Context) {
		_ = oldActive.stream.CloseSend()
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
		}
		oldActive.cancel()
		_ = oldActive.stream.Close()
	}
	if p.session == nil || p.session.Go(closeOld) != nil {
		// No live session or task budget exhausted: close now, without the
		// grace period, rather than sleeping on the caller's goroutine.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		closeOld(ctx)
	}

	return nil
}

func (p *STTProcessor) spawnReceiver(st *activeStream) {
	if p.session == nil {
		return
	}

	if err := p.session.Go(func(ctx context.Context) {
		var traceFailure error
		defer st.cancel() // a detached stream only got CloseSend; free its context too
		defer func() {
			if v := recover(); v != nil {
				traceFailure = errors.New("provider_failure")
				p.mu.Lock()
				turn, gen := st.turnID, st.gen
				p.mu.Unlock()
				st.endTrace(core.TurnID(turn), core.Generation(gen), traceFailure)
				panic(v)
			}
			p.mu.Lock()
			turn, gen := st.turnID, st.gen
			p.mu.Unlock()
			st.endTrace(core.TurnID(turn), core.Generation(gen), traceFailure)
		}()
		for {
			select {
			case <-st.ctx.Done():
				traceFailure = errors.New("canceled")
				return
			case <-ctx.Done():
				traceFailure = errors.New("canceled")
				return
			default:
			}

			results, err := st.stream.Recv()
			if err != nil {
				p.mu.Lock()
				turnGen := st.gen
				if p.active == st {
					p.active = nil
				}
				p.mu.Unlock()

				if errors.Is(err, context.Canceled) || st.ctx.Err() != nil {
					traceFailure = errors.New("canceled")
					return
				}
				if isSTTIdleOrClosed(err) {
					return
				}
				p.Errors.Add(1)
				traceFailure = errors.New("provider_failure")
				if p.session != nil {
					_ = p.session.Reenter(&core.ErrorFrame{
						Err: MapError("stt", err),
					}, core.Downstream, p.index+1, turnGen)
				}
				return
			}
			p.mu.Lock()
			turn, turnGen := st.turnID, st.gen
			currentGen := turnGen
			if p.session != nil {
				currentGen = p.session.CurrentGeneration()
			}
			detached := p.active != st
			if turn == "" || turnGen == 0 || (turnGen < currentGen && !detached) {
				p.mu.Unlock()
				continue
			}
			p.resetInterimsLocked(st)
			var text string
			var latest *STTResult
			var hasFinal bool
			var assemblyErr error
			for _, res := range results {
				if res == nil || res.Transcript == "" {
					continue
				}
				text, assemblyErr = p.assembleResultLocked(st, res)
				latest = res
				hasFinal = hasFinal || res.IsFinal
				if assemblyErr != nil {
					break
				}
			}
			p.mu.Unlock()
			if assemblyErr != nil {
				traceFailure = errors.New("provider_failure")
				if err := p.session.Reenter(&core.TranscriptInvalidationFrame{
					Turn: core.TurnID(turn), Gen: core.Generation(turnGen), Err: assemblyErr,
				}, core.Downstream, p.index+1, turnGen); err != nil {
					// Admission of this terminal frame fails the Session closed.
					// An error frame cannot safely stand in for it out of order.
					continue
				}
				_ = p.session.Reenter(&core.ErrorFrame{
					Err:  fmt.Errorf("%w: google transcript assembly failed: %v", core.ErrTranscriptInvalidated, assemblyErr),
					Turn: core.TurnID(turn), Gen: core.Generation(turnGen),
				}, core.Downstream, p.index+1, turnGen)
				continue
			}
			if latest == nil {
				continue
			}
			if hasFinal && st.traceSpan != nil {
				st.traceFinal.Do(func() {
					st.traceSpan.AddEvent(core.NewTraceEvent("stt.first_final", time.Now(), st.traceSession, core.TurnID(turn), core.Generation(turnGen), st.tracePolicy, nil))
				})
			}
			frame := &core.TranscriptCandidateFrame{
				Text: text, Turn: core.TurnID(turn), Gen: core.Generation(turnGen),
				Timestamp: p.cfg.Now(), Final: latest.IsFinal,
				Stability: float64(latest.Stability), HasStability: !latest.IsFinal && latest.Stability > 0,
				Provider: p.cfg.Provider,
			}
			admitGen := turnGen
			if detached {
				admitGen = currentGen
			}
			_ = p.session.Reenter(frame, core.Downstream, p.index+1, admitGen)
		}
	}); err != nil {
		p.mu.Lock()
		turn, gen := st.turnID, st.gen
		p.mu.Unlock()
		st.endTrace(core.TurnID(turn), core.Generation(gen), errors.New("canceled"))
		st.cancel()
		_ = st.stream.Close()
	}
}

func (p *STTProcessor) resetInterimsLocked(st *activeStream) {
	// A new response can begin after a final portion in this same stream.
	st.lastInterimOffset = st.lastFinalOffset
	if assembly := p.assemblies[googleAssemblyKey{turn: st.turnID, gen: st.gen}]; assembly != nil {
		assembly.interims = nil
	}
}

func (p *STTProcessor) bindOrDetachActiveLocked(turn string, gen uint64) *activeStream {
	if p.active == nil {
		return nil
	}
	if p.active.turnID == "" {
		p.active.turnID, p.active.gen = turn, gen
		return nil
	}
	if p.active.turnID == turn && p.active.gen < gen {
		// A turn manager barged in mid-utterance: the open stream moves to the
		// new generation with its assembled text instead of being cut.
		old := googleAssemblyKey{turn: turn, gen: p.active.gen}
		if a := p.assemblies[old]; a != nil {
			delete(p.assemblies, old)
			p.assemblies[googleAssemblyKey{turn: turn, gen: gen}] = a
		}
		p.active.gen = gen
		return nil
	}
	if p.active.turnID != turn || p.active.gen != gen {
		old := p.active
		p.active = nil
		return old
	}
	return nil
}

func (p *STTProcessor) setPendingOwnershipLocked(turn string, gen uint64) *activeStream {
	if p.turnID != turn {
		p.turnAudioBytes = 0
		p.audioBuffer = p.audioBuffer[:0]
	}
	p.turnID, p.streamGen = turn, gen
	return p.bindOrDetachActiveLocked(turn, gen)
}

func (p *STTProcessor) assembleLocked(st *activeStream, result *STTResult) string {
	text, _ := p.assembleResultLocked(st, result)
	return text
}

func (p *STTProcessor) assembleResultLocked(st *activeStream, result *STTResult) (string, error) {
	if st.gen > p.assemblyGen {
		for key := range p.assemblies {
			if key.gen < st.gen {
				delete(p.assemblies, key)
			}
		}
		p.assemblyGen = st.gen
	}
	key := googleAssemblyKey{turn: st.turnID, gen: st.gen}
	a := p.assemblies[key]
	if a == nil {
		if len(p.assemblies) >= maxGoogleAssemblies {
			return "", ErrTranscriptAssemblyCapacity
		}
		a = &googleAssembly{}
		p.assemblies[key] = a
	}
	text := strings.Clone(strings.TrimSpace(result.Transcript))
	const maxTranscriptBytes = 32 * 1024
	if len(text) > maxTranscriptBytes {
		return "", ErrTranscriptAssemblyCapacity
	}
	if !result.IsFinal {
		start := st.offsetBase + st.lastInterimOffset
		end := st.offsetBase + result.ResultEndOffset
		if result.ResultEndOffset > st.lastInterimOffset {
			st.lastInterimOffset = result.ResultEndOffset
		}
		replaced := false
		for i := range a.interims {
			if a.interims[i].end == end {
				a.interims[i] = googleSegment{start: start, end: end, streamID: st.id, text: text}
				replaced = true
				break
			}
		}
		if !replaced {
			if len(a.interims) >= maxGoogleSegments {
				return "", ErrTranscriptAssemblyCapacity
			}
			a.interims = append(a.interims, googleSegment{start: start, end: end, streamID: st.id, text: text})
			sort.Slice(a.interims, func(i, j int) bool { return a.interims[i].end < a.interims[j].end })
		}
		// Compose from media intervals, including settled portions. A rotated
		// stream can replay the end of a final portion, so flattening that final
		// to a text prefix would duplicate words in the overlapping interim.
		segments := make([]googleSegment, 0, len(a.segments)+len(a.interims))
		segments = append(segments, a.segments...)
		segments = append(segments, a.interims...)
		sort.SliceStable(segments, func(i, j int) bool { return segments[i].end < segments[j].end })
		prefix := ""
		if len(a.segments) == 0 {
			prefix = a.prefix // Results without media offsets have no interval provenance.
		}
		composed := composeGoogleSegments(prefix, segments)
		if len(composed) > maxTranscriptBytes {
			return "", ErrTranscriptAssemblyCapacity
		}
		return composed, nil
	}
	a.interims = nil
	relativeStart := st.lastFinalOffset
	if result.ResultEndOffset > st.lastFinalOffset {
		st.lastFinalOffset = result.ResultEndOffset
	}
	// A following interim in this response starts after the settled portion.
	st.lastInterimOffset = st.lastFinalOffset
	start := st.offsetBase + relativeStart
	end := st.offsetBase + result.ResultEndOffset
	if end <= 0 {
		composed := appendTranscript(a.prefix, text)
		if len(composed) <= maxTranscriptBytes {
			a.prefix = strings.Clone(composed)
		}
		return composed, nil
	}
	replaced := false
	for i := range a.segments {
		if a.segments[i].end == end {
			// Rotation overlap often reports only the suffix ending at the same
			// media offset. Keep the more complete compatible text; otherwise a
			// same-offset result is a provider correction and replaces it.
			existing := a.segments[i].text
			if strings.Contains(existing, text) {
				text = existing
			} else if !strings.Contains(text, existing) {
				a.segments[i].text = text
			} else {
				a.segments[i].text = text
			}
			replaced = true
			break
		}
	}
	if !replaced {
		if len(a.segments) >= maxGoogleSegments {
			return "", ErrTranscriptAssemblyCapacity
		}
		a.segments = append(a.segments, googleSegment{start: start, end: end, streamID: st.id, text: text})
		sort.Slice(a.segments, func(i, j int) bool { return a.segments[i].end < a.segments[j].end })
	}
	a.prefix = composeGoogleSegments("", a.segments)
	if len(a.prefix) > maxTranscriptBytes {
		// The common finalizer rejects this candidate. Do not retain an
		// unbounded composed prefix in the adapter.
		a.prefix = ""
		a.segments = nil
		return "", ErrTranscriptAssemblyCapacity
	}
	return a.prefix, nil
}

func composeGoogleSegments(prefix string, segments []googleSegment) string {
	composed := prefix
	var priorEnd time.Duration
	for i, segment := range segments {
		if (i > 0 || prefix != "") && segment.start < priorEnd {
			composed = joinTranscript(composed, segment.text)
		} else {
			composed = appendTranscript(composed, segment.text)
		}
		if segment.end > priorEnd {
			priorEnd = segment.end
		}
	}
	return composed
}

func appendTranscript(prefix, next string) string {
	prefix, next = strings.TrimSpace(prefix), strings.TrimSpace(next)
	if prefix == "" {
		return next
	}
	if next == "" {
		return prefix
	}
	return prefix + " " + next
}

func joinTranscript(prefix, next string) string {
	prefix, next = strings.TrimSpace(prefix), strings.TrimSpace(next)
	if prefix == "" {
		return next
	}
	if next == "" || strings.HasSuffix(prefix, next) {
		return prefix
	}
	left, right := strings.Fields(prefix), strings.Fields(next)
	max := len(left)
	if len(right) < max {
		max = len(right)
	}
	overlap := 0
	for n := 1; n <= max; n++ {
		if strings.Join(left[len(left)-n:], " ") == strings.Join(right[:n], " ") {
			overlap = n
		}
	}
	if overlap == len(right) {
		return prefix
	}
	return strings.TrimSpace(prefix + " " + strings.Join(right[overlap:], " "))
}

func isSTTIdleOrClosed(err error) bool {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "Stream timed out after receiving no more client requests") ||
		strings.Contains(msg, "context canceled")
}
