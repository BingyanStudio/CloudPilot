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
