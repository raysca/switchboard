// Vocabulary guard: the product says "agent" (was flow) and "handoff" (was route). Keeps the old words out of tracked files.
//   node scripts/check-vocabulary.mjs [--report]
// Exit 1 on a hit no allowlist rule covers, or on an allowlist rule that matches nothing. --report prints counts and exits 0.
// Allowlist (scripts/vocabulary-allow.txt), tab-separated: <glob>  <ids F|R|F,R>  <literal fragment or *>  # reason
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import assert from 'node:assert/strict';

const PATTERNS = [
  ['F1', 'F', /(?<![a-z])flow(?!er)/],
  ['F2', 'F', /Flow/],
  ['F3', 'F', /(?<![A-Z])FLOW/],
  ['R1', 'R', /(?<![a-z/<])[Rr]out(?:e|es|ed|ing)(?![a-z/])/],
  ['R2', 'R', /(?<=[a-z])Route(?!r)/],
  ['R3', 'R', /(?<![A-Z])ROUTES?(?![A-Z])/],
];
// Families (F, R) a line hits.
const families = line => [...new Set(PATTERNS.filter(([, , re]) => re.test(line)).map(([, f]) => f))];

const globRe = g => new RegExp('^' + g.replace(/[.+^${}()|[\]\\?]/g, '\\$&').replace(/\*\*/g, '\0').replace(/\*/g, '[^/]*').replace(/\0/g, '.*') + '$');
const rules = readFileSync('scripts/vocabulary-allow.txt', 'utf8').split('\n').filter(l => l.trim() && !l.startsWith('#')).map((l, i) => {
  const [glob, ids, frag] = l.split('\t');
  assert.ok(glob && ids && frag, `vocabulary-allow.txt: malformed rule ${l}`);
  return { glob, re: globRe(glob), ids: ids.split(','), frag: frag.replace(/\s+#.*$/, ''), used: 0 };
});

// Banned families left on `line` of `f` after blanking only the allowlisted fragments (a `*` rule waives the family on the whole line).
const residual = (f, line) => families(line).filter(fam => {
  const hit = rules.filter(r => r.re.test(f) && r.ids.includes(fam) && (r.frag === '*' || line.includes(r.frag)));
  hit.forEach(r => r.used++);
  if (hit.some(r => r.frag === '*')) return false;
  return families(hit.reduce((l, r) => l.split(r.frag).join(' '), line)).includes(fam);
});

// Self-check: the guard must catch the old words and leave the unrelated ones alone.
const BANNED = ['flow_id', 'FlowID', 'newFlowRig', 'GOVOICE_FLOW_ID', '"routes"', 'route_cue', 'onRoute', 'RouteEvent', 'voice_route_seconds', 'MAX_ROUTES', 'g.Routes'];
const ALLOWED = ['overflow', 'workflow', 'Workflow', '@xyflow/react', 'goroutine', 'Router', 'NewRouter', 'MemoryRouter', '"../routes/agents/AgentPage"', '<Route path=', 'router'];
for (const s of BANNED) assert.ok(families(s).length, `self-check: should ban ${s}`);
for (const s of ALLOWED) assert.deepEqual(families(s), [], `self-check: should allow ${s}`);

const PM = 'docs/pipecat-parity-matrix.md';
assert.deepEqual(residual(PM, 'pipecat-flows'), [], 'self-check: pipecat-flows alone is allowed in the parity matrix');
assert.deepEqual(residual(PM, 'pipecat-flows and a flow'), ['F'], 'self-check: a product flow beside pipecat-flows is banned');
rules.forEach(r => { r.used = 0; });

const SELF = new Set(['scripts/check-vocabulary.mjs', 'scripts/vocabulary-allow.txt']);
const files = execFileSync('git', ['ls-files', '-z', '--', '.', ':!docs/superpowers', ':!pipecat-impl', ':!third_party', ':!**/node_modules/**', ':!**/package-lock.json', ':!reports'], { encoding: 'utf8', maxBuffer: 1 << 28 })
  .split('\0').filter(f => f && !SELF.has(f));
const counts = { F: 0, R: 0 };
const bad = [];
for (const f of files) {
  for (const fam of families(f)) counts[fam]++;
  for (const fam of residual(f, f)) bad.push(`${f}: [${fam}] (path)`);
  let text;
  try { text = readFileSync(f); } catch { continue; }
  if (text.includes(0)) continue;
  for (const [i, line] of text.toString('utf8').split('\n').entries()) {
    for (const fam of families(line)) counts[fam]++;
    for (const fam of residual(f, line)) bad.push(`${f}:${i + 1}: [${fam}] ${line.trim().slice(0, 120)}`);
  }
}
const unused = rules.filter(r => !r.used).map(r => `unused allowlist rule: ${r.glob}\t${r.frag}`);
if (process.argv.includes('--report')) {
  console.log(`check-vocabulary: ${files.length} files; flow hits ${counts.F}, route hits ${counts.R}; not allowlisted ${bad.length}; ${rules.length} rules, ${unused.length} unused`);
  unused.forEach(u => console.log(u));
  process.exit(0);
}
[...bad, ...unused].forEach(m => console.log(m));
console.log(`check-vocabulary: ${bad.length} banned hit(s), ${unused.length} unused rule(s)`);
process.exit(bad.length || unused.length ? 1 : 0);
