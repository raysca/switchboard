package flow

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const stages = `"stt":{"provider":"google","model":"s"},"llm":{"provider":"google","model":"l"},"tts":{"provider":"google","voice":"t"}`

func flatWith(extra string) json.RawMessage {
	return json.RawMessage(`{` + stages + `,` + extra + `}`)
}

func TestCallBehaviorBoundaries(t *testing.T) {
	long := func(n int) string { return strings.Repeat("a", n) }
	cases := []struct {
		name, extra, want string // want "" = accepted, else a substring of the error
	}{
		{"timeout 1000", `"idle":{"user_timeout_ms":1000}`, ""},
		{"timeout 300000", `"idle":{"user_timeout_ms":300000}`, ""},
		{"timeout 0 means off", `"idle":{"user_timeout_ms":0}`, ""},
		{"timeout 999", `"idle":{"user_timeout_ms":999}`, "idle.user_timeout_ms"},
		{"timeout 300001", `"idle":{"user_timeout_ms":300001}`, "idle.user_timeout_ms"},
		{"timeout negative", `"idle":{"user_timeout_ms":-1}`, "idle.user_timeout_ms"},
		{"prompts 1", `"idle":{"user_timeout_ms":2000,"max_prompts":1}`, ""},
		{"prompts 10", `"idle":{"user_timeout_ms":2000,"max_prompts":10}`, ""},
		{"prompts 0 given", `"idle":{"user_timeout_ms":2000,"max_prompts":0}`, "idle.max_prompts"},
		{"prompts 11", `"idle":{"user_timeout_ms":2000,"max_prompts":11}`, "idle.max_prompts"},
		{"prompt 200 bytes", `"idle":{"prompt":"` + long(200) + `"}`, ""},
		{"prompt 201 bytes", `"idle":{"prompt":"` + long(201) + `"}`, "idle.prompt"},
		{"prompt 101 two-byte runes", `"idle":{"prompt":"` + strings.Repeat("é", 101) + `"}`, "idle.prompt"},
		{"prompt control char", `"idle":{"prompt":"a\u0007b"}`, "idle.prompt"},
		{"goodbye 201 bytes", `"idle":{"goodbye":"` + long(201) + `"}`, "idle.goodbye"},
		{"goodbye with newline", `"idle":{"goodbye":"a\nb"}`, ""},
		{"mode say", `"idle":{"prompt_mode":"say"}`, ""},
		{"mode llm", `"idle":{"prompt_mode":"llm"}`, ""},
		{"mode unknown", `"idle":{"prompt_mode":"shout"}`, "idle.prompt_mode"},
		{"empty idle", `"idle":{}`, "idle"},
		{"idle wrong type", `"idle":{"user_timeout_ms":"8"}`, "user_timeout_ms"},
		{"idle unknown member", `"idle":{"wait":8}`, "wait"},
		{"denoise empty object", `"audio":{"denoise":{}}`, ""},
		{"strength 0 would be no denoise at all", `"audio":{"denoise":{"strength":0}}`, "audio.denoise.strength"},
		{"strength 0.01", `"audio":{"denoise":{"strength":0.01}}`, ""},
		{"strength 1", `"audio":{"denoise":{"strength":1}}`, ""},
		{"strength 1.1", `"audio":{"denoise":{"strength":1.1}}`, "audio.denoise.strength"},
		{"strength negative", `"audio":{"denoise":{"strength":-0.1}}`, "audio.denoise.strength"},
		{"gain -30", `"audio":{"output_gain_db":-30}`, ""},
		{"gain +6", `"audio":{"output_gain_db":6}`, ""},
		{"gain explicit 0", `"audio":{"output_gain_db":0}`, ""},
		{"gain -30.1", `"audio":{"output_gain_db":-30.1}`, "audio.output_gain_db"},
		{"gain 6.1", `"audio":{"output_gain_db":6.1}`, "audio.output_gain_db"},
		{"background ok", `"audio":{"background":{"name":"office","gain_db":-60,"loop":false}}`, ""},
		{"background gain 0", `"audio":{"background":{"name":"office","gain_db":0}}`, ""},
		{"background gain 0.1", `"audio":{"background":{"name":"office","gain_db":0.1}}`, "audio.background.gain_db"},
		{"background gain -60.1", `"audio":{"background":{"name":"office","gain_db":-60.1}}`, "audio.background.gain_db"},
		{"background no name means none", `"audio":{"background":{"name":""}}`, ""},
		{"background none with a level", `"audio":{"background":{"name":"","gain_db":-20}}`, "audio.background"},
		{"background path", `"audio":{"background":{"name":"../etc/x"}}`, "audio.background.name"},
		{"background slash", `"audio":{"background":{"name":"a/b"}}`, "audio.background.name"},
		{"background upper", `"audio":{"background":{"name":"Office"}}`, "audio.background.name"},
		{"background 33 chars", `"audio":{"background":{"name":"` + long(33) + `"}}`, "audio.background.name"},
		{"empty audio", `"audio":{}`, "audio"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Decode(flatWith(c.extra))
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("rejected: %v", err)
			case c.want != "" && (err == nil || !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error naming %q, got %v", c.want, err)
			}
		})
	}
}

func TestAbsentCallBehaviorReadsAsNilAndPresentValuesSurvive(t *testing.T) {
	g, err := Decode(flatWith(`"greeting":"Hi"`))
	if err != nil || g.StartStep().Config.Idle != nil || g.StartStep().Config.Audio != nil {
		t.Fatalf("absent: %+v err %v", g.StartStep().Config, err)
	}
	g, err = Decode(flatWith(`"idle":{"user_timeout_ms":0},"audio":{"output_gain_db":0}`))
	if err != nil {
		t.Fatal(err)
	}
	c := g.StartStep().Config
	if c.Idle == nil || c.Idle.UserTimeoutMS == nil || *c.Idle.UserTimeoutMS != 0 {
		t.Fatalf("an explicit 0 timeout must stay present: %+v", c.Idle)
	}
	if c.Audio == nil || c.Audio.OutputGainDB == nil || *c.Audio.OutputGainDB != 0 {
		t.Fatalf("an explicit 0 dB must stay present: %+v", c.Audio)
	}
	out, _ := json.Marshal(c)
	if !strings.Contains(string(out), `"user_timeout_ms":0`) || !strings.Contains(string(out), `"output_gain_db":0`) {
		t.Fatalf("explicit zeros dropped on re-encode: %s", out)
	}
}

func TestCallBehaviorIsCallLevelAndWorksInTheStepsForm(t *testing.T) {
	steps := func(startExtra, otherExtra string) json.RawMessage {
		return json.RawMessage(`{"start":"a","steps":[` +
			`{"id":"a","name":"A","config":{` + stages + startExtra + `}},` +
			`{"id":"b","name":"B","config":{"llm":{"provider":"google","model":"l"},"tts":{"provider":"google","voice":"t"}` + otherExtra + `}}],` +
			`"routes":[{"id":"r","from":"a","to":"b","condition":"x"}]}`)
	}
	g, err := Decode(steps(`,"idle":{"user_timeout_ms":8000},"audio":{"output_gain_db":-3}`, ``))
	if err != nil || g.StartStep().Config.Idle == nil || g.StartStep().Config.Audio == nil {
		t.Fatalf("start step: %v", err)
	}
	for _, extra := range []string{`,"idle":{"user_timeout_ms":8000}`, `,"audio":{"output_gain_db":-3}`} {
		if _, err := Decode(steps(``, extra)); !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), "start step") {
			t.Fatalf("%s on a later step: %v", extra, err)
		}
	}
}
