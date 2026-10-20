/**
 * The copy behind every "Explain" button in the flow editor. One entry per obscure setting; a control
 * names its entry with <SettingHelp id="…" />, and a test keeps the two in step (no dead copy, no
 * control without text).
 */
export type SettingHelpEntry = {
  label: string;
  what: string;
  when?: string;
  defaults: string;
  careful?: string;
};

const COPY = {
  vad_stop_ms: {
    label: "Wait after the caller stops",
    what: "How long the call waits in silence before deciding the caller has finished speaking.",
    when: "Shorter for snappier replies; longer for slow or hesitant speakers.",
    defaults: "200 to 2000 ms. Empty: the server default.",
    careful: "Too short cuts people off mid-sentence.",
  },
  "context.limits": {
    label: "Memory limits",
    what: "How much of the conversation the model is shown; older turns drop off past the limit.",
    when: "Raise for long, detailed calls; lower to cut cost and speed up replies.",
    defaults: "20 messages and 4000 tokens. Max messages is at least 2.",
    careful: "More memory costs more and slows the model.",
  },
  "summarization.enabled": {
    label: "Summarise older turns",
    what: "Condenses the oldest turns into short notes the model keeps, instead of forgetting them.",
    when: "Turn on for long calls where early details still matter.",
    defaults: "Off.",
    careful: "Uses an extra model call when it runs.",
  },
  stt_p99_ms: {
    label: "Wait for a late transcript",
    what: "How long the call waits for the speech-to-text service's final transcript after the caller stops.",
    when: "Raise it if replies sometimes start without the caller's last words.",
    defaults: "100 to 10000 ms. Empty: the server's value for your provider.",
    careful: "Too high adds that delay to every reply that waits.",
  },
  "summarization.trigger_tokens": {
    label: "Summarise-at",
    what: "The history size at which summarising starts.",
    when: "Lower it to summarise earlier.",
    defaults: "Empty: three quarters of Max tokens. Must be below Max tokens.",
  },
  "summarization.keep_turns": {
    label: "Keep the latest turns",
    what: "How many of the newest turns are kept word for word.",
    when: "Raise it if the model loses recent detail.",
    defaults: "Empty: 4.",
  },
  "summarization.prompt": {
    label: "Summary instructions",
    what: "Tells the model what to keep in the notes (names, amounts, promises).",
    when: "When the built-in notes miss what your calls need.",
    defaults: "Empty: the built-in instructions. At most 2048 bytes.",
    careful: "The call transcript is always sent as untrusted data.",
  },
  "failover.on": {
    label: "When to move to a fallback",
    what: "Which kinds of failure make a stage switch to its next fallback. Applies to every step of the call.",
    when: "Rarely. Add rejected requests only if a bad request to one provider may succeed on another.",
    defaults: "Timeouts, server errors and an open circuit breaker.",
    careful: "Rejected requests are usually your bug, not an outage; switching can hide it.",
  },
  async_tools: {
    label: "Runs in the background",
    what: "The model gets an immediate \"working on it\" and is told the result when it arrives.",
    when: "For slow tools (a search or a booking) so the caller is not left waiting in silence.",
    defaults: "Off: the model waits for the result.",
    careful: "The model must handle a late answer gracefully.",
  },
  "idle.user_timeout_ms": {
    label: "Silent caller wait",
    what: "How long the call waits in silence after the agent stops speaking before it asks \"are you still there?\". It asks again after each wait, and the call ends one wait after the last question.",
    when: "Shorter for a quick-moving line; longer when callers look things up while you wait.",
    defaults: "Empty: the server's setting, 15 seconds unless it says otherwise. 1 to 300 seconds. Untick the box to never hang up on silence.",
    careful: "A call nobody is on keeps costing money until the server's call limit.",
  },
  "idle.max_prompts": {
    label: "Questions before hanging up",
    what: "How many times the agent asks if the caller is there before the call ends.",
    when: "Raise it for callers who often step away.",
    defaults: "Empty: 2. 1 to 10.",
  },
  "idle.prompt": {
    label: "What to say",
    what: "The question asked when the caller goes quiet.",
    when: "To match your agent's voice or language.",
    defaults: "Empty: \"Are you still there?\". At most 200 bytes.",
  },
  "idle.prompt_mode": {
    label: "How to say it",
    what: "Whether the model puts the question in its own words, or the agent reads your text exactly as written.",
    when: "Choose exactly as written for wording you must control; it also costs no model call.",
    defaults: "The model words it.",
    careful: "Worded by the model, the question can differ from your text.",
  },
  "idle.goodbye": {
    label: "Goodbye",
    what: "Said, with no model, just before the call ends because the caller stayed silent.",
    when: "To say something kinder or in the caller's language.",
    defaults: "Empty: \"I'll end the call now. Goodbye.\". At most 200 bytes.",
  },
  "audio.denoise": {
    label: "Noise reduction",
    what: "Cleans the caller's audio before the call listens to it: it takes out steady hum and hiss, not other voices.",
    when: "For callers on noisy lines or in cars.",
    defaults: "Off, unless the server already reduces noise on every call (a flow cannot turn that off). Strength 1 to 100%, 50% if you leave it empty.",
    careful: "Costs about 0.1% of one core per call. Recordings keep the original audio.",
  },
  "audio.output_gain_db": {
    label: "Voice volume",
    what: "Makes the agent's voice louder or quieter, in decibels.",
    when: "If your callers find the voice too loud or too quiet.",
    defaults: "Empty: the server's setting, 0 dB (no change) unless it says otherwise. -30 to +6.",
    careful: "Positive values can make loud speech distort.",
  },
  "audio.background": {
    label: "Background sound",
    what: "A sound the server keeps playing quietly under the agent's voice, such as an office.",
    when: "To make a call feel less empty.",
    defaults: "Server default: the sound the server plays on every call, if it has one. Pick None for silence, or one of the sounds your server provides.",
    careful: "It adds continuous audio traffic to every call of this flow.",
  },
  "audio.background_gain": {
    label: "Background volume",
    what: "How loud the background sound is.",
    when: "If it is too loud under speech or too faint to notice.",
    defaults: "Empty: the sound's own setting, usually -30 dB. -60 to 0.",
  },
  "audio.background_loop": {
    label: "Repeat background",
    what: "Plays the sound again when it ends, for as long as the call lasts.",
    when: "Untick for a sound that should play once.",
    defaults: "On.",
  },
  recording: {
    label: "Recording",
    what: "Saves the call's audio on the server (only if the server enables recording too).",
    when: "Only if you need recordings.",
    defaults: "Off.",
    careful: "Tick the disclosure only if callers are actually told: it is your compliance statement.",
  },
  route_cue: {
    label: "Route sound",
    what: "A short sound played while the call moves between steps, so the caller does not hear silence.",
    when: "When a transfer takes more than a moment.",
    defaults: "None. Pick one of the sounds your server has.",
  },
  "turn.interruptions": {
    label: "When can the caller interrupt?",
    what: "Whether the caller's speech cuts the agent off while it talks: at any moment, only after a few words (so a quiet \"mm-hm\" does not), or never until the agent has finished.",
    when: "Never or after a few words for a flow that reads out something the caller must hear; any time for a free-flowing chat.",
    defaults: "The server's setting. After a number of words is 1 to 10 and needs a speech-to-text that sends interim results.",
    careful: "Muted speech still reaches speech-to-text (billed) and recordings; it just starts no turn.",
  },
  "turn.mute": {
    label: "Protect the agent while it works",
    what: "Ignores the caller for the agent's first reply or greeting, or while a tool runs, so neither is cut short.",
    when: "For a greeting with legal wording, or tools that must finish before the caller talks over them.",
    defaults: "The server's list. Ticking a box starts from the server's list and changes that one entry.",
    careful: "A muted caller is not heard at all until the mute ends or the longest-mute limit passes.",
  },
  "turn.max_mute_ms": {
    label: "Longest mute",
    what: "The most time one mute may last before the caller is heard again, so a stuck signal cannot silence them.",
    when: "Shorten it so a caller who really needs to speak is never ignored for long.",
    defaults: "1 to 60 seconds, at most the server's own limit. Empty: the server's limit.",
  },
  "turn.speech_timeout_ms": {
    label: "Wait for the caller to resume",
    what: "After the caller falls silent and a transcript is ready, how long the call waits for them to carry on before the turn is over.",
    when: "Longer for callers who pause to think or read out numbers; shorter for brisk exchanges.",
    defaults: "200 to 3000 ms. Empty: the server's setting.",
    careful: "Too short cuts people off in a pause; too long makes every reply late.",
  },
  "turn.end_of_turn": {
    label: "End-of-turn detection",
    what: "How the call decides the caller has finished. Standard waits for silence. Smart asks a small model whether the sentence is complete. Fast ends the turn as soon as a full sentence has been heard.",
    when: "Smart for slow or hesitant speakers; Fast when the quickest reply matters more than the odd split turn.",
    defaults: "The server's setting. Every choice still falls back to the silence wait above. Smart is offered only when the server has the model loaded.",
    careful: "Fast cannot take a sentence back: \"Book a table. For two.\" becomes two turns, and the second interrupts the reply to the first. Use it with a speech-to-text that adds punctuation.",
  },
  "turn.smart_hold_ms": {
    label: "Smart hold",
    what: "How long a turn the model judges unfinished is kept open before the call replies anyway.",
    when: "Longer for callers who trail off mid-thought.",
    defaults: "500 to 5000 ms. Empty: the server's setting.",
    careful: "The caller hears nothing from the agent for this long after an unfinished sentence.",
  },
  "llm.max_parallel": {
    label: "Tool calls at once",
    what: "How many tool calls the model may run side by side in one round.",
    when: "Raise when the tools are independent lookups.",
    defaults: "1 (one after another). 1 to 16.",
    careful: "Parallel calls load your tool servers harder.",
  },
  "tts.clean_text": {
    label: "Text cleaning",
    what: "Removes markdown, links and emoji so the voice does not read symbols aloud. Applies to every step of the call.",
    when: "Turn off only if your text is already speech-ready.",
    defaults: "On.",
  },
  "tts.voice_settings": {
    label: "Voice settings",
    what: "Tunes the ElevenLabs voice: stability, similarity boost, style (each 0 to 1) and speed (0.7 to 1.2).",
    when: "When the stock voice sounds too flat, too variable or too fast.",
    defaults: "Empty fields keep the ElevenLabs defaults. Voice fallbacks always use their provider defaults.",
  },
  fallbacks: {
    label: "Fallbacks",
    what: "Backup providers tried in order when the main one fails.",
    when: "For flows that must not drop calls on a provider outage.",
    defaults: "None. Up to 2 per stage.",
    careful: "Each fallback needs its own credentials on the server.",
  },
} satisfies Record<string, SettingHelpEntry>;

export type SettingId = keyof typeof COPY;
export const SETTING_HELP: Record<SettingId, SettingHelpEntry> = COPY;
