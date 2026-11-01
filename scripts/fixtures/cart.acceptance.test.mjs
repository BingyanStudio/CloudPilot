import assert from 'node:assert/strict';
import test from 'node:test';
import { pathToFileURL } from 'node:url';

if (!process.env.CLOUDPILOT_E2E_CART_MODULE) throw new Error('Set CLOUDPILOT_E2E_CART_MODULE to the cart module under review');
const { summarizeCart } = await import(pathToFileURL(process.env.CLOUDPILOT_E2E_CART_MODULE).href);

test('ASCII quantities normalize without string concatenation', () => {
  assert.deepEqual(summarizeCart([
    { unitPriceCents: 199, quantity: '0002' },
    { unitPriceCents: 350, quantity: '3' },
    { unitPriceCents: 500, quantity: '0' },
  ]), { itemCount: 5, totalCents: 1448 });
  assert.deepEqual(summarizeCart([{ unitPriceCents: 0, quantity: String(Number.MAX_SAFE_INTEGER) }]),
    { itemCount: Number.MAX_SAFE_INTEGER, totalCents: 0 });
});

test('only missing quantities default to one', () => {
  assert.deepEqual(summarizeCart([
    { unitPriceCents: 125 },
    { unitPriceCents: 225, quantity: undefined },
    { unitPriceCents: 999, quantity: 0 },
    { unitPriceCents: 999, quantity: '000' },
  ]), { itemCount: 2, totalCents: 350 });
});

test('invalid quantities are rejected', () => {
  for (const quantity of [null, false, true, [], {}, '', ' ', '\t2', '2\n', '+2', '-2', '2.0', '2e0', '２', '٢', -1, 0.5, NaN, Infinity, Number.MAX_SAFE_INTEGER + 1, '9007199254740992']) {
    assert.throws(() => summarizeCart([{ unitPriceCents: 100, quantity }]), Error);
  }
});

test('unit prices require nonnegative safe integer numbers', () => {
  for (const unitPriceCents of [undefined, null, false, true, '100', -1, 0.1, NaN, Infinity, Number.MAX_SAFE_INTEGER + 1]) {
    assert.throws(() => summarizeCart([{ unitPriceCents, quantity: 1 }]), Error);
  }
  assert.deepEqual(summarizeCart([{ unitPriceCents: Number.MAX_SAFE_INTEGER, quantity: 1 }]),
    { itemCount: 1, totalCents: Number.MAX_SAFE_INTEGER });
});

test('inputs remain immutable and empty carts have numeric zero totals', () => {
  const items = Object.freeze([Object.freeze({ unitPriceCents: 105, quantity: '03' })]);
  assert.deepEqual(summarizeCart(items), { itemCount: 3, totalCents: 315 });
  assert.equal(items[0].quantity, '03');
  assert.deepEqual(summarizeCart(Object.freeze([])), { itemCount: 0, totalCents: 0 });
});
