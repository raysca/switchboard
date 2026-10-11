package catalog

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const gcreds = "GOOGLE_APPLICATION_CREDENTIALS"

func newTestResolver(t *testing.T, env map[string]string, files map[string]string) *Resolver {
	t.Helper()
	cat, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	read := func(path string, max int64) ([]byte, error) {
		s, ok := files[path]
		if !ok {
			return nil, errors.New("no such file")
		}
		if path == "/dir" {
			return nil, errors.New("is a directory")
		}
		if int64(len(s)) > max {
			return nil, errors.New("file too large")
		}
		return []byte(s), nil
	}
	return NewResolverWithFS(cat, func(k string) string { return env[k] }, read)
}

func TestXAIToken(t *testing.T) {
	for _, tc := range []struct {
		name, val string
		ok        bool
		want      string
	}{
		{"plain", "key", true, "key"},
		{"trimmed", "  key  ", true, "key"},
		{"unset", "", false, ""},
		{"space inside", "a b", false, ""},
		{"newline inside", "a\nb", false, ""},
		{"dollar", "$OTHER", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestResolver(t, map[string]string{"XAI_API_KEY": tc.val}, nil)
			st := r.Status("xai", "llm")
			if !st.Declared || st.Detected != tc.ok {
				t.Fatalf("status = %+v", st)
			}
			b, err := r.Secret("xai", "llm")
			if tc.ok {
				if err != nil || string(b) != tc.want {
					t.Fatalf("secret = %q, %v", b, err)
				}
				return
			}
			var me *MissingError
			if !errors.As(err, &me) || !reflect.DeepEqual(me.Env, []string{"XAI_API_KEY"}) {
				t.Fatalf("err = %v", err)
			}
			if tc.val != "" && strings.Contains(err.Error(), tc.val) {
				t.Fatalf("error leaks value: %v", err)
			}
		})
	}
}

const saJSON = "{\n  \"type\": \"service_account\",\n  \"private_key\": \"pk\"\n}"

func TestGoogleKinds(t *testing.T) {
	files := map[string]string{"/sa.json": saJSON}
	for _, tc := range []struct {
		name         string
		env          map[string]string
		llm          string // expected secret, "" = not detected
		stt, tts     bool
		sttMissing   []string
		wantSTTBytes string
	}{
		{"api key only", map[string]string{"GOOGLE_API_KEY": "k"}, "k", false, false, []string{gcreds}, ""},
		{"file only", map[string]string{gcreds: "/sa.json"}, `{"type":"service_account","private_key":"pk"}`, true, true, nil, `{"type":"service_account","private_key":"pk"}`},
		{"both", map[string]string{"GOOGLE_API_KEY": "k", gcreds: "/sa.json"}, "k", true, true, nil, `{"type":"service_account","private_key":"pk"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestResolver(t, tc.env, files)
			b, err := r.Secret("google", "llm")
			if err != nil || string(b) != tc.llm {
				t.Fatalf("llm = %q, %v", b, err)
			}
			for svc, want := range map[string]bool{"stt": tc.stt, "tts": tc.tts} {
				st := r.Status("google", svc)
				if st.Detected != want {
					t.Fatalf("%s status = %+v", svc, st)
				}
				if !want && !reflect.DeepEqual(st.MissingEnv, tc.sttMissing) {
					t.Fatalf("%s missing = %v", svc, st.MissingEnv)
				}
			}
			if tc.stt {
				b, err := r.Secret("google", "stt")
				if err != nil || string(b) != tc.wantSTTBytes {
					t.Fatalf("stt = %q, %v", b, err)
				}
			}
		})
	}
}

func TestFileJSONProblems(t *testing.T) {
	big := `{"x":"` + strings.Repeat("a", MaxFileBytes) + `"}`
	files := map[string]string{"/text": "not json", "/dir": "", "/big": big}
	for _, tc := range []struct{ name, path, class string }{
		{"unreadable", "/missing", "readable JSON file"},
		{"not json", "/text", "JSON"},
		{"directory", "/dir", "readable JSON file"},
		{"too large", "/big", "readable JSON file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestResolver(t, map[string]string{gcreds: tc.path}, files)
			st := r.Status("google", "stt")
			if st.Detected || !reflect.DeepEqual(st.MissingEnv, []string{gcreds}) {
				t.Fatalf("status = %+v", st)
			}
			if !strings.Contains(st.Detail, gcreds) || !strings.Contains(st.Detail, tc.class) {
				t.Fatalf("detail = %q", st.Detail)
			}
			if _, err := r.Secret("google", "stt"); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestOpenAIJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"key only", map[string]string{"OPENAI_API_KEY": "k"}, `{"api_key":"k"}`},
		{"key and base", map[string]string{"OPENAI_API_KEY": "k", "OPENAI_BASE_URL": "http://h/v1"}, `{"api_key":"k","base_url":"http://h/v1"}`},
		{"base only", map[string]string{"OPENAI_BASE_URL": "http://h/v1"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestResolver(t, tc.env, nil)
			b, err := r.Secret("openai", "llm")
			if tc.want == "" {
				if err == nil || r.Status("openai", "llm").Detected {
					t.Fatalf("want not detected, got %q", b)
				}
				return
			}
			if err != nil || string(b) != tc.want {
				t.Fatalf("secret = %q, %v", b, err)
			}
		})
	}
}

func TestNVIDIA(t *testing.T) {
	r := newTestResolver(t, map[string]string{"NVIDIA_API_KEY": "k"}, nil)
	if b, err := r.Secret("nvidia", "llm"); err != nil || string(b) != "k" {
		t.Fatalf("llm = %q, %v", b, err)
	}
	for svc, env := range map[string]string{"stt": "NVIDIA_STT_FUNCTION_ID", "tts": "NVIDIA_TTS_FUNCTION_ID"} {
		st := r.Status("nvidia", svc)
		if st.Detected || !reflect.DeepEqual(st.MissingEnv, []string{env}) {
			t.Fatalf("%s status = %+v", svc, st)
		}
	}
	r = newTestResolver(t, map[string]string{"NVIDIA_API_KEY": "k", "NVIDIA_STT_FUNCTION_ID": "f1", "NVIDIA_TTS_FUNCTION_ID": "f2"}, nil)
	want := `{"api_key":"k","function_id":"f1","tts_function_id":"f2"}`
	for _, svc := range []string{"stt", "tts"} {
		if b, err := r.Secret("nvidia", svc); err != nil || string(b) != want {
			t.Fatalf("%s = %q, %v", svc, b, err)
		}
	}
}

func TestLive(t *testing.T) {
	env := map[string]string{"XAI_API_KEY": "one"}
	r := newTestResolver(t, env, nil)
	if b, err := r.Secret("xai", "llm"); err != nil || string(b) != "one" {
		t.Fatalf("first = %q, %v", b, err)
	}
	env["XAI_API_KEY"] = "two"
	if b, err := r.Secret("xai", "llm"); err != nil || string(b) != "two" {
		t.Fatalf("changed = %q, %v", b, err)
	}
	delete(env, "XAI_API_KEY")
	if _, err := r.Secret("xai", "llm"); err == nil || r.Status("xai", "llm").Detected {
		t.Fatal("deleted variable must not resolve")
	}
}

func TestUnknown(t *testing.T) {
	r := newTestResolver(t, map[string]string{"DEEPGRAM_API_KEY": "k"}, nil)
	for _, c := range [][2]string{{"nope", "llm"}, {"xai", "video"}, {"deepgram", "llm"}} {
		if _, err := r.Secret(c[0], c[1]); !errors.Is(err, ErrUnknown) {
			t.Fatalf("%v: err = %v", c, err)
		}
		if r.Status(c[0], c[1]).Declared {
			t.Fatalf("%v declared", c)
		}
	}
}

func TestEnvDetectedMasked(t *testing.T) {
	r := newTestResolver(t, map[string]string{"XAI_API_KEY": "abcdefgh", "NVIDIA_API_KEY": "xy"}, nil)
	want := []string{"NVIDIA_API_KEY", "NVIDIA_STT_FUNCTION_ID", "NVIDIA_TTS_FUNCTION_ID"}
	if got := r.Env("nvidia"); !reflect.DeepEqual(got, want) {
		t.Fatalf("env = %v", got)
	}
	if got := r.Masked("xai"); got != "efgh" {
		t.Fatalf("masked = %q", got)
	}
	if got := r.Masked("nvidia"); got != "xy" {
		t.Fatalf("short masked = %q", got)
	}
	if r.Masked("openai") != "" || r.Detected("openai") || !r.Detected("xai") || r.Detected("nope") {
		t.Fatal("detected/masked mismatch")
	}
}
