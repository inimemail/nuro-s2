import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { describe, expect, it } from 'vitest'

const currentDir = dirname(fileURLToPath(import.meta.url))
const source = readFileSync(resolve(currentDir, '../GroupsView.vue'), 'utf8')

describe('groups OpenAI Live control', () => {
  it('uses the shared toggle and scopes both controls to OpenAI groups', () => {
    expect(source.match(/v-if="createForm\.platform === 'openai'"/g)?.length).toBeGreaterThan(0)
    expect(source.match(/v-if="editForm\.platform === 'openai'"/g)?.length).toBeGreaterThan(0)
    expect(source).toContain('<Toggle')
    expect(source).toContain("requestLiveToggle('create', $event)")
    expect(source).toContain("requestLiveToggle('edit', $event)")
  })

  it('keeps the setting default-off, persisted, restored, and cleared off-platform', () => {
    expect(source.match(/allow_live: false/g)?.length).toBe(2)
    expect(source).toContain('editForm.allow_live = group.allow_live ?? false')
    expect(source).toContain('createForm.allow_live = false')
    expect(source).toContain('editForm.allow_live = false')
    expect(source).toContain('showUnsupportedLiveConfirm')
    expect(source).toContain('getLiveCapability()')
  })

  it('does not apply a stale capability result or confirmation to a closed or non-OpenAI form', () => {
    expect(source).toContain('showCreateModal.value && createForm.platform === "openai"')
    expect(source).toContain('showEditModal.value && editForm.platform === "openai"')
    expect(source.match(/pendingLiveForm\.value === "create"/g)?.length).toBeGreaterThanOrEqual(3)
    expect(source.match(/pendingLiveForm\.value === "edit"/g)?.length).toBeGreaterThanOrEqual(3)
  })
})
