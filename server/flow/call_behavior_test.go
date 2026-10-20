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

func TestTurnSettingsValidation(t *testing.T) {
	const greet = `"greeting":"Hello",`
	cases := []struct {
		name, extra, want string // want "" = accepted, else a substring of the error
	}{
		{"min_words 0 means off", `"turn":{"min_words":0}`, ""},
		{"min_words 1", `"turn":{"min_words":1}`, ""},
		{"min_words 10", `"turn":{"min_words":10}`, ""},
		{"min_words -1", `"turn":{"min_words":-1}`, "turn.min_words"},
		{"min_words 11", `"turn":{"min_words":11}`, "turn.min_words"},
		{"min_words fraction", `"turn":{"min_words":1.5}`, "min_words"},
		{"mute none", `"turn":{"mute":[]}`, ""},
		{"mute all four", greet + `"turn":{"mute":["always","first_speech","until_first_bot_complete","function_call"]}`, ""},
		{"mute one", `"turn":{"mute":["function_call"]}`, ""},
		{"mute unknown names its index", `"turn":{"mute":["always","nope"]}`, "turn.mute[1]"},
		{"mute critical_output is gone", `"turn":{"mute":["critical_output"]}`, "turn.mute[0]"},
		{"mute duplicate", `"turn":{"mute":["always","always"]}`, "turn.mute[1]"},
		{"until_first_bot_complete needs a greeting", `"turn":{"mute":["until_first_bot_complete"]}`, "turn.mute[0]"},
		{"until_first_bot_complete blank greeting", `"greeting":"   ","turn":{"mute":["until_first_bot_complete"]}`, "greeting"},
		{"max_mute 1000", `"turn":{"max_mute_ms":1000}`, ""},
		{"max_mute 60000", `"turn":{"max_mute_ms":60000}`, ""},
		{"max_mute 999", `"turn":{"max_mute_ms":999}`, "turn.max_mute_ms"},
		{"max_mute 60001", `"turn":{"max_mute_ms":60001}`, "turn.max_mute_ms"},
		{"max_mute 0", `"turn":{"max_mute_ms":0}`, "turn.max_mute_ms"},
		{"max_mute negative", `"turn":{"max_mute_ms":-5}`, "turn.max_mute_ms"},
		{"speech_timeout 200", `"turn":{"speech_timeout_ms":200}`, ""},
		{"speech_timeout 3000", `"turn":{"speech_timeout_ms":3000}`, ""},
		{"speech_timeout 199", `"turn":{"speech_timeout_ms":199}`, "turn.speech_timeout_ms"},
		{"speech_timeout 3001", `"turn":{"speech_timeout_ms":3001}`, "turn.speech_timeout_ms"},
		{"speech_timeout 0", `"turn":{"speech_timeout_ms":0}`, "turn.speech_timeout_ms"},
		{"speech_timeout negative", `"turn":{"speech_timeout_ms":-1}`, "turn.speech_timeout_ms"},
		{"end_of_turn default", `"turn":{"end_of_turn":"default"}`, ""},
		{"end_of_turn smart", `"turn":{"end_of_turn":"smart"}`, ""},
		{"end_of_turn eager", `"turn":{"end_of_turn":"eager"}`, ""},
		{"end_of_turn unknown", `"turn":{"end_of_turn":"fast"}`, "turn.end_of_turn"},
		{"smart_hold 500 with smart", `"turn":{"end_of_turn":"smart","smart_hold_ms":500}`, ""},
		{"smart_hold 5000 with smart", `"turn":{"end_of_turn":"smart","smart_hold_ms":5000}`, ""},
		{"smart_hold 499", `"turn":{"end_of_turn":"smart","smart_hold_ms":499}`, "turn.smart_hold_ms"},
		{"smart_hold 5001", `"turn":{"end_of_turn":"smart","smart_hold_ms":5001}`, "turn.smart_hold_ms"},
		{"smart_hold 0", `"turn":{"end_of_turn":"smart","smart_hold_ms":0}`, "turn.smart_hold_ms"},
		{"smart_hold without smart", `"turn":{"smart_hold_ms":1000}`, "turn.smart_hold_ms"},
		{"smart_hold with eager", `"turn":{"end_of_turn":"eager","smart_hold_ms":1000}`, "turn.smart_hold_ms"},
		{"empty turn", `"turn":{}`, "turn must set at least one member"},
		{"unknown member", `"turn":{"patience":3}`, "patience"},
		{"wrong type", `"turn":{"min_words":"3"}`, "min_words"},
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

func TestTurnSettingsAreCallLevelAndKeepExplicitZeroAndEmpty(t *testing.T) {
	steps := func(startExtra, otherExtra string) json.RawMessage {
		return json.RawMessage(`{"start":"a","steps":[` +
			`{"id":"a","name":"A","config":{` + stages + startExtra + `}},` +
			`{"id":"b","name":"B","config":{"llm":{"provider":"google","model":"l"},"tts":{"provider":"google","voice":"t"}` + otherExtra + `}}],` +
			`"routes":[{"id":"r","from":"a","to":"b","condition":"x"}]}`)
	}
	g, err := Decode(steps(`,"turn":{"min_words":0,"mute":[]}`, ``))
	if err != nil {
		t.Fatal(err)
	}
	tr := g.StartStep().Config.Turn
	if tr == nil || tr.MinWords == nil || *tr.MinWords != 0 || tr.Mute == nil || len(*tr.Mute) != 0 {
		t.Fatalf("explicit 0 and [] must stay present: %+v", tr)
	}
	if out, _ := json.Marshal(g.StartStep().Config); !strings.Contains(string(out), `"min_words":0`) || !strings.Contains(string(out), `"mute":[]`) {
		t.Fatalf("explicit zero or empty list dropped on re-encode: %s", out)
	}
	if _, err := Decode(steps(``, `,"turn":{"min_words":2}`)); !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), "start step") {
		t.Fatalf("turn on a later step: %v", err)
	}
}

// K7: a step other than the start may set the background sound and nothing else of audio.
func TestANonStartStepMayOnlySetABackground(t *testing.T) {
	steps := withSalesAudio
	g, err := Decode(steps(`{"background":{"name":"office","gain_db":-20}}`))
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := g.Step("sales"); s.Config.Audio == nil || s.Config.Audio.Background == nil || s.Config.Audio.Background.Name != "office" {
		t.Fatalf("the step's background was lost: %+v", s.Config.Audio)
	}
	if _, err := Decode(steps(`{"background":{"name":""}}`)); err != nil {
		t.Fatalf("an explicit none on a step: %v", err)
	}
	for _, audio := range []string{`{"denoise":{}}`, `{"output_gain_db":-3}`, `{"background":{"name":"office"},"output_gain_db":-3}`} {
		_, err := Decode(steps(audio))
		if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), `step "sales": audio may only set background on a step other than the start step`) {
			t.Fatalf("%s: %v", audio, err)
		}
	}
	if _, err := Decode(steps(`{}`)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("an empty audio object: %v", err)
	}
}
