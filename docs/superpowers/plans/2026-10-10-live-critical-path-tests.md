# Live Critical-Path Tests Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A nightly and manually triggerable CI job runs real-provider tests on the critical call paths for the `google` and `xai` profiles, within a hard cap of 60 provider calls per run. It includes new live scenarios for push-to-talk, server-side mute and LLM failover.

**Architecture:**
- The agent-level harness `tools/livecheck` already starts a server, provisions an agent from a provider profile, drives calls over the RTVI wire, and enforces `-max-calls`. Its scenarios (S1 to S10) run by hand only.
- This plan adds three scenarios:
  - **S11 push-to-talk:** a client-ready opt-in plus `push-to-talk` messages.
  - **S12 server-side mute:** `set-muted` messages.
  - **S13 LLM failover:** its own server class, so circuit breakers are isolated, and an agent whose primary LLM key is deliberately broken.
- It also adds a `make live-critical` target and a GitHub Actions workflow running two capped steps, google then xai, whose caps sum to 60.
- No server code changes.

**Tech Stack:** Go 1.27 (`tools/livecheck`, package `main`), GitHub Actions, the existing Makefile.

**Spec:** This plan. Decisions from the 2026-10-10 discussion:
- **Schedule:** nightly plus manual dispatch.
- **Profiles:** `google` and `xai` only.
- **Cap:** 60 provider calls per run.
- **Coverage:** live tests target critical paths, not a soak.
- **Docs fix:** `vad.stop_ms` is already configurable (200 to 2000 ms, also per agent; default 800), so the parity matrix's "not configurable" claim is corrected here.

## Global Constraints

- **Cost.** The scheduled job never exceeds 60 provider calls per run. Each workflow step passes an explicit `-max-calls`, and the step caps sum to at most 60. The harness refuses a plan whose estimate exceeds its cap and ends calls when measured requests pass it.
- **What runs nightly:**
  - google: S1, S2, S11, S12, S13.
  - xai: S1, S11, S12, S13.
  - All in mode `off`, with `-s1-turns 1 -s1-short`.
  - S6 (needs a public MCP server) and S9 (long reply, 3x TTS) are manual-only.
- **Secrets.** These come from a GitHub environment named `live-providers`: `GOOGLE_API_KEY`, `GCP_SA_JSON` (service-account JSON, written to a temp file for `GOOGLE_APPLICATION_CREDENTIALS`) and `XAI_API_KEY`. They are never echoed. The harness redacts secrets in its output (`theRedactor`).
- **Clips.** Synthesized clips are cached across runs (`actions/cache`, path `.livecheck-clips`), so a nightly run does not re-buy TTS for clips. `.livecheck-clips/` is git-ignored.
- **Fake mode.** New scenarios must run (or report `n/a` with a reason) under `-selftest`, which uses fake providers and costs nothing. That is how each scenario is tested before it spends money.
- **Commits.** One commit per task, containing only the named files. End each message with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Run Go commands from the repo root. Darwin cgo needs `CGO_LDFLAGS=-Wl,-w`.

## Review Focus

- **The step caps really bound spend.** A step's measured requests can exceed its estimate (retries, a failed primary request). Confirm in Task 5 that the harness aborts at the cap, not just the estimate. `watchSpend` ends the call when measured requests pass `-max-calls`.
- **S13's broken primary must not poison other scenarios.** Breakers are process-wide per provider and stage. S13 runs in its own server class (its own server process), so a rejected key cannot open the breaker for S1, S2, S11 or S12. Task 4 states this, and the scenario table order keeps S13 last.
- **Unpressed speech in S11 still opens an STT stream.** That is a billed STT open per call, by design: STT follows raw VAD. The estimate accounts for it (`extraSTT`).
- **S12 must not hang if the server never sends `user-stopped-speaking`.** Every wait has a deadline, and the check records the measured time or its absence.
- **A provider rejecting a broken key with 5xx or a timeout instead of 4xx.** The failover policy in S13 lists every class, so failover still happens. The check asserts failover occurred, not its class.

---

### Task 1: Correct the parity matrix on VAD stop configurability

**Files:**
- Modify: `docs/pipecat-parity-matrix.md` (the `| VAD |` row; item 2 of "Remaining work toward production")

- [ ] **Step 1: Edit the VAD row.** In the differences cell, replace `Stop after 25 quiet 32 ms chunks (about 0.7-0.8 s, not configurable) vs Pipecat `stop_secs` 0.2 s.` with:

```
Stop after `vad.stop_ms` of silence (default 800 ms, 200 to 2000 in 32 ms steps; per-agent override `vad_stop_ms`) vs Pipecat `stop_secs` 0.2 s.
```

Before writing `vad_stop_ms`, check the agent JSON field name in `server/agent/config.go` (search `VADStopMS`/`vad_stop`) and use the real name.

- [ ] **Step 2: Edit item 2.** Replace its first bullet ("Make the VAD stop configurable: it is fixed at 25 quiet chunks…") with:

```
   - Lower the VAD stop default: `vad.stop_ms` is configurable (default 800 ms) but far above Pipecat's `stop_secs` 0.2 s; pick a default from live measurements.
```

- [ ] **Step 3: Run the matrix check**

Run: `cd tools/compat && go test -run TestParityMatrix -count=1 .`
Expected: `ok`

- [ ] **Step 4: Commit**

```bash
git add docs/pipecat-parity-matrix.md
git commit -m "docs: vad.stop_ms is configurable; the gap is the default"
```

---

### Task 2: S11 push-to-talk scenario

**Files:**
- Modify: `tools/livecheck/rtvi.go` (`dial`, a new `sendMsg`/`ptt`/`setMuted`, and `sendText` reusing `sendMsg`)
- Modify: `tools/livecheck/scenarios.go` (`scenario` struct, table, `s11`)
- Modify: `tools/livecheck/run.go` (`runScenario` passes the opt-in to `dial`)

**Interfaces:**
- Produces: `dial(ctx, s *server, agent string, pushToTalk bool) (*client, error)`; `(*client).sendMsg(typ string, data any) error`; `(*client).ptt(pressed bool) error`; `(*client).setMuted(muted bool) error`; `scenario.pushToTalk bool`; `parisRe`. Task 3 uses `setMuted`.

- [ ] **Step 1: Watch the self-test reject the unknown scenario**

Run: `go run ./tools/livecheck -selftest -scenarios S11`
Expected: an error naming `S11` as an unknown scenario.

If `-selftest` ignores `-scenarios`, read `main.go` around the `*selftest` branch and use whatever selects scenarios there, such as `LIVE_ARGS` in `make live-check-selftest`. Use that same command in the later steps.

- [ ] **Step 2: Client helpers** (`rtvi.go`)

Change `dial`'s signature and its ready frame:

```go
func dial(ctx context.Context, s *server, agent string, pushToTalk bool) (*client, error) {
```

```go
	ptt := ""
	if pushToTalk {
		ptt = `,"push_to_talk":true`
	}
	ready, _ := ws.EncodeMessageFrame(`{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"2.1.0"` + ptt + `}}`)
```

Replace `sendText` with:

```go
// sendMsg sends one RTVI client message.
func (c *client) sendMsg(typ string, data any) error {
	b, _ := json.Marshal(map[string]any{"id": "m1", "label": "rtvi-ai", "type": typ, "data": data})
	f, err := ws.EncodeMessageFrame(string(b))
	if err != nil {
		return err
	}
	return c.conn.Write(context.Background(), websocket.MessageBinary, f)
}

// sendText sends an RTVI send-text client message (a text turn, no audio).
func (c *client) sendText(text string) error {
	return c.sendMsg("send-text", map[string]string{"content": text})
}

// ptt presses (true) or releases (false) the push-to-talk button.
func (c *client) ptt(pressed bool) error { return c.sendMsg("push-to-talk", map[string]bool{"pressed": pressed}) }

// setMuted mutes or unmutes the caller server-side.
func (c *client) setMuted(muted bool) error { return c.sendMsg("set-muted", map[string]bool{"muted": muted}) }
```

In `run.go`'s `runScenario`, change `c, err := dial(ctx, srv, agent)` to `c, err := dial(ctx, srv, agent, it.sc.pushToTalk)`.

- [ ] **Step 3: The scenario** (`scenarios.go`)

Add the field to `scenario`, after `class serverClass`:

```go
	// pushToTalk opts the call into push-to-talk at client-ready.
	pushToTalk bool
```

Append to the `scenarios` table:

```go
	{id: "S11", title: "push-to-talk (unpressed ignored, pause while held, release before final)", class: classBase,
		clips: []string{"question", "pauseA", "pauseB"}, pushToTalk: true,
		// the unpressed utterance and each held segment open an STT stream; one short reply
		shape: func(*opts) shape { return shape{turns: 1, short: true, extraSTT: 2} }, run: s11},
```

Add the function after `s10`:

```go
var parisRe = regexp.MustCompile(`(?i)\bparis\b`)

// s11: push-to-talk. Speech without the button gets no reply; held speech with a pause inside it is
// one turn; releasing right at the end of speech (before the STT final) still answers all of it.
func s11(x *sctx, r *Result) {
	c := x.c
	c.quiet(400 * time.Millisecond)
	mark := time.Now()
	if _, _, err := c.stream(x.clips["question"]); err != nil {
		r.abort(redact(err))
		return
	}
	replied := c.wait(6*time.Second, func() bool { return c.has(mark, "audio") })
	r.check("unpressed speech gets no reply", !replied, "")
	if replied {
		c.wait(25*time.Second, func() bool { return c.has(mark, "bot-stopped-speaking") })
	}
	c.quiet(time.Second)

	mark = time.Now()
	if err := c.ptt(true); err != nil {
		r.abort(redact(err))
		return
	}
	if _, _, err := c.stream(x.clips["pauseA"]); err != nil {
		r.abort(redact(err))
		return
	}
	c.quiet(x.o.pause + 300*time.Millisecond) // a pause while held must not end the turn
	early := c.has(mark, "audio")
	_, tEnd, err := c.stream(x.clips["pauseB"])
	if err != nil {
		r.abort(redact(err))
		return
	}
	if err := c.ptt(false); err != nil { // released at the end of speech, before the final
		r.abort(redact(err))
		return
	}
	r.check("no reply while held", !early, "")
	ok := c.wait(15*time.Second, func() bool { return c.has(tEnd, "audio") })
	r.check("reply after release", ok, "")
	if e, ok := c.firstAfter(tEnd, "audio"); ok {
		r.m("release_to_audio_ms", msv(e.t.Sub(tEnd)))
	}
	c.wait(25*time.Second, func() bool { return c.has(tEnd, "bot-stopped-speaking") })
	heard, said := c.texts(mark, "user-transcription", true), c.texts(mark, "bot-llm-text", false)
	r.m("heard", heard)
	r.m("reply", truncate(said, 90))
	r.check("no rejected client message", !c.has(mark, "error"), "")
	if x.content() {
		h := strings.ToLower(heard)
		r.check("heard both halves", strings.Contains(h, "know") && strings.Contains(h, "france"), heard)
		r.check("answer is Paris", parisRe.MatchString(said), "")
	}
}
```

If `go vet` reports that `regexp` or `strings` is unused or missing in `scenarios.go`, fix the imports. Both are already imported there.

- [ ] **Step 4: Run it offline**

Run: `go run ./tools/livecheck -selftest -scenarios S11` (or the equivalent from Step 1)
Expected: S11 `PASS`. Content checks are skipped with fake providers. "unpressed speech gets no reply" and "reply after release" must pass. If "reply after release" fails, look at `-keep-work` server logs before changing the scenario: the fake STT may not produce a transcript for these clips. In that case, guard only that check with `x.content()` and record a ruling.

- [ ] **Step 5: Build, vet and run the harness unit tests**

Run: `go vet ./tools/livecheck/ && go test ./tools/livecheck/ -count=1`
Expected: PASS. `profiles_test.go` and `estimate_test.go` still pass. If an estimate test enumerates scenarios, extend it with S11 using the same shape.

- [ ] **Step 6: Commit**

```bash
git add tools/livecheck/rtvi.go tools/livecheck/scenarios.go tools/livecheck/run.go
git commit -m "feat(livecheck): S11 push-to-talk scenario"
```

---

### Task 3: S12 server-side mute scenario

**Files:**
- Modify: `tools/livecheck/scenarios.go` (table, `s12`)

**Interfaces:**
- Consumes: `(*client).setMuted` (Task 2).

- [ ] **Step 1: Watch the self-test reject the unknown scenario**

Run: `go run ./tools/livecheck -selftest -scenarios S12`
Expected: an error naming `S12`.

- [ ] **Step 2: The scenario.** Append to the table:

```go
	{id: "S12", title: "server-side mute (ends the segment, muted speech unheard, no phantom after unmute)", class: classBase,
		clips: []string{"pauseA", "pauseB"},
		shape: func(*opts) shape { return shape{turns: 1, short: true} }, run: s12},
```

Add after `s11`:

```go
// s12: server-side mute. Muting mid-utterance ends the speech segment at once (the turn does not
// hang); speech while muted is not heard; after unmute, silence opens no segment.
func s12(x *sctx, r *Result) {
	c := x.c
	c.quiet(400 * time.Millisecond)
	mark := time.Now()
	a := x.clips["pauseA"]
	if _, _, err := c.stream(a[:len(a)*3/4/2*2]); err != nil { // stop mid-utterance
		r.abort(redact(err))
		return
	}
	c.wait(time.Second, func() bool { return c.has(mark, "user-started-speaking") })
	r.check("speech detected before mute", c.has(mark, "user-started-speaking"), "")
	tMute := time.Now()
	if err := c.setMuted(true); err != nil {
		r.abort(redact(err))
		return
	}
	if _, _, err := c.stream(x.clips["pauseB"]); err != nil { // spoken while muted: must not be heard
		r.abort(redact(err))
		return
	}
	stop, ok := c.firstAfter(tMute, "user-stopped-speaking")
	note := "no user-stopped-speaking after mute"
	if ok {
		r.m("mute_to_stop_ms", msv(stop.t.Sub(tMute)))
		note = ""
	}
	r.check("segment ends at mute", ok && stop.t.Sub(tMute) < 1500*time.Millisecond, note)
	c.wait(15*time.Second, func() bool { return c.has(tMute, "bot-stopped-speaking") }) // the cut turn may be answered
	heard := c.texts(mark, "user-transcription", true)
	r.m("heard", heard)
	if x.content() {
		r.check("muted speech not heard", !strings.Contains(strings.ToLower(heard), "france"), heard)
	}
	tUnmute := time.Now()
	if err := c.setMuted(false); err != nil {
		r.abort(redact(err))
		return
	}
	c.quiet(3 * time.Second) // streams silence after unmute
	r.check("no phantom segment after unmute", !c.has(tUnmute, "user-started-speaking"), "")
	r.check("no rejected client message", !c.has(mark, "error"), "")
}
```

- [ ] **Step 3: Run it offline**

Run: `go run ./tools/livecheck -selftest -scenarios S12`
Expected: S12 `PASS`. VAD is real in the self-test and the clips are speech-shaped, so the mute checks are meaningful offline.

- [ ] **Step 4: Vet and unit tests**

Run: `go vet ./tools/livecheck/ && go test ./tools/livecheck/ -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add tools/livecheck/scenarios.go
git commit -m "feat(livecheck): S12 server-side mute scenario"
```

---

### Task 4: S13 LLM failover with a real fallback

**Files:**
- Modify: `tools/livecheck/agent.go` (`storeCreds` refactor, `failoverConfig`, `brokenSecret`, `provisionFailover`)
- Modify: `tools/livecheck/server.go` (`classFailover`, `failovers()`)
- Modify: `tools/livecheck/run.go` (`runGroup` provisions the failover agent for that class)
- Modify: `tools/livecheck/scenarios.go` (table, `s13`)
- Test: `tools/livecheck/agent_test.go` (create)

**Interfaces:**
- Produces: `failoverConfig(p Profile, cred map[string]string, badLLMCred string) map[string]any`; `brokenSecret(s string) string`; `(*server).provisionFailover(p Profile, env func(string) string) (string, error)`; `(*server).failovers() int`; `classFailover`.

- [ ] **Step 1: Write the failing unit tests** (create `tools/livecheck/agent_test.go`)

```go
package main

import (
	"slices"
	"testing"
)

func TestFailoverConfigBreaksPrimaryAndFallsBackToRealKey(t *testing.T) {
	p := Profile{Name: "x"}
	p.Stages.STT = Stage{Provider: "xai"}
	p.Stages.LLM = Stage{Provider: "xai", Model: "grok-3-mini"}
	p.Stages.TTS = Stage{Provider: "xai", Voice: "eve"}
	cfg := failoverConfig(p, map[string]string{"stt": "c-stt", "llm": "c-llm", "tts": "c-tts"}, "c-bad")
	llm := cfg["llm"].(map[string]any)
	if llm["credential_id"] != "c-bad" {
		t.Fatalf("primary credential = %v, want the broken one", llm["credential_id"])
	}
	fb := llm["fallbacks"].([]map[string]any)
	if len(fb) != 1 || fb[0]["credential_id"] != "c-llm" || fb[0]["provider"] != "xai" || fb[0]["model"] != "grok-3-mini" {
		t.Fatalf("fallbacks = %v, want the real key on the same provider and model", fb)
	}
	on := cfg["failover"].(map[string]any)["on"].([]string)
	if !slices.Contains(on, "client_error") {
		t.Fatalf("failover classes %v must include client_error (a rejected key)", on)
	}
}

func TestBrokenSecretChangesTheKeyButKeepsItsShape(t *testing.T) {
	for _, s := range []string{"xai-abcdefghijklmnop", "AIzaSyExampleExampleExample12"} {
		b := brokenSecret(s)
		if b == s || len(b) != len(s) || b[:len(s)-4] != s[:len(s)-4] {
			t.Fatalf("brokenSecret(%q) = %q", s, b)
		}
	}
}
```

Check the real names of the profile stage types in `profiles.go` (`Stage`, and the `Stages` field shape) and adjust the literals to match. The tests' intent doesn't change.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./tools/livecheck/ -run 'TestFailoverConfig|TestBrokenSecret' -count=1`
Expected: FAIL to compile (`failoverConfig` and `brokenSecret` are undefined).

- [ ] **Step 3: Implement** (`agent.go`)

Move the credential-storing loop out of `provision` into a helper:

```go
// storeCreds stores each distinct secret once and returns stage -> credential id.
func (s *server) storeCreds(secrets, stageKey map[string]string) (map[string]string, error) {
	var keys []string
	for k := range secrets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ids := map[string]string{}
	for _, k := range keys {
		theRedactor.add(secrets[k])
		prov, _, _ := strings.Cut(k, "/")
		id, err := s.id("providers/credentials", map[string]string{"provider": prov, "secret": secrets[k]})
		if err != nil {
			return nil, fmt.Errorf("store %s credential: %w", prov, err)
		}
		ids[k] = id
	}
	cred := map[string]string{}
	for stage, k := range stageKey {
		cred[stage] = ids[k]
	}
	return cred, nil
}
```

In `provision`, replace the loop and the `cred` construction with:

```go
	cred, err := s.storeCreds(secrets, stageKey)
	if err != nil {
		return "", err
	}
```

Add:

```go
// failoverConfig is the standard agent with its primary LLM on a broken credential and one
// fallback: the same provider and model on the real credential. Every failure class may fail
// over, client_error included (a rejected key), so a call must be answered by the fallback.
func failoverConfig(p Profile, cred map[string]string, badLLMCred string) map[string]any {
	cfg := agentConfig(p, cred)
	llm := cfg["llm"].(map[string]any)
	llm["fallbacks"] = []map[string]any{{"provider": p.Stages.LLM.Provider, "model": p.Stages.LLM.Model, "credential_id": cred["llm"]}}
	llm["credential_id"] = badLLMCred
	cfg["failover"] = map[string]any{"on": []string{"client_error", "server_error", "timeout", "circuit_open"}}
	return cfg
}

// brokenSecret keeps a key's length and prefix but changes its last characters, so the provider
// rejects it as invalid instead of the server refusing to store it.
func brokenSecret(s string) string {
	if len(s) < 8 {
		return s + "-invalid"
	}
	return s[:len(s)-4] + "zzzz"
}

// provisionFailover creates the failover agent for S13 and returns its id.
func (s *server) provisionFailover(p Profile, env func(string) string) (string, error) {
	secrets, stageKey, err := credPlan(p, env)
	if err != nil {
		return "", err
	}
	cred, err := s.storeCreds(secrets, stageKey)
	if err != nil {
		return "", err
	}
	bad := brokenSecret(secrets[stageKey["llm"]])
	theRedactor.add(bad)
	prov, _, _ := strings.Cut(stageKey["llm"], "/")
	badID, err := s.id("providers/credentials", map[string]string{"provider": prov, "secret": bad})
	if err != nil {
		return "", fmt.Errorf("store broken %s credential: %w", prov, err)
	}
	return s.id("agents", map[string]any{"name": "Live Check Failover", "description": "livecheck failover " + p.Name, "config": failoverConfig(p, cred, badID)})
}
```

Check that the agent config's failover key is `failover` at the top level, by reading where `Failover FailoverPolicy` sits in `server/agent/config.go` (line 31). If it lives under a stage instead, move `cfg["failover"]` to match and update the test.

- [ ] **Step 4: Run the unit tests and confirm they pass**

Run: `go test ./tools/livecheck/ -run 'TestFailoverConfig|TestBrokenSecret' -count=1`
Expected: PASS

- [ ] **Step 5: Server class and failover counter** (`server.go`)

Next to the other classes:

```go
	// classFailover runs S13 in its own server: its broken primary key must not open the
	// process-wide breaker for the other scenarios.
	classFailover = serverClass{"failover", ""}
```

Add after `requestTotal`:

```go
var failoverRe = regexp.MustCompile(`^voice_llm_failover_total\{[^}]*\} (\d+)`)

// failovers is the server's total LLM failovers (voice_llm_failover_total), 0 if unreadable.
func (s *server) failovers() int {
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(s.admin + "/metrics")
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	n := 0
	for _, ln := range strings.Split(string(b), "\n") {
		if m := failoverRe.FindStringSubmatch(ln); m != nil {
			v, _ := strconv.Atoi(m[1])
			n += v
		}
	}
	return n
}
```

Add the `regexp` import if `server.go` doesn't already have it.

- [ ] **Step 6: Provision the failover agent for that class** (`run.go`, `runGroup`)

Replace:

```go
	agent, err := srv.provision(first.prof, cfg.env, mcp)
```

with:

```go
	var agent string
	if first.sc.class.name == classFailover.name {
		agent, err = srv.provisionFailover(first.prof, cfg.env)
	} else {
		agent, err = srv.provision(first.prof, cfg.env, mcp)
	}
```

- [ ] **Step 7: The scenario** (`scenarios.go`). Append to the table, last:

```go
	{id: "S13", title: "LLM failover to a real fallback (primary key rejected)", class: classFailover, clips: []string{"question"},
		// one turn; the rejected primary request counts as one more LLM request
		shape: func(*opts) shape { return shape{turns: 1, short: true, extraLLM: 1} },
		na: func(p Profile, _ mode, _ *opts) string {
			switch {
			case p.Fake:
				return "fake LLM accepts any key"
			case p.Stages.LLM.Provider == "openai":
				return "openai credentials are JSON (key + base URL); brokenSecret would make them unparseable"
			}
			return ""
		}, run: s13},
```

Add after `s12`:

```go
// s13: the primary LLM's key is rejected; the fallback (same provider, real key) answers.
func s13(x *sctx, r *Result) {
	c := x.c
	before := x.srv.failovers()
	c.quiet(400 * time.Millisecond)
	mark := time.Now()
	_, tEnd, err := c.stream(x.clips["question"])
	if err != nil {
		r.abort(redact(err))
		return
	}
	ok := c.wait(20*time.Second, func() bool { return c.has(tEnd, "audio") })
	r.check("answered through the fallback", ok, "")
	c.wait(25*time.Second, func() bool { return c.has(tEnd, "bot-stopped-speaking") })
	said := c.texts(mark, "bot-llm-text", false)
	r.m("reply", truncate(said, 90))
	n := x.srv.failovers() - before
	r.m("llm_failovers", n)
	r.check("LLM failed over", n >= 1, "")
	if x.content() {
		r.check("answer is four", fourRe.MatchString(said), "")
	}
}
```

- [ ] **Step 8: Run it offline (expect n/a), plus vet and the unit tests**

Run: `go run ./tools/livecheck -selftest -scenarios S13; go vet ./tools/livecheck/ && go test ./tools/livecheck/ -count=1`
Expected: S13 reports `N/A` ("fake LLM accepts any key"), and vet and the tests PASS. The live run is in Task 5.

- [ ] **Step 9: Commit**

```bash
git add tools/livecheck/agent.go tools/livecheck/agent_test.go tools/livecheck/server.go tools/livecheck/run.go tools/livecheck/scenarios.go
git commit -m "feat(livecheck): S13 LLM failover to a real fallback, in its own server"
```

---

### Task 5: `make live-critical`, the nightly workflow, and the budget check

**Files:**
- Modify: `Makefile` (new target next to `agent-matrix`)
- Modify: `.gitignore` (`.livecheck-clips/`)
- Create: `.github/workflows/live.yml`
- Modify: `docs/benchmarks/README.md` (a "Nightly critical-path job" section)
- Modify: `docs/pipecat-parity-matrix.md` (item 1 of "Remaining work": the job exists; what's left)

- [ ] **Step 1: The make target** (`Makefile`, after `agent-matrix`)

```make
# live-critical: the nightly critical-path subset (docs/benchmarks/README.md "Nightly critical-path job").
# No examples/.env needed: secrets come from the environment (CI). Mode off, one short S1 turn, cached clips.
live-critical: ## paid|Critical-path live subset (GOVOICE_LIVE=1 PROFILES=... SCENARIOS=... MAX_CALLS=...)
	@test "$$GOVOICE_LIVE" = 1 || { echo "live-critical spends real provider credit: GOVOICE_LIVE=1 make live-critical PROFILES=google SCENARIOS=S1 MAX_CALLS=10"; exit 1; }
	@test -n "$(PROFILES)" && test -n "$(SCENARIOS)" && test -n "$(MAX_CALLS)" || { echo "live-critical needs PROFILES, SCENARIOS and MAX_CALLS"; exit 1; }
	@go run ./tools/livecheck -profiles "$(PROFILES)" -modes off -scenarios "$(SCENARIOS)" -s1-turns 1 -s1-short -clips .livecheck-clips -max-calls $(MAX_CALLS) -yes $(LIVE_ARGS)
```

Add `live-critical` to the `.PHONY` line that lists `live-check agent-matrix …`. Add `.livecheck-clips/` to `.gitignore`.

- [ ] **Step 2: Check the budget with dry runs (spends nothing)**

```bash
go run ./tools/livecheck -dry-run -profiles google -modes off -scenarios S1,S2,S11,S12,S13 -s1-turns 1 -s1-short -max-calls 32
```

```bash
go run ./tools/livecheck -dry-run -profiles xai -modes off -scenarios S1,S11,S12,S13 -s1-turns 1 -s1-short -max-calls 28
```

Expected: each prints a plan whose upper-bound estimate fits its cap.
- **If a plan exceeds its cap:** move caps between the two steps (they must still sum to 60 or less), or drop S2 from google into the manual-only list. Record the final caps and lists as a ruling.
- **If a profile shows as SKIPPED for missing env:** set placeholder values for the required variables just for the dry run (it makes no calls) so the estimate prints.

- [ ] **Step 3: The workflow** (create `.github/workflows/live.yml`, using the caps and lists from Step 2)

```yaml
# Nightly real-provider tests on the critical call paths (docs/benchmarks/README.md
# "Nightly critical-path job"). Spends real credit: at most 60 provider calls per run,
# enforced by the harness (-max-calls per step; the step caps sum to 60).
name: live-critical-paths

on:
  schedule:
    - cron: "17 3 * * *"
  workflow_dispatch:

concurrency:
  group: live-critical-paths
  cancel-in-progress: false

permissions:
  contents: read

jobs:
  live:
    runs-on: ubuntu-24.04
    timeout-minutes: 45
    environment: live-providers
    env:
      GOVOICE_LIVE: "1"
      GOOGLE_API_KEY: ${{ secrets.GOOGLE_API_KEY }}
      XAI_API_KEY: ${{ secrets.XAI_API_KEY }}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: make ort
      - name: Google service account file
        env:
          GCP_SA_JSON: ${{ secrets.GCP_SA_JSON }}
        run: |
          umask 077
          printf '%s' "$GCP_SA_JSON" > "$RUNNER_TEMP/gcp.json"
          echo "GOOGLE_APPLICATION_CREDENTIALS=$RUNNER_TEMP/gcp.json" >> "$GITHUB_ENV"
      - uses: actions/cache@v4
        with:
          path: .livecheck-clips
          key: livecheck-clips-${{ hashFiles('tools/livecheck/clips.go') }}
      - name: google (S1 S2 S11 S12 S13, cap 32)
        run: make live-critical PROFILES=google SCENARIOS=S1,S2,S11,S12,S13 MAX_CALLS=32 LIVE_ARGS="-out .live-out/google"
      - name: xai (S1 S11 S12 S13, cap 28)
        if: ${{ !cancelled() }}
        run: make live-critical PROFILES=xai SCENARIOS=S1,S11,S12,S13 MAX_CALLS=28 LIVE_ARGS="-out .live-out/xai"
      - uses: actions/upload-artifact@v4
        if: ${{ !cancelled() }}
        with:
          name: live-critical-results
          path: .live-out
          retention-days: 30
```

- [ ] **Step 4: Validate the workflow file**

Run: `python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/live.yml'))" && echo ok`
Expected: `ok`. If `actionlint` is installed, also run `actionlint .github/workflows/live.yml`.

- [ ] **Step 5: Document it** (`docs/benchmarks/README.md`, new section)

```markdown
## Nightly critical-path job

`.github/workflows/live.yml` runs every night at 03:17 UTC and on manual dispatch. It runs
`make live-critical` for two profiles in mode `off`, with the call cap split per step (60 per run):

| Profile | Scenarios | Cap |
|---|---|---|
| google | S1 baseline, S2 barge-in, S11 push-to-talk, S12 server-side mute, S13 LLM failover | 32 |
| xai | S1, S11, S12, S13 | 28 |

S6 (tool call, needs a public MCP server) and S9 (long reply) are manual-only (`make agent-matrix`).
Secrets live in the `live-providers` GitHub environment: `GOOGLE_API_KEY`, `GCP_SA_JSON` (the
service-account JSON; the job writes it to a temp file for `GOOGLE_APPLICATION_CREDENTIALS`) and
`XAI_API_KEY`. Synthesized clips are cached between runs. Results (results.json and agent-matrix.md per
profile) are uploaded as the `live-critical-results` artifact; a failed check fails the job.

Run the same thing locally: `GOVOICE_LIVE=1 make live-critical PROFILES=google SCENARIOS=S11 MAX_CALLS=10`.
```

Change the table if Step 2 changed the caps or lists.

- [ ] **Step 6: Update matrix item 1** (`docs/pipecat-parity-matrix.md`). Replace its body with:

```
1. **Automated live tests on critical paths.** `.github/workflows/live.yml` runs nightly and on dispatch: `google` (S1, S2, S11 push-to-talk, S12 server-side mute, S13 LLM failover) and `xai` (S1, S11, S12, S13), capped at 60 provider calls per run (`docs/benchmarks/README.md`). Still open: first live runs for the never-run profiles (`deepgram-openai-cartesia`, `elevenlabs-google-elevenlabs`, `cartesia-nvidia`), and STT/TTS failover scenarios.
```

Run: `cd tools/compat && go test -run TestParityMatrix -count=1 .`
Expected: `ok`

- [ ] **Step 7: One paid local smoke run (needs your approval and credentials; skip it if neither is available)**

Run: `GOVOICE_LIVE=1 make live-critical PROFILES=google SCENARIOS=S11,S12,S13 MAX_CALLS=20`
Expected: S11, S12 and S13 PASS against Google. This is the first real-provider evidence for push-to-talk, server-side mute and failover. If it is skipped, say so in the commit body. The first scheduled run is then the first evidence.

- [ ] **Step 8: Commit**

```bash
git add Makefile .gitignore .github/workflows/live.yml docs/benchmarks/README.md docs/pipecat-parity-matrix.md
git commit -m "ci: nightly live critical-path job (google, xai; 60-call cap)"
```

---

## Final check

```bash
go vet ./tools/livecheck/ && go test ./tools/livecheck/ -count=1
```

```bash
go run ./tools/livecheck -selftest -scenarios S11,S12,S13
```

```bash
cd tools/compat && go test -run TestParityMatrix -count=1 .
```

Expected:
- vet and the unit tests pass;
- the self-test shows S11 and S12 PASS and S13 N/A;
- the matrix check passes.

**Before the first scheduled run:** create the `live-providers` environment and its three secrets in GitHub (repository settings). This is a manual step outside the repo.
