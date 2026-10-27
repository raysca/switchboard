// Docs reference checker (make verify): relative markdown links resolve; with --paths, backticked repo paths exist.
//   node scripts/check-links.mjs [--paths] [pathspec...]
// Default file set: all tracked *.md except docs/history/** and docs/superpowers/** records, whose code blocks look like links.
import { execFileSync } from 'node:child_process';
import { existsSync, readFileSync } from 'node:fs';
import { dirname, join, normalize } from 'node:path';
const argv = process.argv.slice(2);
const paths = argv.includes('--paths');
const specs = argv.filter(a => a !== '--paths');
const git = (...a) => { try { return execFileSync('git', a, { encoding: 'utf8' }); } catch (e) { console.log(`check-links: git ${a[0]} failed: ${e.message}`); process.exit(1); } };
// Explicit pathspecs are checked as given (so the history/superpowers READMEs can be checked); the default set excludes records.
const files = git('ls-files', '-z', '--', ...(specs.length ? specs : ['*.md', ':!docs/history', ':!docs/superpowers']),
  ':!pipecat-impl', ':!third_party', ':!**/node_modules/**').split('\0').filter(f => f.endsWith('.md'));
if (!files.length) { console.log('check-links: no tracked markdown files matched'); process.exit(1); }
const top = /^(AGENTS\.md|README\.md|Makefile|go\.work|\.github|assets|cmd|config|core|deploy|demos|docs|e2e|examples|offline|proto|providers|reports|scripts|server|smartturn|third_party|tools|transport|ui|vad)(\/|$)/;
// Not repo files: runtime outputs (deploy/sbom.json: written by make release-security), operator-created files, Go identifiers, MCP method names, globs.
const allow = new Set(['e2e/.stack.json', 'deploy/govoice.env', 'deploy/sbom.json', 'server/mcp.Handler', 'tools/list', 'examples/...']);
let bad = 0;
const fail = m => { bad++; console.log(m); };
for (const f of files) {
  for (const [i, line] of readFileSync(f, 'utf8').split('\n').entries()) {
    for (const m of line.matchAll(/\]\(([^)\s#]+)(#[^)]*)?\)/g)) {
      const t = m[1];
      if (/^[a-z]+:/i.test(t) || t.startsWith('/')) continue;
      if (!existsSync(normalize(join(dirname(f), decodeURIComponent(t))))) fail(`${f}:${i + 1}: link ${t}`);
    }
    if (!paths) continue;
    for (const m of line.matchAll(/`([A-Za-z0-9_.\/-]+\/[A-Za-z0-9_.\/:-]*)`/g)) {
      const t = m[1].replace(/:\d+(-\d+)?$/, '').replace(/\/$/, '');
      if (top.test(t) && !allow.has(t) && !/(^|\/)(\.env|node_modules)$/.test(t) && !existsSync(t)) fail(`${f}:${i + 1}: path ${t}`);
    }
  }
}
console.log(`check-links: ${bad} problem(s), ${files.length} markdown files`);
process.exit(bad ? 1 : 0);
