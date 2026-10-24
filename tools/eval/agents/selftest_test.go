package main

import (
	"bytes"
	"testing"
)

func TestRequireSileroFailsInCI(t *testing.T) {
	root := t.TempDir() // no model inside
	for _, c := range []struct {
		ci     string
		wantRC int
		loud   string
	}{{"", 0, "SKIPPED"}, {"true", 1, "FAILED"}} {
		t.Setenv("CI", c.ci)
		var out, errb bytes.Buffer
		ok, rc := requireSilero(root, &out, &errb)
		if ok || rc != c.wantRC || !bytes.Contains(append(out.Bytes(), errb.Bytes()...), []byte(c.loud)) {
			t.Errorf("CI=%q: ok=%v rc=%d out=%q err=%q", c.ci, ok, rc, out.String(), errb.String())
		}
	}
}
