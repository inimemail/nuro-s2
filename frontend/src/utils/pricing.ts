/**
 * formatScaled formats a per-token (or per-request) USD price scaled by `scale`.
 *
 *   formatScaled(0.000003, 1_000_000) → "$3"        // per 1M tokens
 *   formatScaled(0.5,        1)        → "$0.5"      // per request
 *   formatScaled(null,       1_000_000) → "-"
 *
 * Uses toPrecision(10) then strips trailing zeros to avoid IEEE 754 display noise.
 */
export function formatScaled(value: number | null, scale: number, minFractionDigits = 0): string {
  if (value == null) return '-'
  const precise = (value * scale).toPrecision(10)
  const exponentAt = precise.toLowerCase().indexOf('e')
  let mantissa = exponentAt === -1 ? precise : precise.slice(0, exponentAt)
  const exponent = exponentAt === -1 ? '' : precise.slice(exponentAt)
  mantissa = mantissa.replace(/\.?0+$/, '')
  if (minFractionDigits > 0) {
    const dot = mantissa.indexOf('.')
    const digits = dot === -1 ? 0 : mantissa.length - dot - 1
    if (digits < minFractionDigits) {
      mantissa = (dot === -1 ? `${mantissa}.` : mantissa) + '0'.repeat(minFractionDigits - digits)
    }
  }
  return `$${mantissa}${exponent}`
}
