export function summarizeCart(items) {
  let itemCount = 0;
  let totalCents = 0;
  for (const item of items) {
    const quantity = item.quantity || 1;
    itemCount += quantity;
    totalCents += item.unitPriceCents * quantity;
  }
  return { itemCount, totalCents };
}
