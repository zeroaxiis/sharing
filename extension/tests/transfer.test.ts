import { assert, assertBytesEqual, assertEqual, assertThrows, test, tick } from './harness.ts';
import { FakeChannel } from './fakes.ts';
import {
  TransferEngine,
  decodeChunk,
  encodeChunk,
  type TransferRecord,
} from '@/lib/transfer';
import { CHUNK_SIZE, FRAME_HEADER_BYTES, MAX_INLINE_TEXT } from '@/types';

/** A canonical 36-character UUID, so encodeChunk accepts it. */
const ID = '11111111-2222-4333-8444-555555555555';
const OTHER_ID = '99999999-8888-4777-8666-555555555555';

function engineWith(
  channel: FakeChannel,
  overrides: {
    consent?: (() => boolean | Promise<boolean>) | undefined;
    maxInMemoryBytes?: number;
    transferId?: string;
  } = {},
): { engine: TransferEngine; records: Map<string, TransferRecord> } {
  const records = new Map<string, TransferRecord>();
  const consentFn = overrides.consent;
  const engine = new TransferEngine({
    channel,
    peerId: 'peer-1',
    peerName: 'Desktop',
    // Bound to a local first so the closure does not need a non-null assertion.
    consent: consentFn ? () => consentFn() : undefined,
    maxInMemoryBytes: overrides.maxInMemoryBytes,
    newTransferId: () => overrides.transferId ?? ID,
    acceptTimeoutMs: 1000,
  });
  engine.onRecord((record) => {
    records.set(record.transferId, record);
  });
  return { engine, records };
}

// ---------------------------------------------------------------------------
// Frame codec
// ---------------------------------------------------------------------------

test('chunk header round trip preserves the transferId and payload', () => {
  const payload = new Uint8Array(CHUNK_SIZE);
  for (let i = 0; i < payload.byteLength; i += 1) payload[i] = i % 251;

  const frame = encodeChunk(ID, payload);
  assertEqual(frame.byteLength, FRAME_HEADER_BYTES + CHUNK_SIZE, 'frame length');

  const header = new Uint8Array(frame, 0, 4);
  assertEqual(String.fromCharCode(...header), 'NSC1', 'magic');

  const decoded = decodeChunk(frame);
  assert(decoded !== null, 'decode returned null');
  assertEqual(decoded.transferId, ID, 'transferId');
  assertBytesEqual(decoded.payload, payload, 'payload');
});

test('chunk header round trip works for an empty payload', () => {
  const frame = encodeChunk(ID, new Uint8Array(0));
  assertEqual(frame.byteLength, FRAME_HEADER_BYTES, 'header-only frame length');
  const decoded = decodeChunk(frame);
  assert(decoded !== null, 'decode returned null');
  assertEqual(decoded.transferId, ID, 'transferId');
  assertEqual(decoded.payload.byteLength, 0, 'payload length');
});

test('decodeChunk rejects a truncated frame and a wrong magic', () => {
  assertEqual(decodeChunk(new ArrayBuffer(FRAME_HEADER_BYTES - 1)), null, 'truncated frame');

  const frame = new Uint8Array(encodeChunk(ID, new Uint8Array([1, 2, 3])));
  frame[0] = 'X'.charCodeAt(0);
  assertEqual(decodeChunk(frame.buffer), null, 'wrong magic');
});

test('encodeChunk refuses a transferId that is not 36 ASCII characters', async () => {
  await assertThrows(() => encodeChunk('too-short', new Uint8Array(1)), 'short id');
  await assertThrows(() => encodeChunk('é'.repeat(36), new Uint8Array(1)), 'non-ascii id');
});

// ---------------------------------------------------------------------------
// Receive limits
// ---------------------------------------------------------------------------

test('refuses an oversized transfer with transfer:cancel reason too_large', async () => {
  const channel = new FakeChannel();
  let consentAsked = false;
  const { engine, records } = engineWith(channel, {
    maxInMemoryBytes: 1024,
    consent: () => {
      consentAsked = true;
      return true;
    },
  });

  channel.receiveString({
    type: 'transfer:start',
    transferId: ID,
    name: 'huge.mp4',
    mime: 'video/mp4',
    size: 5000,
  });
  await tick();

  const cancel = channel.lastFrame();
  assert(cancel !== null && cancel.type === 'transfer:cancel', 'expected a transfer:cancel');
  assertEqual(cancel.reason, 'too_large', 'cancel reason');
  assertEqual(cancel.transferId, ID, 'cancel transferId');
  assertEqual(consentAsked, false, 'the human must not be prompted about a file we cannot hold');

  const record = records.get(ID);
  assert(record !== undefined, 'no record was emitted');
  assertEqual(record.status, 'failed', 'record status');
  assert((record.error ?? '').includes('1.0 KB'), 'the error should state the ceiling');

  // Chunks for a refused transfer must be dropped, never accumulated.
  channel.receiveBinary(encodeChunk(ID, new Uint8Array(64)));
  assertEqual(engine.getDroppedFrameCount(), 1, 'refused chunks must be dropped');
  engine.dispose();
});

test('drops binary frames for an unknown transferId', () => {
  const channel = new FakeChannel();
  const { engine } = engineWith(channel);

  channel.receiveBinary(encodeChunk(OTHER_ID, new Uint8Array(16)));
  channel.receiveBinary(new ArrayBuffer(8)); // Too short to be one of ours.
  assertEqual(engine.getDroppedFrameCount(), 2, 'both frames should be dropped');
  engine.dispose();
});

// ---------------------------------------------------------------------------
// Consent
// ---------------------------------------------------------------------------

test('declining consent answers transfer:accept with accept:false', async () => {
  const channel = new FakeChannel();
  const { engine, records } = engineWith(channel, { consent: () => false });

  channel.receiveString({
    type: 'transfer:start',
    transferId: ID,
    name: 'photo.png',
    mime: 'image/png',
    size: 10,
  });
  await tick(2);

  const accept = channel.frames().find((frame) => frame.type === 'transfer:accept');
  assert(accept !== undefined && accept.type === 'transfer:accept', 'no transfer:accept sent');
  assertEqual(accept.accept, false, 'accept flag');
  assertEqual(records.get(ID)?.status, 'cancelled', 'record status');
  engine.dispose();
});

test('an engine with no consent handler never auto-accepts', async () => {
  const channel = new FakeChannel();
  const { engine } = engineWith(channel);

  channel.receiveString({
    type: 'transfer:start',
    transferId: ID,
    name: 'photo.png',
    mime: 'image/png',
    size: 10,
  });
  await tick(2);

  const accept = channel.frames().find((frame) => frame.type === 'transfer:accept');
  assert(accept !== undefined && accept.type === 'transfer:accept', 'no transfer:accept sent');
  assertEqual(accept.accept, false, 'a missing handler must decline');
  engine.dispose();
});

test('an accepted transfer assembles a Blob and hands back an explicit revoke', async () => {
  const channel = new FakeChannel();
  const { engine, records } = engineWith(channel, { consent: () => true });

  const part1 = new Uint8Array([1, 2, 3, 4]);
  const part2 = new Uint8Array([5, 6]);
  let receivedUrl: string | null = null;
  let revokeTwice = 0;

  engine.onFile((file) => {
    receivedUrl = file.url;
    assertEqual(file.blob.size, 6, 'blob size');
    assertEqual(file.mime, 'application/octet-stream', 'blob mime');
    file.revoke();
    file.revoke(); // Must be idempotent.
    revokeTwice += 1;
  });

  channel.receiveString({
    type: 'transfer:start',
    transferId: ID,
    name: 'bytes.bin',
    mime: 'application/octet-stream',
    size: 6,
  });
  await tick(2);

  channel.receiveBinary(encodeChunk(ID, part1));
  channel.receiveBinary(encodeChunk(ID, part2));
  channel.receiveString({ type: 'transfer:complete', transferId: ID });
  await tick();

  assert(receivedUrl !== null, 'no file was emitted');
  assertEqual(revokeTwice, 1, 'onFile should fire exactly once');
  assertEqual(records.get(ID)?.status, 'complete', 'record status');
  assertEqual(records.get(ID)?.bytes, 6, 'record bytes');
  engine.dispose();
});

// ---------------------------------------------------------------------------
// Backpressure
// ---------------------------------------------------------------------------

test('the send loop parks above the high-water mark until bufferedamountlow', async () => {
  const channel = new FakeChannel();
  channel.stallAfterSend = true;
  const { engine } = engineWith(channel, { transferId: ID });

  // 70000 bytes: one full 64 KiB chunk plus a 4464-byte tail, so the loop has
  // to park at least once between the two.
  const file = new File([new Uint8Array(70000)], 'big.bin', {
    type: 'application/octet-stream',
  });

  const settled = engine.sendFile(file);
  await tick();

  const start = channel.frames().find((frame) => frame.type === 'transfer:start');
  assert(start !== undefined, 'transfer:start was not sent');

  channel.receiveString({ type: 'transfer:accept', transferId: ID, accept: true });
  await tick(3);

  assertEqual(channel.sentBinary.length, 1, 'the second chunk must wait for drain');

  channel.drain();
  await tick(3);

  assertEqual(channel.sentBinary.length, 2, 'the second chunk should go out after drain');
  const record = await settled;
  assertEqual(record.status, 'complete', 'final status');
  assertEqual(record.bytes, 70000, 'bytes sent');

  const complete = channel.frames().find((frame) => frame.type === 'transfer:complete');
  assert(complete !== undefined, 'transfer:complete was not sent');
  assertEqual(channel.bufferedAmountLowThreshold, 262144, 'threshold must be armed');
  engine.dispose();
});

test('a cancel wakes a send loop parked on backpressure', async () => {
  const channel = new FakeChannel();
  channel.stallAfterSend = true;
  const { engine } = engineWith(channel, { transferId: ID });

  const file = new File([new Uint8Array(70000)], 'big.bin');
  const settled = engine.sendFile(file);
  await tick();
  channel.receiveString({ type: 'transfer:accept', transferId: ID, accept: true });
  await tick(3);

  engine.cancel(ID, 'user_cancelled');
  const record = await settled;
  assertEqual(record.status, 'cancelled', 'a parked loop must unwind on cancel');
  engine.dispose();
});

// ---------------------------------------------------------------------------
// Text
// ---------------------------------------------------------------------------

test('short text goes inline and creates no record', async () => {
  const channel = new FakeChannel();
  const { engine } = engineWith(channel);

  const record = await engine.sendText('Hello from Aashish');
  assertEqual(record, null, 'inline text should not create a record');

  const frame = channel.lastFrame();
  assert(frame !== null && frame.type === 'text', 'expected a text frame');
  assertEqual(frame.text, 'Hello from Aashish', 'text payload');
  engine.dispose();
});

test('text at or above MAX_INLINE_TEXT takes the file path', async () => {
  const channel = new FakeChannel();
  const { engine } = engineWith(channel, { transferId: ID });

  const big = 'x'.repeat(MAX_INLINE_TEXT + 1);
  const settled = engine.sendText(big);
  await tick();

  const start = channel.frames().find((frame) => frame.type === 'transfer:start');
  assert(start !== undefined && start.type === 'transfer:start', 'no transfer:start sent');
  assertEqual(start.name, 'shared-text.txt', 'file name');
  assertEqual(start.size, MAX_INLINE_TEXT + 1, 'declared size');

  engine.cancel(ID, 'user_cancelled');
  const record = await settled;
  assert(record !== null, 'the file path must return a record');
  engine.dispose();
});
