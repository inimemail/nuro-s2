const pricingCatalogPlatforms = new Set([
  'anthropic', 'openai', 'gemini', 'antigravity', 'grok',
  'kimi', 'zhipu', 'deepseek', 'minimax'
])

export function supportsPricingModelSync(platform: string): boolean {
  return pricingCatalogPlatforms.has(platform)
}
