package ws_test

import (
	"strings"
	"testing"
	"time"

	"govoice/transport/ws"
)

// The largest valid frame (a full 16 KB RTVI message) fits the advertised max with headroom, and the max is
// not much larger: it sizes the read buffer one socket can make the server hold.
func TestLargestValidFrameFitsTheAdvertisedMax(t *testing.T) {
	frame, err := ws.EncodeMessageFrame(strings.Repeat("x", ws.MaxMessageDataBytes))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.DecodeBinaryFrame(frame, 1, time.Now()); err != nil {
		t.Fatalf("the largest valid message is rejected: %v", err)
	}
	if len(frame) > ws.MaxEncodedFrameSize || ws.MaxEncodedFrameSize > 24*1024 {
		t.Fatalf("largest valid frame %d B, advertised max %d B", len(frame), ws.MaxEncodedFrameSize)
	}
}
