package server

import (
	"errors"
	"time"

	"govoice/core"
	coreturn "govoice/core/turn"
)

// IdleConfig enables the user-idle prompt/hang-up and the bot-idle (stuck
// model or TTS) watchdog (Pipecat UserIdleController and idle_timeout). Both
// are off by default and add no processor when off. Server-wide for now:
// per-flow idle settings belong in the flow config (not built yet).
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
// speakIdx is filled in by the caller once the pipeline is laid out.
func (c IdleConfig) stage(ctx *core.ConversationContext, tools *coreturn.ToolActivity, turn *coreturn.Processor, answering, held func() bool, force bool) (*core.IdleController, error) {
	if !c.enabled() && !force {
		return nil, nil
	}
	cfg := c.core()
	cfg.History, cfg.Answering = ctx, answering
	if tools != nil {
		cfg.ToolsBusy = tools.Active
	}
	cfg.Muted = held // Muted only gates user-idle prompts
	if turn != nil {
		cfg.Muted = func() bool { return turn.Muted() || held() }
	}
	return core.NewIdleController(cfg)
}
