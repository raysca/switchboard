import type { ServerConfigDTO } from "../../../api/client";
import { SettingHelp } from "./SettingHelp";
import { AUDIO_RANGE, IDLE_RANGE, type StepConfig } from "./model";

type Rec = Record<string, unknown>;
const rec = (v: unknown): Rec => (v && typeof v === "object" && !Array.isArray(v) ? (v as Rec) : {});
const without = (o: Rec, key: string): Rec => {
  const { [key]: _drop, ...rest } = o;
  return rest;
};
const num = (raw: string): number | null => (raw.trim() === "" ? null : Number(raw));
const text = (raw: string): string | null => (raw === "" ? null : raw);

// The built-in wording a call uses where an agent leaves it out (server/idle.go).
const BUILT_IN_PROMPT = "Are you still there?";
const BUILT_IN_GOODBYE = "I'll end the call now. Goodbye.";
const BUILT_IN_WAIT_SECONDS = 15;

type Props = { cfg: StepConfig; setKey: (key: string, value: unknown) => void; serverConfig?: ServerConfigDTO };

/** idle with a member set or removed; an empty object is no object. */
function useIdle({ cfg, setKey }: Props) {
  const idle = rec(cfg.idle);
  const set = (key: string, value: unknown) => {
    const next = value === null ? without(idle, key) : { ...idle, [key]: value };
    setKey("idle", Object.keys(next).length ? next : null);
  };
  return { idle, set };
}

/** The prompt mode a call would use: the agent's own, else the built-in "llm" once the agent sets idle, else the server's. */
const effectiveMode = (idle: Rec, serverMode?: string) => (typeof idle.prompt_mode === "string" ? idle.prompt_mode : Object.keys(idle).length > 0 ? "llm" : (serverMode ?? "llm"));

/** The part of "If the caller goes quiet" every agent sees: on/off and the wait. */
export function QuietCallerBasics(props: Props) {
  const { idle, set } = useIdle(props);
  const serverMs = props.serverConfig?.idle.user_timeout_ms;
  const configured = typeof idle.user_timeout_ms === "number" ? idle.user_timeout_ms : null;
  // An agent that sets idle without a wait gets the built-in 15 s, not the server's.
  const on = configured !== null ? configured > 0 : Object.keys(idle).length > 0 ? true : serverMs === undefined || serverMs > 0;
  const placeholder = Object.keys(idle).length > 0 ? String(BUILT_IN_WAIT_SECONDS) : serverMs ? String(serverMs / 1000) : "Server default";
  return (
    <fieldset className="stage-box">
      <legend>If the caller goes quiet</legend>
      <label className="check-row" htmlFor="agent-idle-on">
        <input
          id="agent-idle-on"
          type="checkbox"
          checked={on}
          onChange={(e) => {
            if (!e.target.checked) return set("user_timeout_ms", 0);
            // Back to the server's own choice, unless the server has it off: then ask for the built-in wait.
            set("user_timeout_ms", serverMs === 0 ? BUILT_IN_WAIT_SECONDS * 1000 : null);
          }}
        />
        <span>End the call if the caller stays silent</span>
      </label>
      <label className="form-field" htmlFor="agent-idle-wait">
        <span>Wait this long (seconds)</span>
        <input
          id="agent-idle-wait"
          type="number"
          min={IDLE_RANGE.timeoutMs[0] / 1000}
          max={IDLE_RANGE.timeoutMs[1] / 1000}
          placeholder={placeholder}
          disabled={!on}
          aria-describedby="agent-idle-wait-hint"
          value={configured && configured > 0 ? configured / 1000 : ""}
          onChange={(e) => {
            const n = num(e.target.value);
            set("user_timeout_ms", n === null ? null : Math.round(n * 1000));
          }}
        />
      </label>
      <p id="agent-idle-wait-hint" className="subtext">
        The call ends one wait after the last question.
      </p>
      <SettingHelp id="idle.user_timeout_ms" />
    </fieldset>
  );
}

/** The rest of the quiet-caller behavior: how many times, what to say, how, and the goodbye. */
export function QuietCallerDetails(props: Props) {
  const { idle, set } = useIdle(props);
  const serverMode = props.serverConfig?.idle.prompt_mode;
  const mode = effectiveMode(idle, serverMode);
  return (
    <fieldset className="stage-box">
      <legend>If the caller goes quiet: the details</legend>
      <label className="form-field" htmlFor="agent-idle-prompts">
        <span>Ask this many times</span>
        <input
          id="agent-idle-prompts"
          type="number"
          min={IDLE_RANGE.prompts[0]}
          max={IDLE_RANGE.prompts[1]}
          placeholder={Object.keys(idle).length > 0 || !props.serverConfig ? "2" : String(props.serverConfig.idle.max_prompts)}
          aria-describedby="agent-idle-prompts-hint"
          value={typeof idle.max_prompts === "number" ? idle.max_prompts : ""}
          onChange={(e) => set("max_prompts", num(e.target.value))}
        />
      </label>
      <p id="agent-idle-prompts-hint" className="subtext">
        1 to 10. Then the call ends.
      </p>
      <SettingHelp id="idle.max_prompts" />
      <label className="form-field" htmlFor="agent-idle-prompt">
        <span>What to say</span>
        <textarea
          id="agent-idle-prompt"
          rows={2}
          placeholder={BUILT_IN_PROMPT}
          aria-describedby="agent-idle-prompt-hint"
          value={typeof idle.prompt === "string" ? idle.prompt : ""}
          onChange={(e) => set("prompt", text(e.target.value))}
        />
      </label>
      <p id="agent-idle-prompt-hint" className="subtext">
        Empty: {BUILT_IN_PROMPT} At most {IDLE_RANGE.textBytes} bytes.
      </p>
      <SettingHelp id="idle.prompt" />
      <fieldset className="stage-box" aria-label="How to say it">
        <label className="check-row" htmlFor="agent-idle-mode-llm">
          <input id="agent-idle-mode-llm" type="radio" name="agent-idle-mode" checked={mode !== "say"} onChange={() => set("prompt_mode", serverMode === "say" && Object.keys(idle).length === 0 ? "llm" : null)} />
          <span>Let the model word it</span>
        </label>
        <label className="check-row" htmlFor="agent-idle-mode-say">
          <input id="agent-idle-mode-say" type="radio" name="agent-idle-mode" checked={mode === "say"} onChange={() => set("prompt_mode", "say")} />
          <span>Say it exactly</span>
        </label>
        <SettingHelp id="idle.prompt_mode" />
      </fieldset>
      <label className="form-field" htmlFor="agent-idle-goodbye">
        <span>Goodbye before hanging up</span>
        <textarea
          id="agent-idle-goodbye"
          rows={2}
          placeholder={BUILT_IN_GOODBYE}
          aria-describedby="agent-idle-goodbye-hint"
          value={typeof idle.goodbye === "string" ? idle.goodbye : ""}
          onChange={(e) => set("goodbye", text(e.target.value))}
        />
      </label>
      <p id="agent-idle-goodbye-hint" className="subtext">
        Empty: {BUILT_IN_GOODBYE} At most {IDLE_RANGE.textBytes} bytes.
      </p>
      <SettingHelp id="idle.goodbye" />
    </fieldset>
  );
}

/** Noise reduction, voice volume and a background sound. */
export function AudioFields({ cfg, setKey, serverConfig }: Props) {
  const audio = rec(cfg.audio);
  const set = (key: string, value: unknown) => {
    const next = value === null ? without(audio, key) : { ...audio, [key]: value };
    setKey("audio", Object.keys(next).length ? next : null);
  };
  const denoise = audio.denoise !== undefined ? rec(audio.denoise) : null;
  const bg = audio.background !== undefined ? rec(audio.background) : null;
  const bgName = typeof bg?.name === "string" ? bg.name : "";
  const setBg = (key: string, value: unknown) => set("background", value === null ? without(bg ?? {}, key) : { ...bg, [key]: value });
  const holdSound = audio.tool_cue !== undefined ? rec(audio.tool_cue) : null;
  const holdName = typeof holdSound?.name === "string" ? holdSound.name : "";
  const cues = serverConfig?.audio.cues ?? [];
  const holdKnown = !holdName || !serverConfig || cues.includes(holdName);
  const setHold = (key: string, value: unknown) => {
    const next = value === null ? without(holdSound ?? {}, key) : { ...holdSound, [key]: value };
    set("tool_cue", typeof next.name === "string" && next.name !== "" ? next : null);
  };
  return (
    <fieldset className="stage-box">
      <legend>Audio</legend>
      <label className="check-row" htmlFor="agent-denoise">
        <input id="agent-denoise" type="checkbox" checked={denoise !== null} onChange={(e) => set("denoise", e.target.checked ? {} : null)} />
        <span>Reduce background noise</span>
      </label>
      {denoise !== null && (
        <label className="form-field" htmlFor="agent-denoise-strength">
          <span>Noise reduction strength (%)</span>
          <input
            id="agent-denoise-strength"
            type="number"
            min={1}
            max={100}
            placeholder="50"
            value={typeof denoise.strength === "number" ? Math.round(denoise.strength * 100) : ""}
            onChange={(e) => {
              const n = num(e.target.value);
              set("denoise", n === null ? {} : { strength: n / 100 });
            }}
          />
        </label>
      )}
      <SettingHelp id="audio.denoise" />
      <label className="form-field" htmlFor="agent-gain">
        <span>Voice volume (dB)</span>
        <input
          id="agent-gain"
          type="number"
          min={AUDIO_RANGE.gain[0]}
          max={AUDIO_RANGE.gain[1]}
          placeholder="0"
          aria-describedby="agent-gain-hint"
          value={typeof audio.output_gain_db === "number" ? audio.output_gain_db : ""}
          onChange={(e) => set("output_gain_db", num(e.target.value))}
        />
      </label>
      <p id="agent-gain-hint" className="subtext">
        -30 to +6. Empty: the server's setting (0 unless it says otherwise).
      </p>
      <SettingHelp id="audio.output_gain_db" />
      <BackgroundSelect id="agent-bg" bg={bg} setBg={(next) => set("background", next)} serverConfig={serverConfig} />
      <SettingHelp id="audio.background" />
      {bg !== null && bgName !== "" && (
        <>
          <label className="form-field" htmlFor="agent-bg-gain">
            <span>Background volume (dB)</span>
            <input
              id="agent-bg-gain"
              type="number"
              min={AUDIO_RANGE.backgroundGain[0]}
              max={AUDIO_RANGE.backgroundGain[1]}
              placeholder="-30"
              value={typeof bg.gain_db === "number" ? bg.gain_db : ""}
              onChange={(e) => setBg("gain_db", num(e.target.value))}
            />
          </label>
          <SettingHelp id="audio.background_gain" />
          <label className="check-row" htmlFor="agent-bg-loop">
            <input id="agent-bg-loop" type="checkbox" checked={bg.loop !== false} onChange={(e) => setBg("loop", e.target.checked ? null : false)} />
            <span>Repeat</span>
          </label>
          <SettingHelp id="audio.background_loop" />
        </>
      )}
      <label className="form-field" htmlFor="agent-hold">
        <span>Hold sound</span>
        <select id="agent-hold" value={holdName} onChange={(e) => setHold("name", e.target.value === "" ? null : e.target.value)}>
          <option value="">None</option>
          {!holdKnown && <option value={holdName}>{holdName} (not on this server)</option>}
          {cues.map((n) => (
            <option key={n} value={n}>
              {n}
            </option>
          ))}
        </select>
      </label>
      {!holdKnown && (
        <p role="note" className="note note--bad">
          "{holdName}" is not a sound on this server.
        </p>
      )}
      <label className="form-field" htmlFor="agent-hold-after">
        <span>Start the hold sound after (ms)</span>
        <input
          id="agent-hold-after"
          type="number"
          min={AUDIO_RANGE.holdAfterMs[0]}
          max={AUDIO_RANGE.holdAfterMs[1]}
          placeholder="1000"
          disabled={holdName === ""}
          value={typeof holdSound?.after_ms === "number" ? holdSound.after_ms : ""}
          onChange={(e) => setHold("after_ms", num(e.target.value))}
        />
      </label>
      <SettingHelp id="audio.tool_cue" />
    </fieldset>
  );
}

/**
 * The background sound choice. On the start step it is the call's sound (absent: the server's default, if any).
 * On another step (`step`) absent means "keep the current sound" and None is its own choice, because a step
 * that names nothing leaves whatever is playing.
 */
export function BackgroundSelect({ id, bg, setBg, serverConfig, step = false }: { id: string; bg: Rec | null; setBg: (next: Rec | null) => void; serverConfig?: ServerConfigDTO; step?: boolean }) {
  const bgName = typeof bg?.name === "string" ? bg.name : "";
  const sounds = serverConfig?.audio.backgrounds ?? [];
  const known = !bgName || !serverConfig || sounds.includes(bgName); // not "unknown" while the list is still loading
  const serverDefault = step ? "" : (serverConfig?.audio.background_default ?? "");
  // Start step, no key: the server's default (if any). An empty name: none, even when the server has a default.
  // Sentinel values the sound-name pattern (lowercase letters, digits, - and _) cannot produce.
  const noneValue = step ? "__none__" : "";
  const selected = bg === null ? (serverDefault ? "__default__" : "") : bgName === "" ? noneValue : bgName;
  const denied = serverConfig !== undefined && !serverConfig.audio.allow_agent_background && !bgName;
  return (
    <>
      <label className="form-field" htmlFor={id}>
        <span>Background sound</span>
        <select
          id={id}
          value={selected}
          disabled={denied}
          aria-describedby={denied ? `${id}-denied` : undefined}
          onChange={(e) => {
            const v = e.target.value;
            if (v === "__default__" || (v === "" && (step || !serverDefault))) return setBg(null);
            setBg(v === noneValue ? { name: "" } : { ...bg, name: v });
          }}
        >
          {step && <option value="">Keep the current sound</option>}
          {serverDefault && <option value="__default__">Server default ({serverDefault})</option>}
          <option value={noneValue}>None</option>
          {!known && <option value={bgName}>{bgName} (not on this server)</option>}
          {sounds.map((n) => (
            <option key={n} value={n}>
              {n}
            </option>
          ))}
        </select>
      </label>
      {denied && (
        <p id={`${id}-denied`} className="subtext">
          This server does not allow agent background sounds.
        </p>
      )}
      {!known && (
        <p role="note" className="note note--bad">
          "{bgName}" is not a sound on this server.
        </p>
      )}
    </>
  );
}
