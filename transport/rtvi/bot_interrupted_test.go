package rtvi_test

import (
	"encoding/json"
	"testing"

	"govoice/transport/rtvi"
)

// Pipecat's BotInterruptedMessage (processors/frameworks/rtvi/models.py:637-646) is the label and the
// type, no data; the envelope adds the id every server message carries.
func TestFormatBotInterruptedIsPipecatsMessage(t *testing.T) {
	msg, err := rtvi.NewHandler().FormatBotInterrupted()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(msg), &got); err != nil {
		t.Fatal(err)
	}
	if got["label"] != "rtvi-ai" || got["type"] != "bot-interrupted" || got["id"] == nil || len(got) != 3 {
		t.Fatalf("bot-interrupted = %s", msg)
	}
}
