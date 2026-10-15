package adapter

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Config is read from the environment (LoadConfig); every field has a default that works with the
// compose file.
type Config struct {
	Listen          string            // OFFLINE_LISTEN
	Token           string            // OFFLINE_TOKEN (empty: any bearer token is accepted)
	SpeachesURL     string            // OFFLINE_SPEACHES_URL
	STTModel        string            // OFFLINE_STT_MODEL
	TTSModel        string            // OFFLINE_TTS_MODEL
	DefaultVoice    string            // OFFLINE_DEFAULT_VOICE
	VoiceAliases    map[string]string // OFFLINE_VOICE_ALIASES "eve=af_heart,ara=af_bella"
	MaxConcurrency  int               // OFFLINE_MAX_CONCURRENCY: simultaneous Speaches requests
	QueueWait       time.Duration     // OFFLINE_QUEUE_WAIT: how long a request waits for a slot
	InterimEvery    time.Duration     // OFFLINE_INTERIM_EVERY
	EndpointSilence time.Duration     // OFFLINE_ENDPOINT_SILENCE
	MaxUtterance    time.Duration     // OFFLINE_MAX_UTTERANCE
	EnergyThreshold float64           // OFFLINE_ENERGY_THRESHOLD: RMS of full scale that counts as speech
}

func LoadConfig(getenv func(string) string) (Config, error) {
	c := Config{
		Listen: ":8099", SpeachesURL: "http://127.0.0.1:8000",
		STTModel: "Systran/faster-whisper-small.en", TTSModel: "speaches-ai/Kokoro-82M-v1.0-ONNX",
		DefaultVoice: "af_heart", VoiceAliases: map[string]string{"eve": "af_heart"},
		MaxConcurrency: 2, QueueWait: 5 * time.Second, InterimEvery: 700 * time.Millisecond,
		EndpointSilence: 600 * time.Millisecond, MaxUtterance: 25 * time.Second, EnergyThreshold: 0.01,
	}
	var errs []error
	str := func(key string, dst *string) {
		if v := getenv(key); v != "" {
			*dst = v
		}
	}
	dur := func(key string, dst *time.Duration) {
		if v := getenv(key); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				errs = append(errs, fmt.Errorf("%s: want a positive duration, got %q", key, v))
				return
			}
			*dst = d
		}
	}
	str("OFFLINE_LISTEN", &c.Listen)
	str("OFFLINE_TOKEN", &c.Token)
	str("OFFLINE_SPEACHES_URL", &c.SpeachesURL)
	str("OFFLINE_STT_MODEL", &c.STTModel)
	str("OFFLINE_TTS_MODEL", &c.TTSModel)
	str("OFFLINE_DEFAULT_VOICE", &c.DefaultVoice)
	dur("OFFLINE_QUEUE_WAIT", &c.QueueWait)
	dur("OFFLINE_INTERIM_EVERY", &c.InterimEvery)
	dur("OFFLINE_ENDPOINT_SILENCE", &c.EndpointSilence)
	dur("OFFLINE_MAX_UTTERANCE", &c.MaxUtterance)
	if v := getenv("OFFLINE_MAX_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			errs = append(errs, fmt.Errorf("OFFLINE_MAX_CONCURRENCY: want an integer >= 1, got %q", v))
		} else {
			c.MaxConcurrency = n
		}
	}
	if v := getenv("OFFLINE_ENERGY_THRESHOLD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 || f >= 1 {
			errs = append(errs, fmt.Errorf("OFFLINE_ENERGY_THRESHOLD: want a number in (0,1), got %q", v))
		} else {
			c.EnergyThreshold = f
		}
	}
	if v := getenv("OFFLINE_VOICE_ALIASES"); v != "" {
		c.VoiceAliases = map[string]string{}
		for _, pair := range strings.Split(v, ",") {
			k, val, ok := strings.Cut(strings.TrimSpace(pair), "=")
			if !ok || k == "" || val == "" {
				errs = append(errs, fmt.Errorf("OFFLINE_VOICE_ALIASES: want name=voice pairs, got %q", pair))
				continue
			}
			c.VoiceAliases[strings.TrimSpace(k)] = strings.TrimSpace(val)
		}
	}
	return c, errors.Join(errs...)
}

// Voice maps a requested voice to a Speaches voice: empty is the default, an alias is replaced
// (xAI names such as "eve"), anything else passes through and Speaches rejects an unknown one.
func (c Config) Voice(name string) string {
	if name == "" {
		return c.DefaultVoice
	}
	if v, ok := c.VoiceAliases[name]; ok {
		return v
	}
	return name
}
