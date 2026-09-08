/**
 * File / text transfer orchestration.
 *
 * Payloads are split into CHUNK_SIZE (64 KiB) frames and pushed over the
 * WebRTC DataChannel from `lib/webrtc.ts`; the daemon socket carries only the
 * control messages (`transfer:start`, `transfer:progress`, `transfer:complete`,
 * `transfer:cancel`).
 *
 * TODO(M9): implement chunked send/receive, progress accounting, and cancel.
 */

import { CHUNK_SIZE, type TransferCancelReason } from '@/types';

export type TransferDirection = 'send' | 'receive';

export type TransferStatus = 'pending' | 'active' | 'complete' | 'cancelled' | 'failed';

/** One transfer as the UI sees it. */
export interface TransferRecord {
  transferId: string;
  direction: TransferDirection;
  /** Device id of the other end. */
  peerId: string;
  peerName: string;
  name: string;
  mime: string;
  size: number;
  bytes: number;
  status: TransferStatus;
  startedAt: number;
  finishedAt: number | null;
  error: string | null;
}

export interface StartTransferOptions {
  peerId: string;
  peerName: string;
  file: File;
}

/** Number of 64 KiB chunks a payload of `size` bytes splits into. */
export function chunkCount(size: number): number {
  if (size <= 0) return 0;
  return Math.ceil(size / CHUNK_SIZE);
}

/** 0-1 progress fraction, clamped; 0 when the total is unknown. */
export function progressFraction(bytes: number, total: number): number {
  if (!Number.isFinite(total) || total <= 0) return 0;
  return Math.min(1, Math.max(0, bytes / total));
}

/** Human-readable byte count, e.g. "2.3 MB". */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '—';
  if (bytes < 1024) return bytes + ' B';
  const units = ['KB', 'MB', 'GB', 'TB'];
  let value = bytes / 1024;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return value.toFixed(value < 10 ? 1 : 0) + ' ' + (units[unit] ?? 'TB');
}

/**
 * Begins sending a file to a peer.
 *
 * TODO(M9): implement. Returns the record the UI will track.
 */
export function startTransfer(options: StartTransferOptions): TransferRecord {
  const { peerId, peerName, file } = options;
  console.warn('[nearby-share] startTransfer is a stub (M9):', file.name);
  return {
    transferId: 'stub',
    direction: 'send',
    peerId,
    peerName,
    name: file.name,
    mime: file.type || 'application/octet-stream',
    size: file.size,
    bytes: 0,
    status: 'pending',
    startedAt: Date.now(),
    finishedAt: null,
    error: null,
  };
}

/**
 * Cancels an in-flight transfer.
 *
 * TODO(M9): implement — send `transfer:cancel` and release the reader.
 */
export function cancelTransfer(transferId: string, reason: TransferCancelReason): void {
  console.warn('[nearby-share] cancelTransfer is a stub (M9):', transferId, reason);
}
