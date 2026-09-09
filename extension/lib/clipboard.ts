/**
 * Clipboard helpers for the side panel.
 *
 * Sending text is NOT here any more. In protocol v2 text travels on the peer
 * DataChannel like everything else (`{"type":"text"}`, spec §6) — the daemon
 * relays signalling and never sees a payload — so `TransferEngine.sendText()`
 * owns that path and this module is left with the two things that genuinely
 * belong to the clipboard.
 *
 * TODO(M14): a contextMenus handler for "send selection to…". If page access is
 * ever needed, request `activeTab` and inject on click — do NOT add an
 * `<all_urls>` content script, which triggers the "read all your data on all
 * websites" install warning.
 */

import { MAX_INLINE_TEXT } from '@/types';

/**
 * Payloads at or under this are sent as one inline `text` frame; anything
 * larger becomes a file transfer, with consent and backpressure.
 */
export const MAX_INLINE_TEXT_BYTES = MAX_INLINE_TEXT;

/**
 * Reads the current clipboard text.
 *
 * Returns null when the document is not focused or permission is denied —
 * `navigator.clipboard.readText()` rejects in both cases, and neither is worth
 * surfacing as an error.
 */
export async function readClipboardText(): Promise<string | null> {
  try {
    if (typeof navigator === 'undefined' || !navigator.clipboard) return null;
    const text = await navigator.clipboard.readText();
    return text.length > 0 ? text : null;
  } catch {
    return null;
  }
}

/** Writes text to the clipboard. Returns false when the write was refused. */
export async function writeClipboardText(text: string): Promise<boolean> {
  try {
    if (typeof navigator === 'undefined' || !navigator.clipboard) return false;
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    return false;
  }
}

/** True when the payload is small enough to travel as one inline frame. */
export function fitsInline(text: string): boolean {
  return new TextEncoder().encode(text).byteLength < MAX_INLINE_TEXT_BYTES;
}
