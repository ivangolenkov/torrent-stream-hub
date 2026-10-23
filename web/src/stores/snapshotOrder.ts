type Version = { epoch: string; sequence: string };

function parse(value?: string): Version | undefined {
  const match = value?.match(/^([A-Za-z0-9_-]{16,64})\/([1-9][0-9]*)$/);
  return match ? { epoch: match[1]!, sequence: match[2]! } : undefined;
}

function newer(a: string, b: string) {
  return a.length > b.length || (a.length === b.length && a > b);
}

// A list watermark represents tombstones and the empty list. Record revisions
// alone cannot order deletion or unrelated delete/re-add identities.
export class SnapshotOrder {
  epoch: string | undefined;
  private sequence = '0';

  acceptSnapshot(value?: string, allowEpochChange = false): 'accepted' | 'changed' | 'stale' | 'refresh' {
    const version = parse(value);
    if (!version) return this.epoch || value ? 'stale' : 'accepted';
    if (this.epoch && version.epoch !== this.epoch && !allowEpochChange) return 'refresh';
    const changed = !!this.epoch && version.epoch !== this.epoch;
    if (!changed && this.epoch && !newer(version.sequence, this.sequence)) return 'stale';
    this.epoch = version.epoch;
    this.sequence = version.sequence;
    return changed ? 'changed' : 'accepted';
  }

  acceptReceipt(value: string | undefined, issuedEpoch: string | undefined) {
    const version = parse(value);
    if (!version) return !this.epoch;
    if (this.epoch && version.epoch !== this.epoch && issuedEpoch !== this.epoch) return false;
    const result = this.acceptSnapshot(value, true);
    return result === 'accepted' || result === 'changed';
  }
}
