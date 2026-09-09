import { Modal } from '@/components/Modal';
import type { PairingView } from '@/lib/session';

export interface PairingDialogProps {
  pairing: PairingView;
  /** Confirm or reject the code. Never called on the user's behalf. */
  onAnswer: (accept: boolean) => void;
  /** Close a dialog that is no longer asking anything (done / failed). */
  onClose: () => void;
}

/** Six digits, spaced so they can be read aloud without losing your place. */
function CodeDigits({ code }: { code: string }) {
  const digits = [...code];
  return (
    <p className="code" aria-label={'Verification code: ' + digits.join(' ')}>
      {digits.map((digit, index) => (
        // Fixed-length numeric code with no reordering, so the index is a
        // stable key here.
        <span className="code__digit" key={index} aria-hidden="true">
          {digit}
        </span>
      ))}
    </p>
  );
}

/**
 * The pairing prompt.
 *
 * THE CODE COMPARISON IS THE ENTIRE SECURITY PROPERTY. Neither daemon ever
 * transmits these digits: both derive them from a nonce and the two device ids,
 * so if the two screens agree, the machine on the other end is the one holding
 * that nonce and not something in the middle. If they disagree, confirming
 * hands a stranger a long-lived token. That is why this dialog states the
 * comparison in plain words above the buttons, why the buttons are symmetrical
 * rather than one-tap-to-accept, and why nothing here confirms automatically or
 * on a timer.
 */
export function PairingDialog({ pairing, onAnswer, onClose }: PairingDialogProps) {
  const { phase, name, code, direction, message } = pairing;
  const answering = phase === 'code';

  // Escape and the backdrop mean "I am not answering this", and the safe
  // reading of that is always a rejection while the other end is still waiting.
  const dismiss = () => {
    if (phase === 'done' || phase === 'failed') onClose();
    else onAnswer(false);
  };

  const title =
    phase === 'done'
      ? 'Paired'
      : phase === 'failed'
        ? 'Not paired'
        : direction === 'incoming'
          ? name + ' wants to pair'
          : 'Pair with ' + name;

  return (
    <Modal
      title={title}
      onDismiss={dismiss}
      actions={
        answering ? (
          <>
            <button type="button" className="button" onClick={() => onAnswer(false)}>
              Reject
            </button>
            <button type="button" className="button button--primary" onClick={() => onAnswer(true)}>
              The codes match
            </button>
          </>
        ) : phase === 'done' || phase === 'failed' ? (
          <button type="button" className="button button--primary" onClick={onClose}>
            Done
          </button>
        ) : (
          <button type="button" className="button" onClick={() => onAnswer(false)}>
            Cancel
          </button>
        )
      }
    >
      {phase === 'starting' ? (
        <p className="modal__lede" role="status">
          Asking {name} to pair. It will show the same six digits.
        </p>
      ) : null}

      {answering || phase === 'submitting' ? (
        <>
          {code ? <CodeDigits code={code} /> : null}
          <p className="modal__lede">
            <strong>{name}</strong> must be showing these exact six digits right now.
          </p>
          <p className="modal__fine">
            If the digits differ, or nothing appears on {name}, reject this. Confirming a code you
            have not compared gives that device permanent access to send you files.
          </p>
        </>
      ) : null}

      {phase === 'submitting' ? (
        <p className="modal__lede" role="status">
          Waiting for {name} to confirm too…
        </p>
      ) : null}

      {phase === 'done' ? (
        <p className="modal__lede" role="status">
          {name} is paired. You can send to it from the device list.
        </p>
      ) : null}

      {phase === 'failed' ? (
        <p className="modal__lede modal__lede--bad" role="status">
          {message ?? 'Pairing did not complete.'}
        </p>
      ) : null}
    </Modal>
  );
}

export default PairingDialog;
