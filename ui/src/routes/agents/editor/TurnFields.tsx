import { useState } from "react";
import type { ServerConfigDTO } from "../../../api/client";
import { SettingHelp } from "./SettingHelp";
import { TURN_RANGE, type StepConfig } from "./model";

type Rec = Record<string, unknown>;
const rec = (v: unknown): Rec => (v && typeof v === "object" && !Array.isArray(v) ? (v as Rec) : {});

const ALWAYS = "always";
const ORDER = [ALWAYS, "first_speech", "until_first_bot_complete", "function_call"];
const DEFAULT_WORDS = 2;
const sameList = (a: string[], b: string[]) => a.length === b.length && a.every((x) => b.includes(x));

type SetKey = (key: string, value: unknown) => void;

/** Members set or removed (null) on the turn object; an empty turn is no turn. A mute list equal to serverMute is no setting. */
function writeTurn(turn: Rec, setKey: SetKey, patch: Rec, serverMute?: string[]) {
  const next: Rec = { ...turn };
  for (const [k, v] of Object.entries(patch)) if (v === null || v === undefined) delete next[k]; else next[k] = v;
  if (serverMute && Array.isArray(next.mute) && sameList(next.mute as string[], serverMute)) delete next.mute;
  setKey("turn", Object.keys(next).length ? next : null);
}

type Props = { cfg: StepConfig; setKey: SetKey; serverConfig?: ServerConfigDTO };

/** How the caller may interrupt the agent: when, which parts are protected, and for how long. */
export function TurnFields({ cfg, setKey, serverConfig }: Props) {
  const [wordsText, setWordsText] = useState<string | null>(null);
  const turn = rec(cfg.turn);
  const st = serverConfig?.turn;
  const serverMute = st?.mute ?? [];
  const agentMute = Array.isArray(turn.mute) ? turn.mute.filter((m): m is string => typeof m === "string") : null;
  const mute = agentMute ?? serverMute; // what a call would use
  const minWords = typeof turn.min_words === "number" ? turn.min_words : null;
  const off = st !== undefined && !st.manager;
  const serverHasAlways = serverMute.includes(ALWAYS);

  // The choice a call would act on: the flow's members over the server's (absent = the server's, 0 = off).
  const serverWords = st?.min_words ?? 0;
  const mode =
    mute.includes(ALWAYS) ? (agentMute?.includes(ALWAYS) ? "never" : "server")
    : minWords !== null ? (minWords === 0 ? "any" : "after")
    : agentMute !== null && serverHasAlways ? (serverWords > 0 ? "after" : "any")
    : "server";
  const shownWords = minWords !== null ? minWords : mode === "after" ? serverWords : 0;

  // Server mute unknown (config loading): never compare against a guess, and never invent a mute list the flow did not have.
  const write = (patch: { min_words?: number | null; mute?: string[] | null; max_mute_ms?: number | null }) =>
    writeTurn(turn, setKey, st || agentMute !== null ? patch : { ...patch, mute: undefined }, st && serverMute);
  const withMute = (name: string, on: boolean) => ORDER.filter((n) => (n === name ? on : mute.includes(n)));
  const rest = mute.filter((m) => m !== ALWAYS); // the list without "always": what every choice but "never" keeps

  const serverSays = !st ? "" : ` (${serverHasAlways ? "not while the agent speaks" : st.min_words > 0 ? `after ${st.min_words} word${st.min_words === 1 ? "" : "s"}` : "any speech interrupts"})`;
  const serverCap = st ? st.max_mute_ms / 1000 : TURN_RANGE.maxMuteS[1];
  const maxSeconds = Math.min(TURN_RANGE.maxMuteS[1], serverCap);
  const minSeconds = Math.min(TURN_RANGE.maxMuteS[0], maxSeconds); // a server cap under the flow minimum leaves one value
  const choices = [
    { id: "server", label: `Server setting${serverSays}`, pick: () => write({ min_words: null, mute: serverHasAlways ? null : rest }) },
    { id: "any", label: "Any time", pick: () => write({ min_words: 0, mute: rest }) },
    { id: "after", label: "After a number of words", pick: () => write({ min_words: minWords !== null && minWords >= 1 ? minWords : DEFAULT_WORDS, mute: rest }) },
    { id: "never", label: "Not while the agent speaks (up to the longest-mute limit)", pick: () => write({ min_words: null, mute: withMute(ALWAYS, true) }) },
  ];

  return (
    <fieldset className="stage-box">
      <legend>Interruptions</legend>
      {off && (
        <>
          <p className="subtext">This server has the turn manager off, so these settings (and the end-of-turn ones above) are not used and cannot be published.</p>
          <button type="button" className="action-button-small" disabled={Object.keys(turn).length === 0} onClick={() => setKey("turn", null)}>
            Remove these settings
          </button>
        </>
      )}
      <fieldset className="stage-box">
        <legend>When can the caller interrupt?</legend>
        {choices.map((c) => (
          <label key={c.id} className="check-row" htmlFor={`flow-turn-${c.id}`}>
            <input id={`flow-turn-${c.id}`} type="radio" name="flow-turn-interrupt" checked={mode === c.id} disabled={off} onChange={c.pick} />
            <span>{c.label}</span>
          </label>
        ))}
        <label className="form-field" htmlFor="flow-turn-words">
          <span>Words before the caller interrupts</span>
          <input
            id="flow-turn-words"
            type="number"
            min={TURN_RANGE.minWords[0]}
            max={TURN_RANGE.minWords[1]}
            disabled={off || mode !== "after"}
            aria-describedby="flow-turn-words-hint"
            value={wordsText ?? (shownWords >= 1 ? shownWords : "")}
            onChange={(e) => {
              setWordsText(e.target.value);
              const n = Number(e.target.value);
              if (e.target.value !== "" && Number.isFinite(n) && n >= 1) write({ min_words: Math.round(n) });
            }}
            onBlur={() => setWordsText(null)}
          />
        </label>
        <p id="flow-turn-words-hint" className="subtext">
          {TURN_RANGE.minWords[0]} to {TURN_RANGE.minWords[1]}. Needs a speech-to-text with interim results.
        </p>
        <SettingHelp id="turn.interruptions" />
      </fieldset>
      <label className="check-row" htmlFor="flow-turn-first">
        <input id="flow-turn-first" type="checkbox" checked={mute.includes("first_speech")} disabled={off} onChange={(e) => write({ mute: withMute("first_speech", e.target.checked) })} />
        <span>Protect the agent's first reply or greeting</span>
      </label>
      <label className="check-row" htmlFor="flow-turn-tool">
        <input id="flow-turn-tool" type="checkbox" checked={mute.includes("function_call")} disabled={off} onChange={(e) => write({ mute: withMute("function_call", e.target.checked) })} />
        <span>Not while a tool runs</span>
      </label>
      {mute.includes("until_first_bot_complete") && <p className="subtext">Also muted until the greeting has finished (set through the API, kept when you save).</p>}
      <SettingHelp id="turn.mute" />
      <label className="form-field" htmlFor="flow-turn-max-mute">
        <span>Longest mute (seconds)</span>
        <input
          id="flow-turn-max-mute"
          type="number"
          min={minSeconds}
          max={maxSeconds}
          disabled={off}
          placeholder={st ? String(st.max_mute_ms / 1000) : "Server setting"}
          aria-describedby="flow-turn-max-mute-hint"
          value={typeof turn.max_mute_ms === "number" ? turn.max_mute_ms / 1000 : ""}
          onChange={(e) => {
            const n = e.target.value.trim() === "" ? null : Number(e.target.value);
            write({ max_mute_ms: n === null || !Number.isFinite(n) ? null : Math.round(n * 1000) });
          }}
        />
      </label>
      <p id="flow-turn-max-mute-hint" className="subtext">
        {minSeconds} to {maxSeconds}. Empty: the server's limit.
      </p>
      <SettingHelp id="turn.max_mute_ms" />
      <p className="subtext">Muted speech still reaches speech-to-text (billed) and recordings.</p>
    </fieldset>
  );
}

const DETECTORS = [
  { id: "default", label: "Standard: wait for silence" },
  { id: "smart", label: "Smart: a model judges if the caller is done" },
  { id: "eager", label: "Fast: end on a full sentence" },
];
const serverDetector = (id: string) => DETECTORS.find((d) => d.id === id)?.label.toLowerCase() ?? id;
const numberOrNull = (text: string) => {
  const n = text.trim() === "" ? null : Number(text);
  return n === null || !Number.isFinite(n) ? null : Math.round(n);
};

/** When the caller's turn ends: how long a pause is waited out, and which detector decides. */
export function EndOfTurnFields({ cfg, setKey, serverConfig }: Props) {
  const turn = rec(cfg.turn);
  const st = serverConfig?.turn;
  const off = st !== undefined && !st.manager;
  const smartOk = st?.smart_turn_available ?? false;
  const detector = typeof turn.end_of_turn === "string" ? turn.end_of_turn : "";
  return (
    <>
      {off && <p className="subtext">This server has the turn manager off. Remove the turn settings under Interruptions.</p>}
      <label className="form-field" htmlFor="flow-turn-speech-timeout">
        <span>Wait for the caller to resume (ms)</span>
        <input
          id="flow-turn-speech-timeout"
          type="number"
          min={TURN_RANGE.speechTimeoutMs[0]}
          max={TURN_RANGE.speechTimeoutMs[1]}
          disabled={off}
          placeholder={st ? String(st.speech_timeout_ms) : "Server setting"}
          aria-describedby="flow-turn-speech-timeout-hint"
          value={typeof turn.speech_timeout_ms === "number" ? turn.speech_timeout_ms : ""}
          onChange={(e) => writeTurn(turn, setKey, { speech_timeout_ms: numberOrNull(e.target.value) })}
        />
      </label>
      <p id="flow-turn-speech-timeout-hint" className="subtext">
        {TURN_RANGE.speechTimeoutMs[0]} to {TURN_RANGE.speechTimeoutMs[1]}. Empty: the server's setting.
      </p>
      <SettingHelp id="turn.speech_timeout_ms" />
      <label className="form-field" htmlFor="flow-turn-end">
        <span>End-of-turn detection</span>
        <select
          id="flow-turn-end"
          disabled={off}
          value={detector}
          onChange={(e) => writeTurn(turn, setKey, { end_of_turn: e.target.value || null, smart_hold_ms: e.target.value === "smart" ? turn.smart_hold_ms : null })}
        >
          <option value="">{st ? `Server setting (${serverDetector(st.end_of_turn)})` : "Server setting"}</option>
          {DETECTORS.map((d) => {
            const unavailable = d.id === "smart" && !smartOk;
            return (
              <option key={d.id} value={d.id} disabled={unavailable}>
                {d.label}
                {unavailable ? " (not available on this server)" : ""}
              </option>
            );
          })}
        </select>
      </label>
      {detector === "eager" && <p className="subtext">Can split "Book a table. For two." into two turns. Best with a speech-to-text that adds punctuation.</p>}
      <SettingHelp id="turn.end_of_turn" />
      {detector === "smart" && (
        <>
          <label className="form-field" htmlFor="flow-turn-smart-hold">
            <span>Smart hold (ms)</span>
            <input
              id="flow-turn-smart-hold"
              type="number"
              min={TURN_RANGE.smartHoldMs[0]}
              max={TURN_RANGE.smartHoldMs[1]}
              disabled={off}
              placeholder={st ? String(st.smart_hold_ms) : "Server setting"}
              aria-describedby="flow-turn-smart-hold-hint"
              value={typeof turn.smart_hold_ms === "number" ? turn.smart_hold_ms : ""}
              onChange={(e) => writeTurn(turn, setKey, { smart_hold_ms: numberOrNull(e.target.value) })}
            />
          </label>
          <p id="flow-turn-smart-hold-hint" className="subtext">
            {TURN_RANGE.smartHoldMs[0]} to {TURN_RANGE.smartHoldMs[1]}. Empty: the server's setting.
          </p>
          <SettingHelp id="turn.smart_hold_ms" />
        </>
      )}
    </>
  );
}
