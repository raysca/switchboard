package rtvi_test

import (
	"errors"
	"testing"
	"time"

	"govoice/transport/rtvi"
)

func TestDTMFMessages(t *testing.T) {
	h := rtvi.NewHandler()
	var got []string
	h.OnDTMF = func(d string) error { got = append(got, d); return nil }
	msg := func(data string) string {
		return `{"id":"1","label":"rtvi-ai","type":"dtmf","data":` + data + `}`
	}
	if _, err := h.HandleIncomingJSON(msg(`{"buttons":["1"]}`)); !errors.Is(err, rtvi.ErrRejectedMessage) {
		t.Fatalf("dtmf before client-ready: %v", err)
	}
	if _, err := h.HandleIncomingJSON(`{"id":"r","label":"rtvi-ai","type":"client-ready","data":{"version":"` + rtvi.ProtocolVersion + `"}}`); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{}`, `{"buttons":[]}`, `{"buttons":["1","x"]}`, `{"buttons":["12"]}`, `{"buttons":[1]}`, `"str"`,
		`{"buttons":["1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1"]}`} {
		if _, err := h.HandleIncomingJSON(msg(bad)); !errors.Is(err, rtvi.ErrRejectedMessage) {
			t.Errorf("%s accepted: %v", bad, err)
		}
	}
	if _, err := h.HandleIncomingJSON(msg(`{"buttons":["1","2","#"]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.HandleIncomingJSON(msg(`{"button":"*"}`)); err != nil { // RTVI 2.0.x legacy form
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "12#" || got[1] != "*" {
		t.Fatalf("delivered %q", got)
	}
	// 4 keys spent above; the burst allows MaxDigitsPerSend, then the cap holds.
	if _, err := h.HandleIncomingJSON(msg(`{"buttons":["1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1","1"]}`)); !errors.Is(err, rtvi.ErrRejectedMessage) {
		t.Fatalf("flood beyond the key rate accepted: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("rate-limited keys delivered: %q", got)
	}
	time.Sleep(150 * time.Millisecond) // one key's worth of tokens
	if _, err := h.HandleIncomingJSON(msg(`{"button":"7"}`)); err != nil {
		t.Fatalf("key after refill rejected: %v", err)
	}
	h.OnDTMF = func(string) error { return errors.New("no active session") }
	if _, err := h.HandleIncomingJSON(msg(`{"buttons":["1"]}`)); !errors.Is(err, rtvi.ErrRejectedMessage) {
		t.Fatalf("callback error must be a rejection: %v", err)
	}
	h.OnDTMF = nil
	if _, err := h.HandleIncomingJSON(msg(`{"buttons":["1"]}`)); !errors.Is(err, rtvi.ErrRejectedMessage) {
		t.Fatalf("unsupported dtmf must be a rejection: %v", err)
	}
}
