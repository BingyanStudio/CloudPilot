# Cart summary

`summarizeCart(items)` returns `{ itemCount, totalCents }` as numbers.

- `unitPriceCents` is a nonnegative safe integer number; never convert money to floating-point dollars.
- `quantity` accepts a nonnegative safe integer number or an ASCII digit string such as `"0"`, `"2"`, or `"003"`.
- An omitted/undefined quantity defaults to 1. Explicit 0 means zero units.
- Reject other quantity values and invalid unitPriceCents with an Error; the error subclass/message is not prescribed.
- The caller's array/items must remain unchanged. Empty carts return two zeroes.
- Successful aggregate counts and amounts in this exercise fit within Number.MAX_SAFE_INTEGER.

Run `npm test`. There are no dependencies or build steps.
