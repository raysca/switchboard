package eval

import (
	"encoding/json"
	"reflect"
	"testing"

	"govoice/server/agent"
)

const flatAgent = `{"stt":{"provider":"google","model":"chirp_2","language":"fr-FR","fallbacks":[{"provider":"deepgram","model":"nova"}]},
"llm":{"provider":"google","model":"gemini","system_prompt":"Be brief.","temperature":0.3,"max_output_tokens":200,
 "fallbacks":[{"provider":"xai","model":"grok"}]},
"tts":{"provider":"elevenlabs","voice":"v1","fallbacks":[{"provider":"google","voice":"x"}]},
"idle":{"user_timeout_ms":10000,"max_prompts":2},"greeting":"Hello there."}`

const deskAgent = `{"start":"front","steps":[
 {"id":"front","name":"Front desk","config":{"stt":{"provider":"google","model":"chirp_2"},"llm":{"provider":"google","model":"m","system_prompt":"front prompt"},"tts":{"provider":"google","voice":"a"}},"position":{"x":0,"y":80}},
 {"id":"sales","name":"Sales","config":{"llm":{"provider":"xai","model":"g","system_prompt":"sales prompt","temperature":0},"tts":{"provider":"xai","voice":"eve"}},"end_call":true,"tools":["s/t"]}],
 "handoffs":[{"id":"front-sales","from":"front","to":"sales","condition":"wants a car"}]}`

const dentalAgent = `{"start":"front","steps":[
 {"id":"front","name":"Front desk","end_call":true,"config":{"stt":{"provider":"google","model":"chirp_2","language":"en-US"},"llm":{"provider":"google","model":"m","system_prompt":"P1"},"tts":{"provider":"elevenlabs","voice":"21m"}}},
 {"id":"new","name":"New patient","end_call":true,"tools":["d/register","d/book"],"config":{"llm":{"provider":"xai","model":"grok","system_prompt":"P2"},"tts":{"provider":"xai","voice":"eve"}}},
 {"id":"returning","name":"Returning","end_call":true,"config":{"llm":{"provider":"google","model":"m","system_prompt":"P3","fallbacks":[{"provider":"xai","model":"g"}]},"tts":{"provider":"google","voice":"Aoede"}}}],
 "handoffs":[{"id":"to-new","from":"front","to":"new","condition":"first visit"},{"id":"to-returning","from":"front","to":"returning","condition":"patient"}]}`

func gen(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func rewritten(t *testing.T, cfg, mode string) map[string]any {
	t.Helper()
	out, err := Rewrite(json.RawMessage(cfg), mode)
	if err != nil {
		t.Fatal(err)
	}
	return gen(t, string(out))
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %#v, want %#v", what, got, want)
	}
}

func TestRewriteFlat(t *testing.T) {
	orig := gen(t, flatAgent)
	got := rewritten(t, flatAgent, ModeOffline)
	eq(t, "stt", got["stt"], map[string]any{"provider": "silence", "model": "text", "language": "fr-FR"})
	eq(t, "tts", got["tts"], map[string]any{"provider": "silence", "voice": "none"})
	llm := got["llm"].(map[string]any)
	eq(t, "llm.provider", llm["provider"], "offline")
	eq(t, "llm.model", llm["model"], "govoice-test")
	if _, ok := llm["fallbacks"]; ok {
		t.Error("llm fallbacks kept on a swapped model")
	}
	ol := orig["llm"].(map[string]any)
	for _, k := range []string{"system_prompt", "temperature", "max_output_tokens"} {
		eq(t, "llm."+k, llm[k], ol[k])
	}
	eq(t, "greeting", got["greeting"], orig["greeting"])
	if e := rewritten(t, flatAgent, ModeEcho)["llm"].(map[string]any); e["provider"] != "echo" || e["model"] != "fake" {
		t.Errorf("echo llm = %v", e)
	}
}

func TestRewriteKeepKeepsModelAndFallbacks(t *testing.T) {
	got := rewritten(t, flatAgent, ModeKeep)
	eq(t, "llm", got["llm"], gen(t, flatAgent)["llm"])
	eq(t, "tts", got["tts"], map[string]any{"provider": "silence", "voice": "none"})
	eq(t, "stt", got["stt"].(map[string]any)["provider"], "silence")
}

func TestRewriteMultiStep(t *testing.T) {
	for _, tc := range []struct{ name, cfg string }{{"desk", deskAgent}, {"dental", dentalAgent}} {
		for _, mode := range []string{ModeKeep, ModeOffline, ModeEcho} {
			orig, got := gen(t, tc.cfg), rewritten(t, tc.cfg, mode)
			eq(t, tc.name+" start", got["start"], orig["start"])
			eq(t, tc.name+" handoffs", got["handoffs"], orig["handoffs"])
			gs, os := got["steps"].([]any), orig["steps"].([]any)
			for i := range os {
				g, o := gs[i].(map[string]any), os[i].(map[string]any)
				for _, k := range []string{"id", "name", "end_call", "tools", "position"} {
					eq(t, tc.name+" step "+k, g[k], o[k])
				}
				gc, oc := g["config"].(map[string]any), o["config"].(map[string]any)
				eq(t, tc.name+" prompt", gc["llm"].(map[string]any)["system_prompt"], oc["llm"].(map[string]any)["system_prompt"])
				eq(t, tc.name+" tts", gc["tts"], map[string]any{"provider": "silence", "voice": "none"})
				if _, had := oc["stt"]; had {
					eq(t, tc.name+" stt provider", gc["stt"].(map[string]any)["provider"], "silence")
				} else if _, has := gc["stt"]; has {
					t.Errorf("%s: stt invented on a step that had none", tc.name)
				}
				if mode != ModeKeep {
					if p := gc["llm"].(map[string]any)["provider"]; p != mode {
						t.Errorf("%s/%s: llm provider %v", tc.name, mode, p)
					}
					if _, ok := gc["llm"].(map[string]any)["fallbacks"]; ok {
						t.Errorf("%s/%s: llm fallbacks kept", tc.name, mode)
					}
				}
			}
		}
	}
	eq(t, "sales temperature", rewritten(t, deskAgent, ModeOffline)["steps"].([]any)[1].(map[string]any)["config"].(map[string]any)["llm"].(map[string]any)["temperature"], float64(0))
}

func TestRewriteRefusesBadInput(t *testing.T) {
	if _, err := Rewrite(json.RawMessage(flatAgent), "live"); err == nil {
		t.Error("unknown mode accepted")
	}
	if _, err := Rewrite(json.RawMessage(`[1]`), ModeKeep); err == nil {
		t.Error("non-object config accepted")
	}
}

func TestNeedsLive(t *testing.T) {
	for _, tc := range []struct {
		name, cfg string
		want      bool
	}{
		{"echo", `{"llm":{"provider":"echo","model":"fake"}}`, false},
		{"offline", `{"llm":{"provider":"offline","model":"govoice-test"}}`, false},
		{"google", `{"llm":{"provider":"google","model":"m"}}`, true},
		{"xai step", deskAgent, true},
		{"offline plus google fallback", `{"llm":{"provider":"offline","model":"m","fallbacks":[{"provider":"google","model":"m"}]}}`, true},
		{"step fallback", `{"start":"a","steps":[{"id":"a","name":"A","config":{"llm":{"provider":"echo","model":"f","fallbacks":[{"provider":"xai","model":"g"}]}}}]}`, true},
		{"garbage", `nope`, true},
	} {
		if got := NeedsLive(json.RawMessage(tc.cfg)); got != tc.want {
			t.Errorf("%s: NeedsLive = %v, want %v", tc.name, got, tc.want)
		}
	}
	for _, mode := range []string{ModeOffline, ModeEcho} {
		out, _ := Rewrite(json.RawMessage(dentalAgent), mode)
		if NeedsLive(out) {
			t.Errorf("%s-rewritten dental agent needs live", mode)
		}
	}
	out, _ := Rewrite(json.RawMessage(dentalAgent), ModeKeep)
	if !NeedsLive(out) {
		t.Error("keep-mode dental agent must need live")
	}
}

// mixedCaseAgent is accepted by the server (encoding/json matches keys case-insensitively).
const mixedCaseAgent = `{"STT":{"Provider":"google","Model":"chirp_2","Language":"fr-FR","Fallbacks":[{"provider":"deepgram","model":"nova"}]},
"LLM":{"PROVIDER":"google","model":"gemini","System_Prompt":"Be brief.","Fallbacks":[{"provider":"xai","model":"grok"}]},
"Tts":{"provider":"elevenlabs","Voice":"v1"},"Idle":{"user_timeout_ms":10000,"max_prompts":2}}`

const mixedCaseDesk = `{"start":"front","steps":[
 {"id":"front","name":"Front desk","Config":{"STT":{"provider":"google","model":"m"},"LLM":{"provider":"google","model":"m"},"TTS":{"provider":"google","voice":"a"},"Idle":{"user_timeout_ms":10000,"max_prompts":2}}},
 {"id":"sales","name":"Sales","CONFIG":{"llm":{"provider":"xai","model":"g"},"tts":{"provider":"xai","voice":"eve"}}}],
 "handoffs":[{"id":"h","from":"front","to":"sales","condition":"wants a car"}]}`

// requireTextOnly decodes out the way the server does and fails unless every step runs text-only
// (and, outside keep mode, on exactly the swapped model).
func requireTextOnly(t *testing.T, name string, out json.RawMessage, mode string) {
	t.Helper()
	def, err := agent.Decode(out)
	if err != nil {
		t.Fatalf("%s/%s: server rejects the rewritten config: %v", name, mode, err)
	}
	for _, s := range def.Steps {
		c := s.Config
		if p := c.STT.Provider; p != "" && p != "silence" {
			t.Errorf("%s/%s step %s: stt %q", name, mode, s.ID, p)
		}
		if p := c.TTS.Provider; p != "silence" {
			t.Errorf("%s/%s step %s: tts %q", name, mode, s.ID, p)
		}
		if c.Idle != nil {
			t.Errorf("%s/%s step %s: idle kept", name, mode, s.ID)
		}
		if ch := c.Chain("llm"); mode != ModeKeep && (len(ch) != 1 || ch[0].Provider != mode) {
			t.Errorf("%s/%s step %s: llm chain %v", name, mode, s.ID, ch)
		}
	}
}

func TestRewriteOutputPassesServerDecoder(t *testing.T) {
	for name, cfg := range map[string]string{"flat": flatAgent, "desk": deskAgent, "dental": dentalAgent, "mixed": mixedCaseAgent, "mixed-desk": mixedCaseDesk} {
		for _, mode := range []string{ModeKeep, ModeOffline, ModeEcho} {
			out, err := Rewrite(json.RawMessage(cfg), mode)
			if err != nil {
				t.Fatalf("%s/%s: %v", name, mode, err)
			}
			requireTextOnly(t, name, out, mode)
		}
	}
}

func TestRewriteDropsIdle(t *testing.T) {
	for _, cfg := range []string{flatAgent, mixedCaseAgent, mixedCaseDesk} {
		in, err := agent.Decode(json.RawMessage(cfg))
		if err != nil || in.StartStep().Config.Idle == nil {
			t.Fatalf("fixture has no idle block: %v", err)
		}
		out, err := Rewrite(json.RawMessage(cfg), ModeKeep)
		if err != nil {
			t.Fatal(err)
		}
		if def, _ := agent.Decode(out); def.StartStep().Config.Idle != nil {
			t.Errorf("idle kept: %s", out)
		}
	}
}

func TestRewriteRefusesConfigTheServerRejects(t *testing.T) {
	if _, err := Rewrite(json.RawMessage(`{"llm":{"provider":"echo","model":"f"},"end_call":true}`), ModeKeep); err == nil {
		t.Error("unknown top-level key accepted")
	}
}

func TestNeedsLiveFailsClosed(t *testing.T) {
	for _, cfg := range []string{`{}`, `{"llm":{"provider":"google"}}`, `{"steps":[]}`, `{"LLM":{"provider":"google","model":"m"}}`,
		`{"LLM":{"provider":"echo","model":"f","Fallbacks":[{"provider":"xai","model":"g"}]}}`,
		`{"start":"a","steps":[{"id":"a","name":"A","Config":{"llm":{"provider":"google","model":"m"}}}]}`} {
		if !NeedsLive(json.RawMessage(cfg)) {
			t.Errorf("NeedsLive(%s) = false", cfg)
		}
	}
	if NeedsLive(json.RawMessage(`{"LLM":{"Provider":"Echo","model":"f"}}`)) {
		t.Error("mixed-case echo reported paid")
	}
}
