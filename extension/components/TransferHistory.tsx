import { formatBytes, type TransferRecord } from '@/lib/transfer';

export interface TransferHistoryProps {
  transfers: readonly TransferRecord[];
  /** Cap on rows rendered; the popup is small. */
  limit?: number;
}

const STATUS_CLASS: Record<TransferRecord['status'], string> = {
  pending: 'history__badge--pending',
  active: 'history__badge--pending',
  complete: 'history__badge--ok',
  cancelled: 'history__badge--muted',
  failed: 'history__badge--bad',
};

function formatWhen(timestamp: number | null): string {
  if (!timestamp) return '';
  return new Date(timestamp).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
}

/** Recently finished transfers. Fed real records in M9. */
export function TransferHistory({ transfers, limit = 5 }: TransferHistoryProps) {
  if (transfers.length === 0) {
    return <p className="history__empty">No recent transfers</p>;
  }

  return (
    <ul className="history">
      {transfers.slice(0, limit).map((transfer) => (
        <li key={transfer.transferId} className="history__row">
          <span className={'history__badge ' + STATUS_CLASS[transfer.status]} aria-hidden="true" />
          <span className="history__text">
            <span className="history__name" title={transfer.name}>
              {transfer.name}
            </span>
            <span className="history__meta">
              {transfer.direction === 'send' ? 'Sent to' : 'Received from'} {transfer.peerName} ·{' '}
              {formatBytes(transfer.size)}
            </span>
          </span>
          <span className="history__when">{formatWhen(transfer.finishedAt ?? transfer.startedAt)}</span>
        </li>
      ))}
    </ul>
  );
}

export default TransferHistory;
