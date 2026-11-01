export function summarizeCart(items) {
  let itemCount = 0;
  let totalCents = 0;
  for (const item of items) {
    const unitPriceCents = item.unitPriceCents;
    if (
      typeof unitPriceCents !== 'number' ||
      !Number.isSafeInteger(unitPriceCents) ||
      unitPriceCents < 0
    ) {
      throw new RangeError('unitPriceCents must be a nonnegative safe integer');
    }

    let quantity = item.quantity;
    if (quantity === undefined) {
      quantity = 1;
    } else if (typeof quantity === 'string') {
      if (!/^[0-9]+$/.test(quantity)) {
        throw new RangeError('quantity must be a nonnegative safe integer');
      }
      quantity = Number(quantity);
    }
    if (!Number.isSafeInteger(quantity) || quantity < 0) {
      throw new RangeError('quantity must be a nonnegative safe integer');
    }

    itemCount += quantity;
    totalCents += unitPriceCents * quantity;
  }
  return { itemCount, totalCents };
}
