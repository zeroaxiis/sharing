import { formatBytes, progressFraction, type TransferRecord } from '@/lib/transfer';

export interface TransferProgressProps {
  transfer: TransferRecord;
  onCancel?: (transferId: string) => void;
}

const STATUS_LABEL: Record<TransferRecord['status'], string> = {
  pending: 'Waiting',
  active: 'Sending',
  complete: 'Done',
  cancelled: 'Cancelled',
  failed: 'Failed',
};

/** A single in-flight transfer: name, bar, byte counter, cancel. */
export function TransferProgress({ transfer, onCancel }: TransferProgressProps) {
  const fraction = progressFraction(transfer.bytes, transfer.size);
  const percent = Math.round(fraction * 100);
  const running = transfer.status === 'pending' || transfer.status === 'active';
  const label = transfer.direction === 'send' ? 'To' : 'From';

  return (
    <div className="progress">
      <div className="progress__head">
        <span className="progress__name" title={transfer.name}>
          {transfer.name}
        </span>
        <span className="progress__percent">{percent}%</span>
      </div>

      <div
        className="progress__track"
        role="progressbar"
        aria-valuenow={percent}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-label={'Transfer of ' + transfer.name}
      >
        <div className="progress__fill" style={{ width: percent + '%' }} />
      </div>

      <div className="progress__foot">
        <span className="progress__meta">
          {label} {transfer.peerName} · {formatBytes(transfer.bytes)} of{' '}
          {formatBytes(transfer.size)}
        </span>
        {running ? (
          <button
            type="button"
            className="progress__cancel"
            onClick={() => onCancel?.(transfer.transferId)}
          >
            Cancel
          </button>
        ) : (
          <span className="progress__state">{STATUS_LABEL[transfer.status]}</span>
        )}
      </div>
    </div>
  );
}

export default TransferProgress;
