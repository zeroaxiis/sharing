import { useCallback, useRef, useState } from 'react';
import type { ChangeEvent, DragEvent } from 'react';

export interface FileDropProps {
  /** Receives the dropped or picked files. Not wired to a transfer until M9. */
  onFiles?: (files: File[]) => void;
  /** Disabled while no device is selected or no daemon is reachable. */
  disabled?: boolean;
  hint?: string;
}

/**
 * Drop zone for outgoing files. Fully interactive; the files it hands back are
 * not sent anywhere yet — `lib/transfer.ts` picks that up in M9.
 */
export function FileDrop({ onFiles, disabled = false, hint = 'Drop a file to send' }: FileDropProps) {
  const [over, setOver] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);

  const emit = useCallback(
    (list: FileList | null) => {
      if (!list || list.length === 0) return;
      onFiles?.(Array.from(list));
    },
    [onFiles],
  );

  const handleDrop = useCallback(
    (event: DragEvent<HTMLDivElement>) => {
      event.preventDefault();
      setOver(false);
      if (disabled) return;
      emit(event.dataTransfer.files);
    },
    [disabled, emit],
  );

  const handleDragOver = useCallback(
    (event: DragEvent<HTMLDivElement>) => {
      event.preventDefault();
      if (!disabled) setOver(true);
    },
    [disabled],
  );

  const handleChange = useCallback(
    (event: ChangeEvent<HTMLInputElement>) => {
      emit(event.target.files);
      event.target.value = '';
    },
    [emit],
  );

  const className =
    'drop' + (over ? ' drop--over' : '') + (disabled ? ' drop--disabled' : '');

  return (
    <div
      className={className}
      onDragOver={handleDragOver}
      onDragLeave={() => setOver(false)}
      onDrop={handleDrop}
    >
      <p className="drop__hint">{hint}</p>
      <button
        type="button"
        className="drop__button"
        disabled={disabled}
        onClick={() => inputRef.current?.click()}
      >
        Choose file
      </button>
      <input
        ref={inputRef}
        type="file"
        multiple
        className="drop__input"
        onChange={handleChange}
        tabIndex={-1}
        aria-hidden="true"
      />
    </div>
  );
}

export default FileDrop;
