import type { AccountTestModel } from '@/composables/useGrokAccountTest'

const additions = ['gpt-6-sol', 'gpt-6-luna', 'claude-opus-5-5', 'grok-4.7', 'grok-4.7-latest']
const geminiOrder = ['gemini-3.1-flash-image', 'gemini-2.5-flash-image', 'gemini-3.5-flash', 'gemini-2.5-flash', 'gemini-2.5-pro', 'gemini-3-flash-preview', 'gemini-3-pro-preview', 'gemini-2.0-flash']
export function sortAccountTestModels(models: AccountTestModel[], platform: string): AccountTestModel[] {
  const priorities = [...additions, ...(platform === 'openai' ? ['gpt-6-astra', 'gpt-6'] : ['gemini', 'antigravity'].includes(platform) ? geminiOrder : [])]
  const rank = (model: AccountTestModel) => {
    const id = (model.upstream_model || model.id).replace(/^(openai|anthropic|xai|x-ai|grok|models)\//, '')
    const index = priorities.indexOf(id)
    return index < 0 ? priorities.length : index
  }
  const seen = new Set<string>()
  return models.filter(model => { if (seen.has(model.id)) return false; seen.add(model.id); return true }).sort((a,b) => rank(a) - rank(b))
}
