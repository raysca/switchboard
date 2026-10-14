package flow

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"govoice/server/storage"
)

func withGreeting(g string) json.RawMessage {
	body, _ := json.Marshal(g)
	return json.RawMessage(`{"stt":{"provider":"google","model":"s"},"llm":{"provider":"google","model":"l"},"tts":{"provider":"google","voice":"t"},"greeting":` + string(body) + `}`)
}

func stepsWithGreetings(start, other string) json.RawMessage {
	a, _ := json.Marshal(start)
	b, _ := json.Marshal(other)
	return json.RawMessage(`{"start":"a","steps":[` +
		`{"id":"a","name":"A","config":{"stt":{"provider":"google","model":"s"},"llm":{"provider":"google","model":"l"},"tts":{"provider":"google","voice":"t"},"greeting":` + string(a) + `}},` +
		`{"id":"b","name":"B","config":{"llm":{"provider":"google","model":"l"},"tts":{"provider":"google","voice":"t"},"greeting":` + string(b) + `}}],` +
		`"routes":[{"id":"r","from":"a","to":"b","condition":"x"}]}`)
}

func TestAGreetingIsAcceptedOnAnyStepAndTheFlatForm(t *testing.T) {
	g, err := Decode(withGreeting("  Hello there.  "))
	if err != nil || g.StartStep().Config.GreetingText() != "Hello there." {
		t.Fatalf("flat: %+v err %v", g.StartStep().Config.GreetingText(), err)
	}
	g, err = Decode(stepsWithGreetings("Welcome.", "Sales here."))
	if err != nil {
		t.Fatal(err)
	}
	if got := g.Steps[0].Config.GreetingText() + "|" + g.Steps[1].Config.GreetingText(); got != "Welcome.|Sales here." {
		t.Fatalf("steps: %q", got)
	}
}

func TestAnEmptyOrBlankGreetingMeansNone(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\t "} {
		g, err := Decode(withGreeting(in))
		if err != nil || g.StartStep().Config.GreetingText() != "" {
			t.Fatalf("%q: %q err %v", in, g.StartStep().Config.GreetingText(), err)
		}
	}
}

func TestGreetingLimitsAreInBytesAndControlCharactersAreRefused(t *testing.T) {
	if _, err := Decode(withGreeting(strings.Repeat("a", 500))); err != nil {
		t.Fatalf("500 bytes: %v", err)
	}
	for name, in := range map[string]string{
		"501 bytes":          strings.Repeat("a", 501),
		"multibyte over 500": strings.Repeat("é", 251), // 502 bytes, 251 runes
		"nul":                "Hi\u0000there",
		"bell":               "Hi\u0007there",
		"del":                "Hi\u007fthere",
	} {
		if _, err := Decode(withGreeting(in)); !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), "greeting") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := Decode(withGreeting("Line one.\r\n\tLine two.")); err != nil {
		t.Fatalf("newline and tab: %v", err)
	}
	// A greeting that is only too long once trimmed is fine; one over the limit before trimming is not counted untrimmed.
	if _, err := Decode(withGreeting("  " + strings.Repeat("a", 500) + "  ")); err != nil {
		t.Fatalf("padding is trimmed before counting: %v", err)
	}
}

func TestAGreetingMustBeText(t *testing.T) {
	raw := json.RawMessage(`{"stt":{"provider":"google","model":"s"},"llm":{"provider":"google","model":"l"},"tts":{"provider":"google","voice":"t"},"greeting":5}`)
	if _, err := Decode(raw); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("a number: %v", err)
	}
	raw = json.RawMessage(`{"stt":{"provider":"google","model":"s"},"llm":{"provider":"google","model":"l"},"tts":{"provider":"google","voice":"t"},"greeting":{"a":1}}`)
	if _, err := Decode(raw); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("an object: %v", err)
	}
}

func TestTheGreetingIsPartOfTheConfigHash(t *testing.T) {
	a, _, _ := storage.ConfigHash(withGreeting(""))
	b, _, _ := storage.ConfigHash(withGreeting("Hi."))
	if a == b {
		t.Fatal("a greeting must change the hash")
	}
}

func TestAGreetingErrorNamesTheStep(t *testing.T) {
	_, err := Decode(stepsWithGreetings("ok", strings.Repeat("a", 501)))
	if err == nil || !strings.Contains(err.Error(), `"b"`) || !strings.Contains(err.Error(), "greeting") {
		t.Fatalf("got %v", err)
	}
}
