import { useCallback, useId, useState } from 'react';
import type { ChangeEvent, KeyboardEvent } from 'react';
import { MAX_INLINE_TEXT } from '@/types';

export interface TextComposerProps {
  onSend: (text: string) => void;
  disabled?: boolean;
  /** Named so the button says where the text is going. */
  peerName?: string;
}

const encoder = new TextEncoder();

/**
 * A small composer for sending text or a link.
 *
 * Text under MAX_INLINE_TEXT travels as a single `text` frame and appears on
 * the other side without a consent prompt (spec v2 §9); anything larger is
 * turned into a .txt file by the transfer engine and goes through the normal
 * accept flow. The hint below says so rather than letting the behaviour change
 * silently at 64 KiB.
 */
export function TextComposer({ onSend, disabled = false, peerName }: TextComposerProps) {
  const [text, setText] = useState('');
  const fieldId = useId();

  const bytes = encoder.encode(text).byteLength;
  const asFile = bytes >= MAX_INLINE_TEXT;
  const empty = text.trim().length === 0;

  const submit = useCallback(() => {
    if (disabled || empty) return;
    onSend(text);
    setText('');
  }, [disabled, empty, onSend, text]);

  const onKeyDown = useCallback(
    (event: KeyboardEvent<HTMLTextAreaElement>) => {
      // Enter sends, Shift+Enter is a newline — the convention every messaging
      // surface uses, so muscle memory does the right thing.
      if (event.key === 'Enter' && !event.shiftKey) {
        event.preventDefault();
        submit();
      }
    },
    [submit],
  );

  return (
    <div className="composer">
      <label className="composer__label" htmlFor={fieldId}>
        Text or a link
      </label>
      <textarea
        id={fieldId}
        className="composer__input"
        value={text}
        rows={2}
        disabled={disabled}
        placeholder={disabled ? 'Connect to a device first' : 'Paste a link, type a note…'}
        onChange={(event: ChangeEvent<HTMLTextAreaElement>) => setText(event.target.value)}
        onKeyDown={onKeyDown}
      />
      <div className="composer__foot">
        <span className="composer__hint">
          {asFile ? 'Over 64 KB — this will be sent as a file.' : 'Enter to send'}
        </span>
        <button
          type="button"
          className="chipbutton chipbutton--accent"
          disabled={disabled || empty}
          onClick={submit}
        >
          {peerName ? 'Send text' : 'Send'}
        </button>
      </div>
    </div>
  );
}

export default TextComposer;
