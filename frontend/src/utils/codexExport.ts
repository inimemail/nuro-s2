// Match the backend's exact or trailing-* model allowlist semantics.
export function selectDefaultCodexModel(allowed?: string[] | null): string {
  const patterns = allowed?.map(value => value.trim()).filter(Boolean)
  if (!patterns?.length) return 'gpt-6.1-sol'
  const matches = (model: string) => patterns.some(pattern => pattern === model || (pattern.endsWith('*') && model.startsWith(pattern.slice(0, -1))))
  for (const model of ['gpt-6.1-sol', 'gpt-6.1-terra']) if (matches(model)) return model
  return patterns.find(model => !model.includes('*')) || ''
}

export function utf8Base64(value: string): string {
  const bytes = new TextEncoder().encode(value)
  // Avoid spreading an unbounded array onto the function argument stack.
  let binary = ''
  for (const byte of bytes) binary += String.fromCharCode(byte)
  return btoa(binary)
}

// This compatibility marker is public metadata. Authentication still uses the
// exported API key through the client's normal Bearer mechanism.
export const CODEX_IMAGE_ACTOR_HEADERS = 'http_headers = { "x-openai-actor-authorization" = "local-image-extension" }'
