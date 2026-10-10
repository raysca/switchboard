package core

import (
	"context"
	"strings"
	"testing"
)

func TestCleanTTSText(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "plain text unchanged",
			input:    "Hello, how can I help you today?",
			expected: "Hello, how can I help you today?",
		},
		{
			name:     "bold asterisks",
			input:    "This is **extremely important** for you.",
			expected: "This is extremely important for you.",
		},
		{
			name:     "bold underscores",
			input:    "This is __very important__ for you.",
			expected: "This is very important for you.",
		},
		{
			name:     "italic asterisks",
			input:    "Please listen *carefully* now.",
			expected: "Please listen carefully now.",
		},
		{
			name:     "italic underscores",
			input:    "Please listen _carefully_ now.",
			expected: "Please listen carefully now.",
		},
		{
			name:     "consecutive italic underscores",
			input:    "Listen _very_ _carefully_ now.",
			expected: "Listen very carefully now.",
		},
		{
			name:     "snake_case preservation",
			input:    "The user_id and session_token_v2 are verified.",
			expected: "The user_id and session_token_v2 are verified.",
		},
		{
			name:     "bold and italic combination",
			input:    "This is ***critical advice*** here.",
			expected: "This is critical advice here.",
		},
		{
			name:     "strikethrough",
			input:    "That was ~~wrong~~ corrected.",
			expected: "That was wrong corrected.",
		},
		{
			name:     "inline code",
			input:    "Run the command `go test ./...` in your shell.",
			expected: "Run the command go test ./... in your shell.",
		},
		{
			name:     "fenced code block with language",
			input:    "Here is the code:\n```go\nfmt.Println(\"hello world\")\n```\nEnjoy!",
			expected: "Here is the code:\nfmt.Println(\"hello world\")\nEnjoy!",
		},
		{
			name:     "markdown links",
			input:    "Visit our website at [Documentation](https://docs.example.com/guide).",
			expected: "Visit our website at Documentation.",
		},
		{
			name:     "markdown image with alt text",
			input:    "Here is a chart: ![Monthly Sales](https://img.example.com/sales.png).",
			expected: "Here is a chart: Monthly Sales.",
		},
		{
			name:     "headers",
			input:    "# Introduction\n## Subtitle\n### Deep dive\nWelcome.",
			expected: "Introduction\nSubtitle\nDeep dive\nWelcome.",
		},
		{
			name:     "bullet points",
			input:    "We have:\n* Option A\n- Option B\n+ Option C",
			expected: "We have:\nOption A\nOption B\nOption C",
		},
		{
			name:     "blockquotes",
			input:    "> To be or not to be, that is the question.",
			expected: "To be or not to be, that is the question.",
		},
		{
			name:     "horizontal rules",
			input:    "Part one\n---\nPart two\n***",
			expected: "Part one\nPart two",
		},
		{
			name:     "HTML and XML tags",
			input:    "Welcome to <span class=\"highlight\">Go Voice</span> <br/> platform.",
			expected: "Welcome to Go Voice platform.",
		},
		{
			name:     "protocol simplification",
			input:    "Go to https://www.example.com or http://test.org for details.",
			expected: "Go to example.com or test.org for details.",
		},
		{
			name:     "emojis stripped",
			input:    "Great job! 🚀🎉 Let's get started. 😃❤️⭐",
			expected: "Great job! Let's get started.",
		},
		{
			name:     "technical emojis and clocks stripped",
			input:    "Wait a moment ⏰⌛ or check the status ⚡.",
			expected: "Wait a moment or check the status.",
		},
		{
			name:     "sentence-split fragments with stray markers",
			input:    "**First part of the sentence.",
			expected: "First part of the sentence.",
		},
		{
			name:     "sentence-split closing fragment with stray markers",
			input:    "Second part of the sentence.**",
			expected: "Second part of the sentence.",
		},
		{
			name:     "stray asterisks and backticks",
			input:    "* some text with stray backtick `",
			expected: "some text with stray backtick",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CleanTTSText(tc.input)
			if got != tc.expected {
				t.Errorf("\nInput:    %q\nExpected: %q\nGot:      %q", tc.input, tc.expected, got)
			}
			// Test idempotency
			secondPass := CleanTTSText(got)
			if secondPass != got {
				t.Errorf("Idempotency violation on %q:\nFirst pass:  %q\nSecond pass: %q", tc.input, got, secondPass)
			}
		})
	}
}

type cleanCaptureEmitter struct {
	frames []Frame
}

func (e *cleanCaptureEmitter) Push(f Frame) error {
	e.frames = append(e.frames, f)
	return nil
}

func (e *cleanCaptureEmitter) PushDirection(f Frame, _ Direction) error {
	e.frames = append(e.frames, f)
	return nil
}

func TestAssistantContextProcessor_TextCleaningIntegration(t *testing.T) {
	ctx := context.Background()

	t.Run("default cleaning enabled", func(t *testing.T) {
		store := NewConversationContext(ContextConfig{MaxMessages: 10, MaxTokens: 500})
		cfg := DefaultAssistantContextConfig()
		p := NewAssistantContextProcessor(store, cfg)
		em := &cleanCaptureEmitter{}

		if err := p.ProcessFrame(ctx, &UserMessageFrame{Turn: "turn-1", Gen: 1}, Downstream, em); err != nil {
			t.Fatalf("UserMessageFrame failed: %v", err)
		}
		if err := p.ProcessFrame(ctx, &LLMTextFrame{Turn: "turn-1", Gen: 1, Text: "Here is **bold** text and an emoji 🚀! How can I help?"}, Downstream, em); err != nil {
			t.Fatalf("LLMTextFrame failed: %v", err)
		}

		var units []*SynthesisUnitFrame
		for _, f := range em.frames {
			if su, ok := f.(*SynthesisUnitFrame); ok {
				units = append(units, su)
			}
		}

		if len(units) == 0 {
			t.Fatal("expected synthesis units, got none")
		}
		for _, u := range units {
			if strings.Contains(u.Text, "**") {
				t.Errorf("unit text still has bold asterisks: %q", u.Text)
			}
			if strings.Contains(u.Text, "🚀") {
				t.Errorf("unit text still has emoji: %q", u.Text)
			}
		}
	})

	t.Run("cleaning disabled", func(t *testing.T) {
		store := NewConversationContext(ContextConfig{MaxMessages: 10, MaxTokens: 500})
		cfg := DefaultAssistantContextConfig()
		cfg.DisableTextCleaning = true
		p := NewAssistantContextProcessor(store, cfg)
		em := &cleanCaptureEmitter{}

		if err := p.ProcessFrame(ctx, &UserMessageFrame{Turn: "turn-1", Gen: 1}, Downstream, em); err != nil {
			t.Fatalf("UserMessageFrame failed: %v", err)
		}
		if err := p.ProcessFrame(ctx, &LLMTextFrame{Turn: "turn-1", Gen: 1, Text: "Here is **bold** text and an emoji 🚀! How can I help?"}, Downstream, em); err != nil {
			t.Fatalf("LLMTextFrame failed: %v", err)
		}

		var units []*SynthesisUnitFrame
		for _, f := range em.frames {
			if su, ok := f.(*SynthesisUnitFrame); ok {
				units = append(units, su)
			}
		}

		if len(units) == 0 {
			t.Fatal("expected synthesis units, got none")
		}
		foundBold := false
		for _, u := range units {
			if strings.Contains(u.Text, "**bold**") {
				foundBold = true
			}
		}
		if !foundBold {
			t.Errorf("expected unit text to preserve **bold**, got: %+v", units)
		}
	})
}
