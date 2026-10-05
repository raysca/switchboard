package google

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"

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
	APIKey                  string        // Fallback API key if supported
	QuotaManager            *QuotaManager // Headroom manager (multiplier 2.0)
	StreamOpener            STTStreamOpener
	RetryPolicy             RetryPolicy
	CircuitBreaker          *CircuitBreaker
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
		RetryPolicy:             DefaultRetryPolicy(),
		CircuitBreaker:          NewCircuitBreaker(DefaultCircuitConfig()),
	}
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

func NewRealGoogleSTTOpener(ctx context.Context, cfg STTConfig) (*RealGoogleSTTOpener, error) {
	opts := []option.ClientOption{
		option.WithEndpoint(speechEndpoint(cfg) + ":443"),
	}
	if cfg.CredentialsFile != "" {
		opts = append(opts, option.WithCredentialsFile(cfg.CredentialsFile))
	} else if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}

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
	stream, err := o.client.StreamingRecognize(ctx)
	if err != nil {
		return nil, MapError("stt", err)
	}

	project := cfg.ProjectID
	if project == "" {
		project = "_"
	}
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
	stream     STTStream
	cancel     context.CancelFunc
	ctx        context.Context
	createdAt  time.Time
	id         int
	turnID     string
	gen        uint64
	offsetBase time.Duration
}

type googleSegment struct {
	end  time.Duration
	text string
}

type googleAssembly struct {
	segments []googleSegment
	prefix   string
}

// STTProcessor implements core.Processor for Google Speech-to-Text V2.
type STTProcessor struct {
	cfg     STTConfig
	session core.SessionController
	index   int

	mu         sync.Mutex
	active     *activeStream
	streamID   int
	closed     bool
	turnID     string
	streamGen  uint64
	assemblies map[string]*googleAssembly
	audioBytes int64

	// Audio overlap buffer: keeps last 500ms (16,000 bytes for 16kHz 16-bit mono)
	audioBuffer  []byte
	overlapBytes int

	// Metrics
	RotationsCount      atomic.Uint64
	RotationFailures    atomic.Uint64
	DeduplicatedResults atomic.Uint64
	TotalRequestsSent   atomic.Uint64
	Errors              atomic.Uint64
}

// NewSTTProcessor creates a new STTProcessor with the given config.
func NewSTTProcessor(cfg STTConfig) (*STTProcessor, error) {
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
		cfg.CircuitBreaker = NewCircuitBreaker(DefaultCircuitConfig())
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
		assemblies:   make(map[string]*googleAssembly),
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
	defer p.mu.Unlock()

	p.closed = true
	if p.active != nil {
		p.active.cancel()
		_ = p.active.stream.Close()
		p.active = nil
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
		p.turnID = fmt.Sprintf("turn-%d-%d", frame.Gen, frame.Seq)
		p.streamGen = frame.Gen
		p.bindOrDetachActiveLocked(p.turnID, p.streamGen)
		p.mu.Unlock()
		return e.Push(f)

	case *core.TurnEventFrame:
		if frame.Kind == core.TurnEventStarted {
			p.mu.Lock()
			p.turnID = string(frame.Turn)
			p.streamGen = uint64(frame.Gen)
			p.bindOrDetachActiveLocked(p.turnID, p.streamGen)
			p.mu.Unlock()
		}
		return e.PushDirection(f, dir)

	case *core.InputAudioFrame:
		return p.handleAudio(ctx, frame, e)

	case *core.UserStoppedSpeakingFrame:
		// Speech ended for current turn
		return e.Push(f)

	case *core.InterruptionFrame:
		p.mu.Lock()
		// Never relabel an existing stream. A new speech start binds the next
		// stream to its immutable turn/generation owner.
		p.mu.Unlock()
		return e.PushDirection(f, dir)

	case *core.CancelFrame:
		return e.PushDirection(f, dir)

	default:
		return e.PushDirection(f, dir)
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

	// Update rolling overlap buffer
	p.audioBuffer = append(p.audioBuffer, f.Samples...)
	p.audioBytes += int64(len(f.Samples))
	if len(p.audioBuffer) > p.overlapBytes {
		overflow := len(p.audioBuffer) - p.overlapBytes
		p.audioBuffer = p.audioBuffer[overflow:]
	}

	// Check if active stream requires rotation
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
	st := p.active
	p.mu.Unlock()

	if st == nil {
		return ErrProviderUnavailable
	}

	// Send audio chunk
	if err := st.stream.Send(f.Samples); err != nil {
		provErr := MapError("stt", err)
		if p.cfg.CircuitBreaker != nil {
			p.cfg.CircuitBreaker.ReportFailure()
		}
		return provErr
	}

	p.TotalRequestsSent.Add(1)
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

	streamCtx, cancel := context.WithCancel(p.ioParent(ctx))
	st, err := p.cfg.StreamOpener.OpenStream(streamCtx, p.cfg)
	if err != nil {
		cancel()
		provErr := MapError("stt", err)
		if p.cfg.CircuitBreaker != nil {
			p.cfg.CircuitBreaker.ReportFailure()
		}
		return provErr
	}

	if p.cfg.CircuitBreaker != nil {
		p.cfg.CircuitBreaker.ReportSuccess()
	}

	p.mu.Lock()
	p.streamID++
	newActive := &activeStream{
		stream:    st,
		cancel:    cancel,
		ctx:       streamCtx,
		createdAt: p.cfg.Now(),
		id:        p.streamID,
		turnID:    p.turnID,
		gen:       p.streamGen,
	}
	p.active = newActive
	p.mu.Unlock()

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

	// Reserve temporary second-stream quota
	if p.cfg.QuotaManager != nil {
		if err := p.cfg.QuotaManager.ReserveRotationStream(); err != nil {
			return err
		}
		defer p.cfg.QuotaManager.ReleaseRotationStream()
	}

	// Open replacement stream
	streamCtx, cancel := context.WithCancel(p.ioParent(ctx))
	newStream, err := p.cfg.StreamOpener.OpenStream(streamCtx, p.cfg)
	if err != nil {
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
				cancel()
				_ = newStream.Close()
				return MapError("stt", err)
			}
		}
	}

	p.mu.Lock()
	p.streamID++
	newActive := &activeStream{
		stream:     newStream,
		cancel:     cancel,
		ctx:        streamCtx,
		createdAt:  p.cfg.Now(),
		id:         p.streamID,
		turnID:     oldActive.turnID,
		gen:        oldActive.gen,
		offsetBase: time.Duration(p.audioBytes*int64(time.Second)/32000) - p.cfg.StreamOverlap,
	}
	if newActive.offsetBase < 0 {
		newActive.offsetBase = 0
	}
	p.active = newActive
	p.RotationsCount.Add(1)
	p.mu.Unlock()

	// Spawn receiver for the new stream
	p.spawnReceiver(newActive)

	// Gracefully close the old stream
	go func() {
		_ = oldActive.stream.CloseSend()
		time.Sleep(200 * time.Millisecond)
		oldActive.cancel()
		_ = oldActive.stream.Close()
	}()

	return nil
}

func (p *STTProcessor) spawnReceiver(st *activeStream) {
	if p.session == nil {
		return
	}

	_ = p.session.Go(func(ctx context.Context) {
		for {
			select {
			case <-st.ctx.Done():
				return
			case <-ctx.Done():
				return
			default:
			}

			results, err := st.stream.Recv()
			if err != nil {
				p.mu.Lock()
				if p.active == st {
					p.active = nil
				}
				p.mu.Unlock()

				if errors.Is(err, context.Canceled) || st.ctx.Err() != nil {
					return
				}
				if isSTTIdleOrClosed(err) {
					return
				}
				p.Errors.Add(1)
				if p.session != nil {
					_ = p.session.Reenter(&core.ErrorFrame{
						Err: MapError("stt", err),
					}, core.Downstream, p.index+1, st.gen)
				}
				return
			}
			for _, res := range results {
				if res == nil || res.Transcript == "" || st.turnID == "" || st.gen == 0 {
					continue
				}
				currentGen := st.gen
				if p.session != nil {
					currentGen = p.session.CurrentGeneration()
				}
				if st.gen < currentGen {
					continue
				}
				p.mu.Lock()
				text := p.assembleLocked(st, res)
				p.mu.Unlock()
				frame := &core.TranscriptCandidateFrame{
					Text: text, Turn: core.TurnID(st.turnID), Gen: core.Generation(st.gen),
					Timestamp: p.cfg.Now(), Final: res.IsFinal,
					Stability: float64(res.Stability), HasStability: !res.IsFinal && res.Stability > 0,
					Provider: "google",
				}
				_ = p.session.Reenter(frame, core.Downstream, p.index+1, st.gen)
			}
		}
	})
}

func (p *STTProcessor) bindOrDetachActiveLocked(turn string, gen uint64) {
	if p.active == nil {
		return
	}
	if p.active.turnID == "" {
		p.active.turnID, p.active.gen = turn, gen
		return
	}
	if p.active.turnID != turn || p.active.gen != gen {
		old := p.active
		p.active = nil
		_ = old.stream.CloseSend()
	}
}

func (p *STTProcessor) assembleLocked(st *activeStream, result *STTResult) string {
	key := fmt.Sprintf("%d/%s", st.gen, st.turnID)
	a := p.assemblies[key]
	if a == nil {
		if len(p.assemblies) >= 128 {
			return strings.Clone(strings.TrimSpace(result.Transcript))
		}
		a = &googleAssembly{}
		p.assemblies[key] = a
	}
	text := strings.Clone(strings.TrimSpace(result.Transcript))
	const maxTranscriptBytes = 32 * 1024
	if len(text) > maxTranscriptBytes {
		return text // common policy rejects it fail-closed; do not retain it here
	}
	if !result.IsFinal {
		return joinTranscript(a.prefix, text)
	}
	end := st.offsetBase + result.ResultEndOffset
	if end <= 0 {
		composed := joinTranscript(a.prefix, text)
		if len(composed) <= maxTranscriptBytes {
			a.prefix = strings.Clone(composed)
		}
		return composed
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
		a.segments = append(a.segments, googleSegment{end: end, text: text})
		sort.Slice(a.segments, func(i, j int) bool { return a.segments[i].end < a.segments[j].end })
		if len(a.segments) > 128 {
			a.segments = a.segments[len(a.segments)-128:]
		}
	}
	a.prefix = ""
	for _, segment := range a.segments {
		a.prefix = joinTranscript(a.prefix, segment.text)
	}
	if len(a.prefix) > maxTranscriptBytes {
		// The common finalizer rejects this candidate. Do not retain an
		// unbounded composed prefix in the adapter.
		a.prefix = ""
		a.segments = nil
	}
	return a.prefix
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
