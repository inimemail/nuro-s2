// Empty form/API values mean unknown, not a free (0x) upstream.
export function manualMultiplierFromValue(value: unknown): number | null {
  if (typeof value !== 'number' && typeof value !== 'string') return null
  if (typeof value === 'string' && value.trim() === '') return null
  const parsed = Number(value)
  // The backend treats 1 as clearing the manual override.
  return Number.isFinite(parsed) && parsed >= 0 && parsed <= 100 && parsed !== 1 ? parsed : null
}
