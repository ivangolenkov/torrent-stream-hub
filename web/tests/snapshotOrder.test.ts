import { expect, it } from 'vitest';
import { SnapshotOrder } from '../src/stores/snapshotOrder';

const a = 'aaaaaaaaaaaaaaaa', b = 'bbbbbbbbbbbbbbbb';

it('orders large sequences as decimal text, including empty-list tombstones', () => {
  const order = new SnapshotOrder();
  expect(order.acceptSnapshot(`${a}/9007199254740993`)).toBe('accepted');
  expect(order.acceptSnapshot(`${a}/9007199254740992`)).toBe('stale');
  expect(order.acceptSnapshot(`${a}/9007199254740993`)).toBe('stale');
  expect(order.acceptSnapshot(`${a}/10000000000000000`)).toBe('accepted');
  expect(order.acceptSnapshot()).toBe('stale');
});

it('confirms epoch changes through a fresh REST request, not an unsolicited retired SSE', () => {
  const order = new SnapshotOrder();
  order.acceptSnapshot(`${a}/99`);
  expect(order.acceptSnapshot(`${b}/1`)).toBe('refresh');
  expect(order.epoch).toBe(a);
  expect(order.acceptSnapshot(`${b}/2`, true)).toBe('changed');
  expect(order.acceptSnapshot(`${a}/999`)).toBe('refresh');
  expect(order.epoch).toBe(b);
  expect(order.acceptReceipt(`${a}/1000`, a)).toBe(false);
});

it('command acknowledgment retires in-flight snapshots without using a browser clock', () => {
  const order = new SnapshotOrder();
  order.acceptSnapshot(`${a}/7`);
  expect(order.acceptReceipt(`${a}/9`, a)).toBe(true);
  expect(order.acceptSnapshot(`${a}/8`)).toBe('stale');
  expect(order.acceptSnapshot(`${a}/10`)).toBe('accepted');
  expect(order.acceptReceipt(`${a}/9`, a)).toBe(false);
});
