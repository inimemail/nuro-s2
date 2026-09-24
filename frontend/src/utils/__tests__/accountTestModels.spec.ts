import { describe, expect, it } from 'vitest'
import { sortAccountTestModels } from '../accountTestModels'

const model = (id: string, upstream_model?: string) => ({ id, display_name: id, type: 'model', created_at: '', upstream_model })
describe('account test model ordering', () => {
  it('keeps distinct requested aliases and removes only duplicate requested IDs', () => {
    const models = [model('old'), model('opus', 'claude-opus-5-5'), model('sol', 'gpt-6-sol'), model('gpt-6-sol'), model('old'), model('luna', 'gpt-6-luna'), model('grok-4.7')]
    expect(sortAccountTestModels(models, 'openai').map(m => m.id)).toEqual(['sol', 'gpt-6-sol', 'luna', 'opus', 'grok-4.7', 'old'])
    expect(models).toHaveLength(7)
  })
  it('does not insert unsupported models or reorder unrelated models', () => {
    expect(sortAccountTestModels([model('kimi-k2'), model('custom')], 'kimi').map(m => m.id)).toEqual(['kimi-k2', 'custom'])
    expect(sortAccountTestModels([model('gemini-2.5-pro'), model('gemini-2.5-flash-image')], 'gemini')[0].id).toBe('gemini-2.5-flash-image')
  })
})
