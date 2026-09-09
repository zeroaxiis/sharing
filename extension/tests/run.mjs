/**
 * Test entry point:  node tests/run.mjs   (or `npm test`)
 *
 * Node 24 strips TypeScript types natively, so the .ts test files run directly.
 * The only thing Node cannot do on its own is resolve the project's `@/` path
 * alias, which is what the resolve hook below adds. Written in plain JS so it
 * stays outside the extension's DOM-only tsconfig, which has no @types/node.
 */

import { registerHooks } from 'node:module';
import { statSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

/** The extension root, i.e. the directory `@` points at. */
const ROOT = new URL('../', import.meta.url);

function resolveAlias(relative) {
  const candidates = [relative + '.ts', relative + '/index.ts', relative + '.tsx', relative];
  for (const candidate of candidates) {
    const url = new URL(candidate, ROOT);
    try {
      if (statSync(fileURLToPath(url)).isFile()) return url.href;
    } catch {
      // Not this one; try the next candidate.
    }
  }
  return null;
}

registerHooks({
  resolve(specifier, context, nextResolve) {
    if (specifier.startsWith('@/')) {
      const url = resolveAlias(specifier.slice(2));
      if (url) return { url, shortCircuit: true };
    }
    return nextResolve(specifier, context);
  },
});

console.log('');
console.log('sharing transport core');
console.log('');

await import('./transfer.test.ts');
await import('./webrtc.test.ts');
await import('./discovery.test.ts');

const { runAll } = await import('./harness.ts');
const failures = await runAll();
process.exitCode = failures > 0 ? 1 : 0;
