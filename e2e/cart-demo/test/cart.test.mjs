import assert from 'node:assert/strict';
import test from 'node:test';
import { summarizeCart } from '../src/cart.mjs';

test('empty cart', () => {
  assert.deepEqual(summarizeCart([]), { itemCount: 0, totalCents: 0 });
});

test('quantities received as JSON strings are added numerically', () => {
  assert.deepEqual(summarizeCart([
    { unitPriceCents: 199, quantity: '2' },
    { unitPriceCents: 350, quantity: '3' },
  ]), { itemCount: 5, totalCents: 1448 });
});

test('an explicit zero quantity does not purchase one item', () => {
  assert.deepEqual(summarizeCart([
    { unitPriceCents: 499, quantity: 0 },
  ]), { itemCount: 0, totalCents: 0 });
});

test('digit strings, zeroes and default quantities preserve integer cents', () => {
  const items = Object.freeze([
    Object.freeze({ unitPriceCents: 199, quantity: '003' }),
    Object.freeze({ unitPriceCents: 499, quantity: '0' }),
    Object.freeze({ unitPriceCents: 101 }),
    Object.freeze({ unitPriceCents: 203, quantity: undefined }),
    Object.freeze({ unitPriceCents: 0, quantity: 2 }),
  ]);
  assert.deepEqual(summarizeCart(items), { itemCount: 7, totalCents: 901 });
});

test('rejects quantities outside the nonnegative safe integer contract', () => {
  for (const quantity of [
    '', ' ', '\t', '\n', '-1', '1.5', ' 2', '2 ', '+2', '2\n',
    '1e2', '0x10', '３', -1, 1.5, null, true, false, [], {},
    NaN, Infinity, Number.MAX_SAFE_INTEGER + 1, '9007199254740992',
  ]) {
    assert.throws(() => summarizeCart([{ unitPriceCents: 199, quantity }]), Error);
  }
});

test('rejects invalid prices even for zero quantity', () => {
  for (const unitPriceCents of [
    -1, 1.5, '199', null, undefined, true, NaN, Infinity,
    Number.MAX_SAFE_INTEGER + 1,
  ]) {
    assert.throws(() => summarizeCart([{ unitPriceCents, quantity: 0 }]), Error);
  }
});

test('accepts safe integer boundaries without changing the input', () => {
  const items = [{ unitPriceCents: 1, quantity: String(Number.MAX_SAFE_INTEGER) }];
  const original = structuredClone(items);
  assert.deepEqual(summarizeCart(items), {
    itemCount: Number.MAX_SAFE_INTEGER,
    totalCents: Number.MAX_SAFE_INTEGER,
  });
  assert.deepEqual(items, original);
  assert.deepEqual(summarizeCart([{ unitPriceCents: Number.MAX_SAFE_INTEGER }]), {
    itemCount: 1,
    totalCents: Number.MAX_SAFE_INTEGER,
  });
});
