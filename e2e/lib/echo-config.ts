// The simulated echo provider needs no keys; its settings live in the server config. instant = no
// provider latency, label_replies = every reply names its step (the suites tell steps apart by it).
// Throws if the config has no top-level `providers:` line to hook on, instead of silently leaving
// the stack on echo's defaults (google timing, typed turns only).
export function withEchoProvider(config: string): string {
  const out = config.replace(/^providers:$/m, "providers:\n  echo:\n    mimic: instant\n    label_replies: true");
  if (out === config) throw new Error("config/development.yaml has no top-level `providers:` line to add providers.echo to");
  return out;
}
