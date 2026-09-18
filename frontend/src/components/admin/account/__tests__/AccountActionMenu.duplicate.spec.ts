import { afterEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import AccountActionMenu from '../AccountActionMenu.vue'
import type { Account } from '@/types'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const wrappers: ReturnType<typeof mount>[] = []
afterEach(() => { wrappers.splice(0).forEach(wrapper => wrapper.unmount()); document.body.innerHTML = '' })

function render(type: Account['type'], parent_account_id?: number, duplicating = false) {
  const account = { id: 42, name: 'source', platform: 'openai', type, parent_account_id } as Account
  const wrapper = mount(AccountActionMenu, {
    props: { show: true, position: { top: 10, left: 10 }, account, duplicating },
    global: { stubs: { Icon: true } }
  })
  wrappers.push(wrapper)
  const button = Array.from(document.querySelectorAll('button')).find(button => button.textContent?.includes('admin.accounts.duplicateAccount'))
  return { wrapper, button, account }
}

describe('account duplication menu', () => {
  it.each(['apikey', 'upstream', 'bedrock', 'service_account'] as const)('duplicates %s and closes the menu', type => {
    const { wrapper, button, account } = render(type)
    expect(button).toBeDefined()
    button!.click()
    expect(wrapper.emitted('duplicate')).toEqual([[account]])
    expect(wrapper.emitted('close')).toHaveLength(1)
  })
  it.each(['oauth', 'setup-token'] as const)('does not duplicate rotating %s credentials', type => {
    const { button } = render(type)
    expect(button).toBeUndefined()
    expect(document.body.textContent).toContain('admin.accounts.reAuthorize')
  })
  it('hides duplication for linked shadows', () => {
    expect(render('apikey', 1).button).toBeUndefined()
  })
  it('disables duplication while a request is pending and keeps other actions', () => {
    const { wrapper, button } = render('apikey', undefined, true)
    expect(button!.disabled).toBe(true)
    button!.click()
    expect(wrapper.emitted('duplicate')).toBeUndefined()
    expect(document.body.textContent).toContain('admin.accounts.testConnection')
    expect(document.body.textContent).toContain('admin.accounts.viewStats')
  })
})
