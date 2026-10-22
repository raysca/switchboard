package server

import (
	"errors"
	"strings"
	"time"

	"govoice/core"
	coreturn "govoice/core/turn"
	"govoice/server/agent"
)

// IdleConfig enables the user-idle prompt/hang-up and the bot-idle (stuck
// model or TTS) watchdog (Pipecat UserIdleController and idle_timeout). Both
// are server-wide defaults: user idle is on by default (15 s, 2 asks, a spoken goodbye;
// user_timeout: 0 turns it off) and a flow can replace it (flow idle). The bot-idle watchdog
// is off by default. Neither adds a processor when off.
type IdleConfig struct {
	// UserTimeout is the silence after the bot stops speaking (or the call
	// starts) before a prompt (0 = off). Prompts are UserTimeout apart; the call ends one UserTimeout
	// after the last of MaxPrompts (default 2, >= 1).
	UserTimeout time.Duration `yaml:"user_timeout"`
	MaxPrompts  int           `yaml:"max_prompts"`
	// Prompt is the nudge text (default "Are you still there?"). By default the
	// model is told, in a labelled non-user message, to say something like it;
	// with prompt_mode: say it is spoken verbatim without the model.
	Prompt     string `yaml:"prompt"`
	PromptMode string `yaml:"prompt_mode"` // "" | llm | say
	// Goodbye is spoken (TTS, no model) before hanging up for user idle.
	Goodbye string `yaml:"goodbye"`
	// BotTimeout: a finished user turn with no bot speech after this long
	// cancels the generation and sends the client an error (0 = off). After
	// BotMaxStuck such turns in a row (default 3) the call ends. BotFallback is
	// spoken after each cancel (TTS, no model). It also cancels an LLM failover
	// in flight: keep it above failover.llm_first_token x (chain length - 1) for
	// a hung primary to be rescued, above failover.llm_budget for every failover to finish.
	BotTimeout  time.Duration `yaml:"bot_timeout"`
	BotMaxStuck int           `yaml:"bot_max_stuck"`
	BotFallback string        `yaml:"bot_fallback"`
}

// The built-in quiet-caller defaults: what the server config starts from and what a flow's idle settings
// fall back to for any member they leave out.
const (
	defaultUserTimeout = 15 * time.Second
	defaultMaxPrompts  = 2
	defaultIdlePrompt  = "Are you still there?"
	defaultIdleBye     = "I'll end the call now. Goodbye."
)

// forCall is the idle config one call runs: the server's, unless its flow sets idle. A flow's idle
// replaces the user-idle members as one unit (members it leaves out take the built-in defaults, so what
// the editor shows is what the call does); the bot-idle watchdog always stays the server's.
func (c IdleConfig) forCall(f *agent.IdleSettings) IdleConfig {
	if f == nil {
		return c
	}
	c.UserTimeout, c.MaxPrompts, c.Prompt, c.PromptMode, c.Goodbye = defaultUserTimeout, defaultMaxPrompts, defaultIdlePrompt, "", defaultIdleBye
	if f.UserTimeoutMS != nil {
		c.UserTimeout = time.Duration(*f.UserTimeoutMS) * time.Millisecond
	}
	if f.MaxPrompts != nil {
		c.MaxPrompts = *f.MaxPrompts
	}
	if p := strings.TrimSpace(f.Prompt); p != "" {
		c.Prompt = p
	}
	c.PromptMode = f.PromptMode
	if g := strings.TrimSpace(f.Goodbye); g != "" {
		c.Goodbye = g
	}
	return c
}

func (c IdleConfig) enabled() bool { return c.UserTimeout > 0 || c.BotTimeout > 0 }

func (c IdleConfig) core() core.IdleConfig {
	return core.IdleConfig{UserTimeout: c.UserTimeout, MaxPrompts: c.MaxPrompts, UserPrompt: c.Prompt, Say: c.PromptMode == "say",
		Goodbye: c.Goodbye, BotTimeout: c.BotTimeout, BotMaxStuck: c.BotMaxStuck, BotFallback: c.BotFallback}
}

func (c IdleConfig) validate(dtmfAggregate bool, dtmfTimeout time.Duration) error {
	if err := c.core().Validate(); err != nil {
		return err
	}
	switch c.PromptMode {
	case "", "llm", "say":
	default:
		return errors.New("idle.prompt_mode must be llm or say")
	}
	if dtmfAggregate && c.UserTimeout > 0 && c.UserTimeout <= max(dtmfTimeout, 2*time.Second) {
		return errors.New("idle.user_timeout must exceed dtmf.aggregate_timeout (default 2s): a key press is not silence")
	}
	return nil
}

// stage builds the call's idle controller, nil when idle handling is off.
// Its prompts, fallback and goodbye are spoken through speak (the assistant's Say), so they are assistant turns.
func (c IdleConfig) stage(ctx *core.ConversationContext, tools *coreturn.ToolActivity, turn *coreturn.Processor, answering func() bool, speak func(string) error, held func() bool, force bool) (*core.IdleController, error) {
	if !c.enabled() && !force {
		return nil, nil
	}
	cfg := c.core()
	cfg.History, cfg.Answering, cfg.Speak = ctx, answering, speak
	if tools != nil {
		cfg.ToolsBusy = tools.Active
	}
	cfg.Muted = held // Muted only gates user-idle prompts
	if turn != nil {
		cfg.Muted = func() bool { return turn.Muted() || held() }
	}
	return core.NewIdleController(cfg)
}
