export function summarizeCart(items) {
  let itemCount = 0;
  let totalCents = 0;
  for (const item of items) {
    const rawQuantity = item.quantity === undefined ? 1 : item.quantity;
    const quantity = typeof rawQuantity === 'string' && /^[0-9]+$/.test(rawQuantity)
      ? Number(rawQuantity)
      : rawQuantity;
    if (!Number.isSafeInteger(quantity) || quantity < 0) {
      throw new Error('Invalid quantity');
    }
    if (!Number.isSafeInteger(item.unitPriceCents) || item.unitPriceCents < 0) {
      throw new Error('Invalid unitPriceCents');
    }
    itemCount += quantity;
    totalCents += item.unitPriceCents * quantity;
  }
  return { itemCount, totalCents };
}
