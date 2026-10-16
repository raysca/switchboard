// The simulated echo provider needs no keys; its settings live in the server config. instant = no
// provider latency, label_replies = every reply names its step (the suites tell steps apart by it).
// The development profile points providers.offline at a local stack (offline/); blank endpoints keep that
// provider undetected, so a developer's running stack cannot change what the suite offers.
// The development profile points providers.offline at a local stack (offline/); blank endpoints keep that
// provider undetected, so a developer's running stack cannot change what the suite offers.
// Throws if the config has no top-level `providers:` line to hook on, instead of silently leaving
// the stack on echo's defaults (google timing, typed turns only).
export function withEchoProvider(config: string): string {
  const out = config.replace(/^providers:$/m, "providers:\n  echo:\n    mimic: instant\n    label_replies: true\n  offline:\n    llm_endpoint: \"\"\n    stt_endpoint: \"\"\n    tts_endpoint: \"\"");
  if (out === config) throw new Error("config/development.yaml has no top-level `providers:` line to add providers.echo to");
  return out;
}
