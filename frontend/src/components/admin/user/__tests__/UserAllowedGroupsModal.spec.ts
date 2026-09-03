import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const { listGroupsMock, updateUserMock, showSuccessMock } = vi.hoisted(() => ({
  listGroupsMock: vi.fn(),
  updateUserMock: vi.fn(),
  showSuccessMock: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    groups: { list: listGroupsMock },
    users: { update: updateUserMock }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showSuccess: showSuccessMock })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

import UserAllowedGroupsModal from '../UserAllowedGroupsModal.vue'

const BaseDialogStub = defineComponent({
  props: { show: Boolean },
  emits: ['close'],
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

const groups = [
  { id: 1, name: 'Exclusive', platform: 'openai', subscription_type: 'standard', status: 'active', is_exclusive: true, rate_multiplier: 1 },
  { id: 2, name: 'Public A', platform: 'openai', subscription_type: 'standard', status: 'active', is_exclusive: false, rate_multiplier: 1 },
  { id: 3, name: 'Public B', platform: 'openai', subscription_type: 'standard', status: 'active', is_exclusive: false, rate_multiplier: 1 },
  { id: 4, name: 'Inactive', platform: 'openai', subscription_type: 'standard', status: 'disabled', is_exclusive: false, rate_multiplier: 1 }
]

function user(overrides: Record<string, unknown> = {}) {
  return {
    id: 9,
    email: 'user@example.com',
    allowed_groups: [1, 2],
    group_rates: { 1: 1.5, 2: 0.8 },
    restrict_public_groups: true,
    ...overrides
  } as any
}

async function mountDialog(currentUser = user()) {
  const wrapper = mount(UserAllowedGroupsModal, {
    props: { show: false, user: currentUser },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        PlatformIcon: true
      }
    }
  })
  await wrapper.setProps({ show: true })
  await flushPromises()
  return wrapper
}

describe('UserAllowedGroupsModal public group restrictions', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    listGroupsMock.mockResolvedValue({ items: groups })
    updateUserMock.mockResolvedValue({})
  })

  it('loads only explicitly allowed public groups for a restricted user', async () => {
    const wrapper = await mountDialog()
    const checkboxes = wrapper.findAll('input[type="checkbox"]')

    expect(wrapper.find('[role="switch"]').attributes('aria-checked')).toBe('true')
    expect(checkboxes).toHaveLength(3)
    expect((checkboxes[0].element as HTMLInputElement).checked).toBe(true)
    expect((checkboxes[1].element as HTMLInputElement).checked).toBe(true)
    expect((checkboxes[2].element as HTMLInputElement).checked).toBe(false)
  })

  it('saves selected public groups and preserves custom rates when restriction is enabled', async () => {
    const wrapper = await mountDialog()
    const checkboxes = wrapper.findAll('input[type="checkbox"]')
    await checkboxes[2].setValue(true)
    await wrapper.findAll('button').at(-1)!.trigger('click')
    await flushPromises()

    expect(updateUserMock).toHaveBeenCalledWith(9, {
      allowed_groups: [1, 2, 3],
      restrict_public_groups: true,
      group_rates: { 1: 1.5, 2: 0.8 }
    })
  })

  it('removes public groups from allowed_groups when restriction is disabled', async () => {
    const wrapper = await mountDialog()
    await wrapper.find('[role="switch"]').trigger('click')
    await wrapper.findAll('button').at(-1)!.trigger('click')
    await flushPromises()

    expect(updateUserMock).toHaveBeenCalledWith(9, {
      allowed_groups: [1],
      restrict_public_groups: false,
      group_rates: { 1: 1.5, 2: 0.8 }
    })
  })
})
