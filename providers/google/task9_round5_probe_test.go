package google

import (
	"testing"
	"time"

	tr "govoice/core/transcript"
)

func TestRound5GoogleNonoverlappingRepeatedInterimSurvives(t *testing.T) {
	p, _ := NewSTTProcessor(DefaultSTTConfig())
	st := &activeStream{turnID: "A", gen: 1, id: 1}
	f, _ := tr.NewFinalizer(tr.DefaultConfig())
	at := time.Unix(100, 0)
	first, err := p.assembleResultLocked(st, &STTResult{Transcript: "go", IsFinal: true, ResultEndOffset: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Observe(tr.Candidate{Turn: "A", Generation: 1, Text: first, At: at, Final: true}); err != nil {
		t.Fatal(err)
	}
	p.resetInterimsLocked(st)
	second, err := p.assembleResultLocked(st, &STTResult{Transcript: "go again", ResultEndOffset: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Observe(tr.Candidate{Turn: "A", Generation: 1, Text: second, At: at.Add(time.Millisecond)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Complete("A", 1, at.Add(2*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	out, err := f.Advance(at.Add(3 * time.Second))
	if err != nil || len(out) != 1 || out[0].Text != "go go again" {
		t.Fatalf("nonoverlapping repeated speech removed: snapshot=%q commits=%v err=%v", second, out, err)
	}
}

func TestRound5GoogleFinalThenInterimInSameResponseKeepsBoundary(t *testing.T) {
	p, _ := NewSTTProcessor(DefaultSTTConfig())
	st := &activeStream{turnID: "A", gen: 1, id: 1}
	p.resetInterimsLocked(st)
	for _, result := range []*STTResult{
		{Transcript: "go", IsFinal: true, ResultEndOffset: time.Second},
		{Transcript: "go again", ResultEndOffset: 2 * time.Second},
	} {
		text, err := p.assembleResultLocked(st, result)
		if err != nil {
			t.Fatal(err)
		}
		if result.ResultEndOffset == 2*time.Second && text != "go go again" {
			t.Fatalf("mixed final and interim omitted repeated speech: %q", text)
		}
	}
}
