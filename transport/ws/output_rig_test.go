package ws_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	"govoice/core"
	framespb "govoice/core/transport/v1/framespb"
	"govoice/transport/token"
	"govoice/transport/ws"
)

// wireEvent is one frame the client received: an RTVI message (msg set) or bot audio.
type wireEvent struct {
	msg   string // RTVI message type; "" for audio
	bytes int    // audio payload bytes
	rate  uint32 // audio sample rate on the wire
	first int16  // first audio sample: tests give each reply its own constant value
	text  string // data.text of an RTVI message that has one (bot-tts-text)
}

// outputRig is a real ws.Server whose pipeline is only the Connection (the
// transport's output side) and a client that logs every frame in arrival
// order. Tests post output frames into the session and read the wire. Audio
// is decoded from the protobuf directly, so a frame the client decoder would
// reject (a wrongly resampled one) is still counted.
type outputRig struct {
	sess *core.Session
	conn *ws.Connection
	mu   sync.Mutex
	log  []wireEvent
}

func newOutputRig(t *testing.T) *outputRig {
	t.Helper()
	now := time.Now()
	val, _ := token.NewValidator(token.DefaultConfig(testIssuer, testAudience, []byte(testSecret)), now.Add(-time.Minute))
	server := ws.NewServer(ws.DefaultServerConfig(val))
	sessCh := make(chan *core.Session, 1)
	connCh := make(chan *ws.Connection, 1)
	server.OnSessionReady = func(conn *ws.Connection) (*core.Session, error) {
		pipe, err := core.NewPipeline(conn)
		if err != nil {
			return nil, err
		}
		sess, err := core.NewSession(210, pipe, core.DefaultResourceBudget())
		if err == nil {
			sessCh <- sess
			connCh <- conn
		}
		return sess, err
	}
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"?token="+createTestToken(t, "jti-"+t.Name(), now, now.Add(time.Minute)), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	performHandshake(t, ctx, conn)
	r := &outputRig{sess: <-sessCh, conn: <-connCh}
	t.Cleanup(func() { _ = r.sess.Close() })
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var pb framespb.Frame
			if proto.Unmarshal(data, &pb) != nil {
				continue
			}
			var ev wireEvent
			switch {
			case pb.GetAudio() != nil:
				a := pb.GetAudio()
				ev.bytes, ev.rate = len(a.Audio), a.SampleRate
				if len(a.Audio) >= 2 {
					ev.first = int16(binary.LittleEndian.Uint16(a.Audio))
				}
			case pb.GetMessage() != nil:
				var head struct {
					Type string `json:"type"`
					Data struct {
						Text string `json:"text"`
					} `json:"data"`
				}
				_ = json.Unmarshal([]byte(pb.GetMessage().Data), &head)
				ev.msg, ev.text = head.Type, head.Data.Text
			default:
				continue
			}
			r.mu.Lock()
			r.log = append(r.log, ev)
			r.mu.Unlock()
		}
	}()
	return r
}

func (r *outputRig) events() []wireEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]wireEvent(nil), r.log...)
}

// waitFor polls cond over the wire log every 5 ms for up to 5 s.
func (r *outputRig) waitFor(t *testing.T, what string, cond func([]wireEvent) bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !cond(r.events()); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s; wire: %v", what, r.events())
		}
	}
}

// constPCM is d of PCM16 mono at rate with every sample v.
func constPCM(rate int, d time.Duration, v int16) []byte {
	b := make([]byte, 2*int(d*time.Duration(rate)/time.Second))
	for i := 0; i < len(b); i += 2 {
		binary.LittleEndian.PutUint16(b[i:], uint16(v))
	}
	return b
}

// countAudio counts the audio frames whose first sample is first.
func countAudio(log []wireEvent, first int16) int {
	n := 0
	for _, ev := range log {
		if ev.msg == "" && ev.first == first {
			n++
		}
	}
	return n
}
