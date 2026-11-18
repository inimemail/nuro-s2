import type { AccountTestModel } from '@/composables/useGrokAccountTest'

const gpt62Order = ['gpt-6.2-sol', 'gpt-6.2-astra']
const additions = ['gpt-6.1-sol', 'gpt-6.1-terra', 'gpt-6-sol', 'gpt-6-luna', 'claude-sonnet-5-5', 'claude-opus-5-5', 'grok-4.7', 'grok-4.7-latest']
const geminiOrder = ['gemini-3.1-flash-image', 'gemini-2.5-flash-image', 'gemini-3.5-flash', 'gemini-2.5-flash', 'gemini-2.5-pro', 'gemini-3-flash-preview', 'gemini-3-pro-preview', 'gemini-2.0-flash']
export function sortAccountTestModels(models: AccountTestModel[], platform: string): AccountTestModel[] {
  const priorities = platform === 'typesafe'
    ? ['jev-latest']
    : [...gpt62Order, ...additions, ...(platform === 'openai' ? ['gpt-6-astra', 'gpt-6'] : ['gemini', 'antigravity'].includes(platform) ? geminiOrder : [])]
  const rank = (model: AccountTestModel) => {
    const ids = [model.id, model.upstream_model]
      .filter((id): id is string => Boolean(id))
      .map(id => id.trim().toLowerCase().replace(/^(openai|anthropic|xai|x-ai|grok|models)\//, ''))
    if (platform === 'openai') {
      const exactRank = gpt62Order.indexOf(ids[0])
      if (exactRank >= 0) return exactRank - gpt62Order.length
    }
    const ranks = ids.map(id => priorities.indexOf(id)).filter(index => index >= 0)
    return ranks.length > 0 ? Math.min(...ranks) : priorities.length
  }
  const seen = new Set<string>()
  return models.filter(model => { if (seen.has(model.id)) return false; seen.add(model.id); return true }).sort((a,b) => rank(a) - rank(b))
}

export function isOpenAIAccountTestImageModel(modelId: string, accountType: string): boolean {
  const id = modelId.trim().toLowerCase()
  if (id.startsWith('gpt-image-')) return true
  return accountType === 'apikey' && id.startsWith('gemini-') && (id.endsWith('-image') || id.includes('-image-'))
}
