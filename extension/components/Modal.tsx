import { useCallback, useEffect, useId, useRef } from 'react';
import type { KeyboardEvent, ReactNode } from 'react';

export interface ModalProps {
  /** Rendered as the dialog's accessible name. */
  title: string;
  /**
   * Escape, and a click on the backdrop.
   *
   * Both dialogs in this panel answer a question the other end is waiting on,
   * so dismissal is never silent: the caller wires this to the safe answer
   * (reject the pairing, decline the file), never to a bare close.
   */
  onDismiss: () => void;
  children: ReactNode;
  /** The action row, pinned under the body. */
  actions?: ReactNode;
}

/** Focusable descendants, in DOM order, skipping anything currently disabled. */
function focusable(root: HTMLElement): HTMLElement[] {
  const nodes = root.querySelectorAll<HTMLElement>(
    'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
  );
  return [...nodes].filter((node) => node.offsetParent !== null || node === document.activeElement);
}

/**
 * A modal dialog with a real focus trap.
 *
 * The side panel is a document with nothing behind the dialog worth reaching,
 * so focus is moved in on open, cycled inside on Tab, and returned to whatever
 * had it when the dialog closes. Without the restore, dismissing a pairing
 * prompt drops focus onto <body> and a keyboard user has to tab from the top of
 * the panel again.
 */
export function Modal({ title, onDismiss, children, actions }: ModalProps) {
  const titleId = useId();
  const dialogRef = useRef<HTMLDivElement>(null);
  const restoreRef = useRef<Element | null>(null);

  useEffect(() => {
    restoreRef.current = document.activeElement;
    const dialog = dialogRef.current;
    // Focus the dialog itself rather than its first control: assistive tech
    // then announces the title before the buttons, and no destructive action
    // is ever the thing that happens to be focused.
    dialog?.focus();

    return () => {
      const previous = restoreRef.current;
      if (previous instanceof HTMLElement && document.contains(previous)) previous.focus();
    };
  }, []);

  const onKeyDown = useCallback(
    (event: KeyboardEvent<HTMLDivElement>) => {
      if (event.key === 'Escape') {
        event.stopPropagation();
        onDismiss();
        return;
      }
      if (event.key !== 'Tab') return;

      const dialog = dialogRef.current;
      if (!dialog) return;
      const stops = focusable(dialog);
      if (stops.length === 0) {
        event.preventDefault();
        dialog.focus();
        return;
      }
      const first = stops[0];
      const last = stops[stops.length - 1];
      if (!first || !last) return;

      const active = document.activeElement;
      if (event.shiftKey && (active === first || active === dialog)) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && active === last) {
        event.preventDefault();
        first.focus();
      }
    },
    [onDismiss],
  );

  return (
    <div
      className="scrim"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) onDismiss();
      }}
    >
      <div
        className="modal"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        ref={dialogRef}
        onKeyDown={onKeyDown}
      >
        <h2 className="modal__title" id={titleId}>
          {title}
        </h2>
        <div className="modal__body">{children}</div>
        {actions ? <div className="modal__actions">{actions}</div> : null}
      </div>
    </div>
  );
}

export default Modal;
