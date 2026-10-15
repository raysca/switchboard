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
    defaults: "None. The name must be a sound your server has.",
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
    what: "Removes markdown, links and emoji so the voice does not read symbols aloud.",
    when: "Turn off only if your text is already speech-ready.",
    defaults: "On.",
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
