// Match the backend capability reader, including API/imported configurations.
// A boolean override wins; null/unset falls back to the capability collection.
export function isSeedanceEnabled(credentials?: Record<string, unknown>): boolean {
  if (typeof credentials?.seedance_enabled === 'boolean') return credentials.seedance_enabled
  const capabilities = credentials?.openai_capabilities
  if (Array.isArray(capabilities)) {
    return capabilities.some((value) => typeof value === 'string' && value.trim().toLowerCase() === 'seedance')
  }
  if (capabilities !== null && typeof capabilities === 'object') {
    return Object.entries(capabilities).some(([key, enabled]) => key.trim().toLowerCase() === 'seedance' && enabled === true)
  }
  return false
}
