/**
 * Clipboard and text sharing.
 *
 * The daemon advertises the "text" capability in DaemonInfo.capabilities, so
 * short payloads (selected text, a link, a snippet) go over the control socket
 * rather than opening a DataChannel.
 *
 * TODO(M10): send/receive clipboard text through the daemon.
 * TODO(M14): hook this up to the background contextMenus handler. If page access
 * is ever needed, request `activeTab` and inject on click - do not add an
 * `<all_urls>` content script, which triggers the "read all your data on all
 * websites" install warning.
 */

/** Payloads above this are sent as a file transfer instead of inline text. */
export const MAX_INLINE_TEXT_BYTES = 64 * 1024;

export interface TextPayload {
  /** Device id of the destination. */
  peerId: string;
  text: string;
  /** Where the text came from, for the receiver's notification. */
  source: 'selection' | 'clipboard' | 'link' | 'manual';
}

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

/** True when the payload is small enough to travel inline over the socket. */
export function fitsInline(text: string): boolean {
  return new TextEncoder().encode(text).byteLength <= MAX_INLINE_TEXT_BYTES;
}

/**
 * Sends text to a paired device.
 *
 * TODO(M10): implement on top of the daemon socket.
 */
export async function sendText(payload: TextPayload): Promise<boolean> {
  console.warn('[nearby-share] sendText is a stub (M10):', payload.peerId, payload.source);
  return false;
}
