/**
 * A ~60-line test harness.
 *
 * The extension has no test runner dependency, and adding one for three unit
 * tests would be more machinery than the tests themselves. Node 24 strips
 * TypeScript types natively, so `node tests/run.mjs` executes these files
 * directly; run.mjs only teaches Node how to resolve the `@/` alias.
 *
 * Deliberately free of `node:` imports so these files typecheck under the
 * extension's DOM-only tsconfig, with no @types/node.
 */

export type TestFn = () => void | Promise<void>;

interface TestCase {
  name: string;
  fn: TestFn;
}

const cases: TestCase[] = [];

export function test(name: string, fn: TestFn): void {
  cases.push({ name, fn });
}

export function assert(condition: boolean, message: string): asserts condition {
  if (!condition) throw new Error(message);
}

export function assertEqual<T>(actual: T, expected: T, message: string): void {
  if (!Object.is(actual, expected)) {
    throw new Error(message + ' — expected ' + String(expected) + ', got ' + String(actual));
  }
}

export function assertBytesEqual(actual: Uint8Array, expected: Uint8Array, message: string): void {
  if (actual.byteLength !== expected.byteLength) {
    throw new Error(
      message + ' — length ' + actual.byteLength + ' !== ' + expected.byteLength,
    );
  }
  for (let i = 0; i < expected.byteLength; i += 1) {
    if (actual[i] !== expected[i]) {
      throw new Error(message + ' — byte ' + i + ' differs');
    }
  }
}

export async function assertThrows(fn: () => unknown, message: string): Promise<void> {
  try {
    await fn();
  } catch {
    return;
  }
  throw new Error(message + ' — expected a throw, got none');
}

/** Yields to the macrotask queue so pending awaits and timers can run. */
export function tick(times = 1): Promise<void> {
  let chain = Promise.resolve();
  for (let i = 0; i < times; i += 1) {
    chain = chain.then(() => new Promise<void>((resolve) => setTimeout(resolve, 0)));
  }
  return chain;
}

/** Runs every registered case. Returns the number of failures. */
export async function runAll(): Promise<number> {
  let failed = 0;
  for (const testCase of cases) {
    try {
      await testCase.fn();
      console.log('  PASS  ' + testCase.name);
    } catch (error) {
      failed += 1;
      const detail = error instanceof Error ? error.message : String(error);
      console.log('  FAIL  ' + testCase.name);
      console.log('        ' + detail);
    }
  }
  console.log('');
  console.log(cases.length - failed + ' passed, ' + failed + ' failed');
  return failed;
}
