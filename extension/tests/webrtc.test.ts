import { assert, assertEqual, test, tick } from './harness.ts';
import { FakePeerConnection, asPeerConnection } from './fakes.ts';
import { PeerLink } from '@/lib/webrtc';
import type { TransferChannel } from '@/lib/transfer';
import type { SignalOutboundMessage } from '@/types';

/**
 * Compile-time proof that PeerLink still satisfies the transfer engine's
 * channel contract. The two modules are wired together only structurally, so
 * without this a rename on either side would fail at runtime in the panel
 * rather than in `tsc --noEmit`. Exported so it is not an unused local.
 */
type AssertChannel<T extends TransferChannel> = T;
export type PeerLinkIsATransferChannel = AssertChannel<PeerLink>;

interface Rig {
  link: PeerLink;
  pc: FakePeerConnection;
  signals: SignalOutboundMessage[];
}

function rig(localId: string, remoteId: string): Rig {
  const pc = new FakePeerConnection();
  const signals: SignalOutboundMessage[] = [];
  const link = new PeerLink({
    localId,
    remoteId,
    remoteName: 'Desktop',
    sendSignal: (message) => {
      signals.push(message);
    },
    newId: () => 'test-id',
    createConnection: () => asPeerConnection(pc),
  });
  return { link, pc, signals };
}

// ---------------------------------------------------------------------------
// ICE buffering — the race that silently kills connections
// ---------------------------------------------------------------------------

test('remote ICE candidates arriving before the remote description are buffered', async () => {
  const { link, pc } = rig('aaa', 'zzz');

  await link.handleSignal({
    type: 'signal:ice',
    from: 'zzz',
    candidate: 'candidate:1 1 udp 2122 192.168.1.31 51000 typ host',
    sdpMid: '0',
    sdpMLineIndex: 0,
  });
  await link.handleSignal({
    type: 'signal:ice',
    from: 'zzz',
    candidate: 'candidate:2 1 udp 2122 192.168.1.31 51001 typ host',
    sdpMid: '0',
    sdpMLineIndex: 0,
  });

  assertEqual(link.pendingRemoteCandidateCount, 2, 'both candidates should be parked');
  assertEqual(pc.addedCandidates.length, 0, 'nothing may reach addIceCandidate yet');

  link.close();
});

test('buffered candidates flush once setRemoteDescription lands, in order', async () => {
  const { link, pc, signals } = rig('aaa', 'zzz');

  await link.handleSignal({
    type: 'signal:ice',
    from: 'zzz',
    candidate: 'candidate:1 first',
    sdpMid: '0',
    sdpMLineIndex: 0,
  });
  await link.handleSignal({
    type: 'signal:ice',
    from: 'zzz',
    candidate: 'candidate:2 second',
    sdpMid: '0',
    sdpMLineIndex: 0,
  });

  await link.handleSignal({ type: 'signal:offer', from: 'zzz', sdp: 'REMOTE-OFFER' });
  await tick();

  assertEqual(link.pendingRemoteCandidateCount, 0, 'the buffer should be drained');
  assertEqual(pc.addedCandidates.length, 2, 'both candidates should be applied');
  assertEqual(pc.addedCandidates[0]?.candidate, 'candidate:1 first', 'order preserved');
  assertEqual(pc.addedCandidates[1]?.candidate, 'candidate:2 second', 'order preserved');

  const answer = signals.find((signal) => signal.type === 'signal:answer');
  assert(answer !== undefined, 'an answer should have been sent');

  // After the remote description exists, later candidates go straight through.
  await link.handleSignal({
    type: 'signal:ice',
    from: 'zzz',
    candidate: 'candidate:3 late',
    sdpMid: '0',
    sdpMLineIndex: 0,
  });
  assertEqual(pc.addedCandidates.length, 3, 'a late candidate is applied immediately');
  assertEqual(link.pendingRemoteCandidateCount, 0, 'nothing should be buffered after flush');

  link.close();
});

test('an empty sdpMid is normalised to null before addIceCandidate', async () => {
  const { link, pc } = rig('aaa', 'zzz');
  await link.handleSignal({ type: 'signal:offer', from: 'zzz', sdp: 'REMOTE-OFFER' });
  await link.handleSignal({
    type: 'signal:ice',
    from: 'zzz',
    candidate: 'candidate:1 host',
    sdpMid: '',
    sdpMLineIndex: 0,
  });

  assertEqual(pc.addedCandidates[0]?.sdpMid, null, 'an empty sdpMid must become null');
  link.close();
});

test('a signal addressed from another device is ignored', async () => {
  const { link, pc } = rig('aaa', 'zzz');
  await link.handleSignal({ type: 'signal:offer', from: 'someone-else', sdp: 'X' });
  assertEqual(pc.calls.length, 0, 'a mis-routed signal must not touch the connection');
  link.close();
});

// ---------------------------------------------------------------------------
// Negotiation order and glare
// ---------------------------------------------------------------------------

test('the initiator creates the DataChannel before createOffer', async () => {
  const { link, pc, signals } = rig('aaa', 'zzz');
  await link.connect();

  const channelIndex = pc.calls.indexOf('createDataChannel');
  const offerIndex = pc.calls.indexOf('createOffer');
  assert(channelIndex >= 0, 'no DataChannel was created');
  assert(offerIndex >= 0, 'no offer was created');
  assert(channelIndex < offerIndex, 'the channel must exist before the offer is built');

  assertEqual(pc.channels[0]?.label, 'sharing', 'channel label');
  const offer = signals.find((signal) => signal.type === 'signal:offer');
  assert(offer !== undefined && offer.type === 'signal:offer', 'no offer was signalled');
  assertEqual(offer.to, 'zzz', 'an outbound signal must carry `to`');
  assertEqual(offer.sdp, 'FAKE-OFFER', 'offer sdp');
  link.close();
});

test('glare: the lexicographically smaller deviceId wins and does not roll back', async () => {
  // localId 'aaa' < remoteId 'zzz', so we win and ignore their offer.
  const { link, pc, signals } = rig('aaa', 'zzz');
  await link.connect();
  const before = signals.length;

  await link.handleSignal({ type: 'signal:offer', from: 'zzz', sdp: 'THEIR-OFFER' });

  assertEqual(pc.rollbacks, 0, 'the winner must not roll back');
  assertEqual(signals.length, before, 'the winner must not answer a colliding offer');
  link.close();
});

test('glare: the larger deviceId rolls back and answers', async () => {
  // localId 'zzz' > remoteId 'aaa', so we are polite and give way.
  const { link, pc, signals } = rig('zzz', 'aaa');
  await link.connect();

  await link.handleSignal({ type: 'signal:offer', from: 'aaa', sdp: 'THEIR-OFFER' });
  await tick();

  assertEqual(pc.rollbacks, 1, 'the loser must roll back its own offer');
  const answer = signals.find((signal) => signal.type === 'signal:answer');
  assert(answer !== undefined && answer.type === 'signal:answer', 'the loser must answer');
  assertEqual(answer.to, 'aaa', 'the answer must be addressed');
  link.close();
});

// ---------------------------------------------------------------------------
// Local candidates, failure reporting, teardown
// ---------------------------------------------------------------------------

test('local candidates are trickled out as they arrive, end-of-candidates is not', async () => {
  const { link, pc, signals } = rig('aaa', 'zzz');
  await link.connect();

  pc.emitLocalCandidate('candidate:local 1');
  // A null candidate is the end-of-candidates marker and must not be relayed.
  pc.onicecandidate?.({ candidate: null } as unknown as RTCPeerConnectionIceEvent);

  const ice = signals.filter((signal) => signal.type === 'signal:ice');
  assertEqual(ice.length, 1, 'exactly one candidate should have been relayed');
  const first = ice[0];
  assert(first !== undefined && first.type === 'signal:ice', 'wrong signal type');
  assertEqual(first.candidate, 'candidate:local 1', 'candidate line');
  assertEqual(first.sdpMid, '0', 'sdpMid');
  link.close();
});

test('an ICE failure is reported loudly, with a message a person can act on', async () => {
  const { link, pc } = rig('aaa', 'zzz');
  const errors: string[] = [];
  let fatalSeen = false;
  link.onError((error) => {
    errors.push(error.code);
    if (error.fatal) fatalSeen = true;
    assert(error.message.length > 0, 'every error must carry a displayable message');
  });

  await link.connect();
  pc.setIceConnectionState('failed');

  assert(errors.includes('ice_failed'), 'an ICE failure must surface');
  assert(fatalSeen, 'an ICE failure is fatal');
  link.close();
});

test('ICE disconnected is surfaced but not fatal', async () => {
  const { link, pc } = rig('aaa', 'zzz');
  let fatal = true;
  link.onError((error) => {
    if (error.code === 'ice_disconnected') fatal = error.fatal;
  });

  await link.connect();
  pc.setIceConnectionState('disconnected');
  assertEqual(fatal, false, 'a disconnect may still recover');
  link.close();
});

test('connected requires both the transport and the channel', async () => {
  const { link, pc } = rig('aaa', 'zzz');
  await link.connect();

  pc.setConnectionState('connected');
  assertEqual(link.getState(), 'connecting', 'a closed channel is not yet usable');
  assertEqual(link.isOpen, false, 'isOpen must follow the channel');

  const channel = pc.channels[0];
  assert(channel !== undefined, 'no channel was created');
  channel.open();

  assertEqual(link.getState(), 'connected', 'both halves are ready');
  assertEqual(link.isOpen, true, 'isOpen should now be true');
  link.close();
});

test('close() is idempotent and emits exactly one close event', async () => {
  const { link, pc } = rig('aaa', 'zzz');
  let closes = 0;
  link.onClose(() => {
    closes += 1;
  });

  await link.connect();
  link.close();
  link.close();
  link.close();

  assertEqual(closes, 1, 'close must fire once');
  assertEqual(pc.closed, true, 'the peer connection must be closed');
  assertEqual(link.getState(), 'closed', 'state');
  assertEqual(link.isOpen, false, 'a closed link is never open');
});
