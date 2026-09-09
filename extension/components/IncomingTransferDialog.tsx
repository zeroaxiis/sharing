import { Modal } from '@/components/Modal';
import { formatBytes } from '@/lib/transfer';
import type { IncomingTransferOffer } from '@/lib/transfer';

export interface IncomingTransferDialogProps {
  offer: IncomingTransferOffer;
  /** Accept or decline. There is no third answer and no default. */
  onAnswer: (accept: boolean) => void;
}

/**
 * Consent for one incoming file.
 *
 * Nothing is written to memory before this resolves — the sender is parked on
 * `transfer:accept` — so declining costs the receiver nothing at all. The
 * filename is rendered as plain text and never as a link or a preview: it is
 * attacker-controlled, and the only safe thing to do with it is show it.
 */
export function IncomingTransferDialog({ offer, onAnswer }: IncomingTransferDialogProps) {
  return (
    <Modal
      title="Incoming file"
      onDismiss={() => onAnswer(false)}
      actions={
        <>
          <button type="button" className="button" onClick={() => onAnswer(false)}>
            Decline
          </button>
          <button type="button" className="button button--primary" onClick={() => onAnswer(true)}>
            Accept
          </button>
        </>
      }
    >
      <p className="modal__lede">
        <strong>{offer.peerName}</strong> wants to send you a file.
      </p>

      <dl className="facts">
        <div className="facts__row">
          <dt className="facts__key">Name</dt>
          <dd className="facts__value" title={offer.name}>
            {offer.name}
          </dd>
        </div>
        <div className="facts__row">
          <dt className="facts__key">Size</dt>
          <dd className="facts__value">{formatBytes(offer.size)}</dd>
        </div>
        <div className="facts__row">
          <dt className="facts__key">Type</dt>
          <dd className="facts__value">{offer.mime || 'Unknown'}</dd>
        </div>
      </dl>

      <p className="modal__fine">
        Nothing is downloaded until you accept, and the file is kept in this panel until you save
        it.
      </p>
    </Modal>
  );
}

export default IncomingTransferDialog;
