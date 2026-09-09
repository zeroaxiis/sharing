import { useCallback, useState } from 'react';
import { writeClipboardText } from '@/lib/clipboard';
import type { ReceivedItem } from '@/lib/session';
import { formatBytes } from '@/lib/transfer';

export interface ReceivedListProps {
  items: readonly ReceivedItem[];
  /** Removes the item and, for a file, revokes its object URL. */
  onDismiss: (id: string) => void;
}

function CopyButton({ text }: { text: string }) {
  const [state, setState] = useState<'idle' | 'ok' | 'fail'>('idle');

  const copy = useCallback(() => {
    void writeClipboardText(text).then((ok) => {
      setState(ok ? 'ok' : 'fail');
      window.setTimeout(() => setState('idle'), 2000);
    });
  }, [text]);

  return (
    <button type="button" className="chipbutton" onClick={copy}>
      {state === 'ok' ? 'Copied' : state === 'fail' ? 'Copy blocked' : 'Copy'}
    </button>
  );
}

/**
 * Payloads that have arrived and are waiting to be dealt with.
 *
 * A file lives here as a Blob behind an object URL until it is saved or
 * dismissed. The download is an ordinary anchor rather than a scripted save:
 * the panel is a real document, so the browser's own download UI handles it,
 * and no `downloads` permission is needed.
 *
 * TODO(M13): with streaming-to-disk the file would land in the downloads
 * directory as it arrives and this list would hold only a receipt.
 */
export function ReceivedList({ items, onDismiss }: ReceivedListProps) {
  if (items.length === 0) return null;

  return (
    <ul className="received">
      {items.map((item) => (
        <li className="received__row" key={item.id}>
          {item.kind === 'file' ? (
            <>
              <span className="received__text">
                <span className="received__name" title={item.name}>
                  {item.name}
                </span>
                <span className="received__meta">
                  From {item.from} · {formatBytes(item.size)}
                </span>
              </span>
              <span className="received__actions">
                <a className="chipbutton chipbutton--accent" href={item.url} download={item.name}>
                  Save
                </a>
                <button
                  type="button"
                  className="linkbutton linkbutton--quiet"
                  onClick={() => onDismiss(item.id)}
                >
                  Dismiss
                </button>
              </span>
            </>
          ) : (
            <>
              <span className="received__text">
                <span className="received__body">{item.text}</span>
                <span className="received__meta">From {item.from}</span>
              </span>
              <span className="received__actions">
                <CopyButton text={item.text} />
                <button
                  type="button"
                  className="linkbutton linkbutton--quiet"
                  onClick={() => onDismiss(item.id)}
                >
                  Dismiss
                </button>
              </span>
            </>
          )}
        </li>
      ))}
    </ul>
  );
}

export default ReceivedList;
