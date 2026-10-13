package catalog

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func loadOverlay(t *testing.T, files map[string]string) (*Catalog, []string) {
	t.Helper()
	var logs []string
	c, err := LoadWithOverlay(writeFiles(t, files), func(f string, a ...any) { logs = append(logs, f) })
	if err != nil {
		t.Fatal(err)
	}
	return c, logs
}

func voiceIDs(c *Catalog, provider string) (ids []string, label map[string]string, defaults int) {
	p, _ := c.Provider(provider)
	label = map[string]string{}
	for _, e := range p.Services[ServiceTTS].Voices {
		ids = append(ids, e.ID)
		label[e.ID] = e.Label
		if e.Default {
			defaults++
		}
	}
	return
}

func TestOverlayAddsAndReplacesVoices(t *testing.T) {
	c, logs := loadOverlay(t, map[string]string{"xai.yaml": `
id: xai
services:
  tts:
    voices:
      - {id: ara2, label: Ara 2}
      - {id: eve, label: Eve (new)}
`})
	if len(logs) != 0 {
		t.Fatalf("unexpected logs %v", logs)
	}
	ids, label, defaults := voiceIDs(c, "xai")
	// ara2 is appended after the packaged voices; eve is replaced in place; others are untouched.
	if ids[len(ids)-1] != "ara2" || strings.Count(strings.Join(ids, ","), "eve") != 1 || label["eve"] != "Eve (new)" || label["ara"] != "Ara" || defaults != 1 {
		t.Fatalf("got %v %v defaults=%d", ids, label, defaults)
	}
}

func TestOverlayDefaultMoves(t *testing.T) {
	c, _ := loadOverlay(t, map[string]string{"xai.yaml": `
id: xai
services:
  tts:
    voices:
      - {id: ara2, label: Ara 2, default: true}
`})
	p, _ := c.Provider("xai")
	d, _ := DefaultEntry(p.Services[ServiceTTS].Voices)
	_, _, defaults := voiceIDs(c, "xai")
	if d.ID != "ara2" || defaults != 1 {
		t.Fatalf("default %q count %d", d.ID, defaults)
	}
}

func TestOverlayHostileFilesAreSkipped(t *testing.T) {
	base, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"unknown_key.yaml": "id: xai\nbogus: 1\n",
		"big.yaml":         "id: xai\n# " + strings.Repeat("x", MaxFileBytes) + "\n",
		"creds.yaml":       "id: xai\ncredentials:\n  - id: key\n    label: x\n    fields:\n      api_key: {env: EVIL}\n    assemble: token\n",
		"unknown.yaml":     "id: nope\nname: Nope\nservices:\n  tts:\n    voices:\n      - {id: a, label: A}\n",
		"notyaml.yaml":     "\x00: [unterminated",
	}
	c, logs := loadOverlay(t, cases)
	if len(logs) != len(cases) {
		t.Fatalf("want %d log lines, got %d: %v", len(cases), len(logs), logs)
	}
	if len(c.Providers()) != len(base.Providers()) {
		t.Fatal("provider set changed")
	}
	for _, p := range base.Providers() {
		g, _ := c.Provider(p.ID)
		if !reflect.DeepEqual(p, g) {
			t.Errorf("provider %s changed", p.ID)
		}
	}
}

func TestOverlayDirHandling(t *testing.T) {
	if _, err := LoadWithOverlay(filepath.Join(t.TempDir(), "missing"), func(string, ...any) {}); err == nil {
		t.Fatal("missing dir must be an error")
	}
	if c, err := LoadWithOverlay("", nil); err != nil || c == nil {
		t.Fatalf("empty dir: %v", err)
	}
	c, logs := loadOverlay(t, map[string]string{"notes.txt": "id: xai\nbogus: 1\n", "xai.yml": "garbage"})
	if len(logs) != 0 {
		t.Fatalf("non-.yaml files must be ignored: %v", logs)
	}
	if _, ok := c.Provider("xai"); !ok {
		t.Fatal("xai missing")
	}
}

func TestOverlayFIFOIsSkippedNotBlocking(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe.yaml"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	var logs []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := LoadWithOverlay(dir, func(f string, a ...any) { logs = append(logs, f) }); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("a FIFO in the overlay dir blocked start-up")
	}
	if len(logs) != 1 {
		t.Fatalf("logs = %v", logs)
	}
}
