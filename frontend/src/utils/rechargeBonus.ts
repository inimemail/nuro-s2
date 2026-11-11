/** Keep exchange-rate precision separate from the currency settlement precision. */
export function formatRechargeMultiplier(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return '1.00'
  return value.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 6, useGrouping: false })
}
