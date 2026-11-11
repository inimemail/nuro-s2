import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountUsageCell from '../AccountUsageCell.vue'
import type { Account } from '@/types'

const { getUsage, queryOpenAIQuota, queryGrokQuota, resetOpenAIQuota, updateCodexAutoResetMode, sendOpenAICodexInvite } = vi.hoisted(() => ({
  getUsage: vi.fn(),
  queryOpenAIQuota: vi.fn(),
  queryGrokQuota: vi.fn(),
  resetOpenAIQuota: vi.fn(),
  updateCodexAutoResetMode: vi.fn(),
  sendOpenAICodexInvite: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      getUsage,
      queryOpenAIQuota,
      resetOpenAIQuota,
      updateCodexAutoResetMode,
      sendOpenAICodexInvite
    },
    grok: { queryQuota: queryGrokQuota }
  }
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

function makeAccount(overrides: Partial<Account>): Account {
  return {
    id: 1,
    name: 'account',
    platform: 'antigravity',
    type: 'oauth',
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    status: 'active',
    error_message: null,
    last_used_at: null,
    expires_at: null,
    auto_pause_on_expired: true,
    created_at: '2026-03-15T00:00:00Z',
    updated_at: '2026-03-15T00:00:00Z',
    schedulable: true,
    rate_limited_at: null,
    rate_limit_reset_at: null,
    overload_until: null,
    temp_unschedulable_until: null,
    temp_unschedulable_reason: null,
    session_window_start: null,
    session_window_end: null,
    session_window_status: null,
    ...overrides,
  }
}

describe('AccountUsageCell', () => {
  beforeEach(() => {
    getUsage.mockReset()
    queryOpenAIQuota.mockReset()
    queryOpenAIQuota.mockResolvedValue({})
    Object.defineProperty(window, 'matchMedia', {
      writable: true,
      value: vi.fn().mockImplementation(() => ({
        matches: true,
        media: '(min-width: 768px)',
        onchange: null,
        addListener: vi.fn(),
        removeListener: vi.fn(),
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
        dispatchEvent: vi.fn(),
      }))
    })
  })

  it('Vertex 配额与本地今日统计同时显示，非 Gemini service account 不误用配额', async () => {
    getUsage.mockResolvedValue({})
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 2118, platform: 'gemini', type: 'service_account', quota_limit: 100, quota_used: 25, quota_daily_limit: 10, quota_daily_used: 4 }),
        todayStats: { requests: 12, tokens: 3200, cost: 1.25, user_cost: 2.5 }
      },
      global: { stubs: { UsageProgressBar: { props: ['label', 'utilization'], template: '<div>{{ label }}|{{ utilization }}</div>' }, AccountQuotaInfo: true } }
    })
    await flushPromises()
    expect(wrapper.get('[data-testid="gemini-account-quota"]').text()).toContain('total|25')
    expect(wrapper.text()).toContain('12 req')
    expect(wrapper.text()).toContain('A $1.25')
    await wrapper.setProps({ account: makeAccount({ id: 2119, platform: 'anthropic', type: 'service_account', quota_limit: 100 }) })
    expect(wrapper.find('[data-testid="gemini-account-quota"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('Antigravity 图片用量会聚合新旧 image 模型', async () => {
    getUsage.mockResolvedValue({
      antigravity_quota: {
        'gemini-2.5-flash-image': {
          utilization: 45,
          reset_time: '2026-03-01T11:00:00Z'
        },
        'gemini-3.1-flash-image': {
          utilization: 20,
          reset_time: '2026-03-01T10:00:00Z'
        },
        'gemini-3-pro-image': {
          utilization: 70,
          reset_time: '2026-03-01T09:00:00Z'
        }
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 1001,
          platform: 'antigravity',
          type: 'oauth',
          extra: {}
        })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization', 'resetsAt', 'color'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ resetsAt }}</div>'
          },
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()

    expect(wrapper.text()).toContain('admin.accounts.usageWindow.gemini3Image|70|2026-03-01T09:00:00Z')
  })

  it('Antigravity 会显示 AI Credits 余额信息', async () => {
    getUsage.mockResolvedValue({
      ai_credits: [
        {
          credit_type: 'GOOGLE_ONE_AI',
          amount: 25,
          minimum_balance: 5
        }
      ]
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 1002,
          platform: 'antigravity',
          type: 'oauth',
          extra: {}
        })
      },
      global: {
        stubs: {
          UsageProgressBar: true,
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()

    expect(wrapper.text()).toContain('admin.accounts.aiCreditsBalance')
    expect(wrapper.text()).toContain('25')
  })


  it('OpenAI OAuth 快照已过期时首屏会重新请求 usage', async () => {
    getUsage.mockResolvedValue({
      five_hour: {
        utilization: 15,
        resets_at: '2026-03-08T12:00:00Z',
        remaining_seconds: 3600,
        window_stats: {
          requests: 3,
          tokens: 300,
          cost: 0.03,
          standard_cost: 0.03,
          user_cost: 0.03
        }
      },
      seven_day: {
        utilization: 77,
        resets_at: '2026-03-13T12:00:00Z',
        remaining_seconds: 3600,
        window_stats: {
          requests: 3,
          tokens: 300,
          cost: 0.03,
          standard_cost: 0.03,
          user_cost: 0.03
        }
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 2000,
          platform: 'openai',
          type: 'oauth',
          extra: {
            codex_usage_updated_at: '2026-03-07T00:00:00Z',
            codex_5h_used_percent: 12,
            codex_5h_reset_at: '2026-03-08T12:00:00Z',
            codex_7d_used_percent: 34,
            codex_7d_reset_at: '2026-03-13T12:00:00Z'
          }
        })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization', 'resetsAt', 'windowStats', 'color'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ windowStats?.tokens }}</div>'
          },
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()

    expect(getUsage).toHaveBeenCalledWith(2000, undefined)
    expect(wrapper.text()).toContain('5h|15|300')
    expect(wrapper.text()).toContain('7d|77|300')
  })

  it('OpenAI OAuth 有 codex 快照时仍然使用 /usage API 数据渲染', async () => {
    getUsage.mockResolvedValue({
      five_hour: {
        utilization: 18,
        resets_at: '2099-03-07T12:00:00Z',
        remaining_seconds: 3600,
        window_stats: {
          requests: 9,
          tokens: 900,
          cost: 0.09,
          standard_cost: 0.09,
          user_cost: 0.09
        }
      },
      seven_day: {
        utilization: 36,
        resets_at: '2099-03-13T12:00:00Z',
        remaining_seconds: 3600,
        window_stats: {
          requests: 9,
          tokens: 900,
          cost: 0.09,
          standard_cost: 0.09,
          user_cost: 0.09
        }
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 2001,
          platform: 'openai',
          type: 'oauth',
          extra: {
            codex_usage_updated_at: '2099-03-07T10:00:00Z',
            codex_5h_used_percent: 12,
            codex_5h_reset_at: '2099-03-07T12:00:00Z',
            codex_7d_used_percent: 34,
            codex_7d_reset_at: '2099-03-13T12:00:00Z'
          }
        })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization', 'resetsAt', 'windowStats', 'color'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ windowStats?.tokens }}</div>'
          },
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()

    expect(getUsage).toHaveBeenCalledWith(2001, undefined)
    // 单一数据源：始终使用 /usage API 返回值，忽略 codex 快照
    expect(wrapper.text()).toContain('5h|18|900')
    expect(wrapper.text()).toContain('7d|36|900')
  })

  it('OpenAI OAuth 点击查询会单独刷新 reset credit 次数', async () => {
    getUsage.mockResolvedValue({
      updated_at: null,
      five_hour: null,
      seven_day: null,
      seven_day_sonnet: null
    })
    queryOpenAIQuota.mockResolvedValue({
      rate_limit_reset_credits: {
        available_count: 1
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 2020,
          platform: 'openai',
          type: 'oauth',
          extra: {}
        })
      },
      global: {
        stubs: {
          UsageProgressBar: true,
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()

    const queryButton = wrapper.findAll('button').find(button => button.text().includes('admin.accounts.usageWindow.activeQuery'))
    expect(queryButton).toBeTruthy()
    await queryButton!.trigger('click')
    await flushPromises()

    expect(getUsage).toHaveBeenCalledWith(2020, 'active', true)
    expect(queryOpenAIQuota).toHaveBeenCalledWith(2020)
    expect(wrapper.text()).toContain('admin.accounts.usageWindow.resetCredits 1')
  })

  it('OpenAI OAuth reset credit 查询缺少次数时显示 0', async () => {
    getUsage.mockResolvedValue({
      updated_at: null,
      five_hour: null,
      seven_day: null,
      seven_day_sonnet: null
    })
    queryOpenAIQuota.mockResolvedValue({
      rate_limit_reset_credits: {}
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 2021,
          platform: 'openai',
          type: 'oauth',
          extra: {}
        })
      },
      global: {
        stubs: {
          UsageProgressBar: true,
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()

    const queryButton = wrapper.findAll('button').find(button => button.text().includes('admin.accounts.usageWindow.activeQuery'))
    expect(queryButton).toBeTruthy()
    await queryButton!.trigger('click')
    await flushPromises()

    expect(queryOpenAIQuota).toHaveBeenCalledWith(2021)
    expect(wrapper.text()).toContain('admin.accounts.usageWindow.resetCredits 0')
    expect(wrapper.text()).not.toContain('未知')
  })

  it('OpenAI OAuth invites 缺少 slotsLeft 时用 limit-created 显示邀请按钮', async () => {
    getUsage.mockResolvedValue({
      updated_at: null,
      five_hour: null,
      seven_day: null,
      seven_day_sonnet: null
    })
    queryOpenAIQuota.mockResolvedValue({
      rate_limit_reset_credits: {
        available_count: 0
      },
      invites: {
        created: 1,
        limit: 3
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 2022,
          platform: 'openai',
          type: 'oauth',
          extra: {}
        })
      },
      global: {
        stubs: {
          UsageProgressBar: true,
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()

    const queryButton = wrapper.findAll('button').find(button => button.text().includes('admin.accounts.usageWindow.activeQuery'))
    expect(queryButton).toBeTruthy()
    await queryButton!.trigger('click')
    await flushPromises()

    expect(wrapper.findAll('button').some(button => button.text().trim() === '邀请')).toBe(true)
  })

  it('OpenAI OAuth 未获取到 invites 时仍显示可点邀请按钮', async () => {
    getUsage.mockResolvedValue({
      updated_at: null,
      five_hour: null,
      seven_day: null,
      seven_day_sonnet: null,
      codex_reset_credits_supported: true,
      codex_reset_credits_available_count: 1
    })
    queryOpenAIQuota.mockResolvedValue({
      rate_limit_reset_credits: {
        available_count: 1
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 2023,
          platform: 'openai',
          type: 'oauth',
          extra: {}
        })
      },
      global: {
        stubs: {
          UsageProgressBar: true,
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()

    const inviteButton = wrapper.findAll('button').find(button => button.text().trim() === '邀请')
    expect(inviteButton).toBeTruthy()
    expect(inviteButton!.attributes('disabled')).toBeUndefined()
  })

  it('仅为 OpenAI OAuth 7d 窗口计算预计总费用', async () => {
    getUsage.mockResolvedValue({
      five_hour: {
        utilization: 25,
        resets_at: null,
        remaining_seconds: 0,
        window_stats: { requests: 1, tokens: 100, cost: 2 }
      },
      seven_day: {
        utilization: 40,
        resets_at: null,
        remaining_seconds: 0,
        window_stats: { requests: 2, tokens: 200, cost: 12 }
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 6752, platform: 'openai', type: 'oauth' })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'estimatedTotalCost'],
            template: '<div class="usage-bar">{{ label }}|{{ estimatedTotalCost ?? "none" }}</div>'
          },
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()

    expect(wrapper.text()).toContain('5h|none')
    expect(wrapper.text()).toContain('7d|30')
  })

  it.each([
    { id: 6801, utilization: 0, cost: 12 },
    { id: 6802, utilization: -1, cost: 12 },
    { id: 6803, utilization: Number.NaN, cost: 12 },
    { id: 6804, utilization: 40, cost: 0 },
    { id: 6805, utilization: 40, cost: Number.POSITIVE_INFINITY }
  ])('OpenAI OAuth 7d 输入无效时不显示预计总费用 (%o)', async ({ id, utilization, cost }) => {
    getUsage.mockResolvedValue({
      seven_day: {
        utilization,
        resets_at: null,
        remaining_seconds: 0,
        window_stats: { requests: 1, tokens: 100, cost }
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id, platform: 'openai', type: 'oauth' })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'estimatedTotalCost'],
            template: '<div class="usage-bar">{{ label }}|{{ estimatedTotalCost ?? "none" }}</div>'
          },
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()

    expect(wrapper.text()).toContain('7d|none')
    expect(wrapper.text()).not.toMatch(/Infinity|NaN/)
  })

  it('OpenAI OAuth 有现成快照时，手动刷新信号会触发 usage 重拉', async () => {
    getUsage.mockResolvedValue({
      five_hour: {
        utilization: 18,
        resets_at: '2099-03-07T12:00:00Z',
        remaining_seconds: 3600,
        window_stats: {
          requests: 9,
          tokens: 900,
          cost: 0.09,
          standard_cost: 0.09,
          user_cost: 0.09
        }
      },
      seven_day: {
        utilization: 36,
        resets_at: '2099-03-13T12:00:00Z',
        remaining_seconds: 3600,
        window_stats: {
          requests: 9,
          tokens: 900,
          cost: 0.09,
          standard_cost: 0.09,
          user_cost: 0.09
        }
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 2010,
          platform: 'openai',
          type: 'oauth',
          extra: {
            codex_usage_updated_at: '2099-03-07T10:00:00Z',
            codex_5h_used_percent: 12,
            codex_5h_reset_at: '2099-03-07T12:00:00Z',
            codex_7d_used_percent: 34,
            codex_7d_reset_at: '2099-03-13T12:00:00Z'
          },
          rate_limit_reset_at: null
        }),
        manualRefreshToken: 0
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization', 'resetsAt', 'windowStats', 'color'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ windowStats?.tokens }}</div>'
          },
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()
    // mount 时已经拉取一次
    expect(getUsage).toHaveBeenCalledTimes(1)

    await wrapper.setProps({ manualRefreshToken: 1 })
    await flushPromises()

    // 手动刷新再拉一次
    expect(getUsage).toHaveBeenCalledTimes(2)
    expect(getUsage).toHaveBeenCalledWith(2010, undefined)
    // 单一数据源：始终使用 /usage API 值
    expect(wrapper.text()).toContain('5h|18|900')
  })

  it('OpenAI OAuth 在无 codex 快照时会回退显示 usage 接口窗口', async () => {
    getUsage.mockResolvedValue({
      five_hour: {
        utilization: 0,
        resets_at: null,
        remaining_seconds: 0,
        window_stats: {
          requests: 2,
          tokens: 27700,
          cost: 0.06,
          standard_cost: 0.06,
          user_cost: 0.06
        }
      },
      seven_day: {
        utilization: 0,
        resets_at: null,
        remaining_seconds: 0,
        window_stats: {
          requests: 2,
          tokens: 27700,
          cost: 0.06,
          standard_cost: 0.06,
          user_cost: 0.06
        }
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 2002,
          platform: 'openai',
          type: 'oauth',
          extra: {}
        })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization', 'resetsAt', 'windowStats', 'color'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ windowStats?.tokens }}</div>'
          },
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()

    expect(getUsage).toHaveBeenCalledWith(2002, undefined)
    expect(wrapper.text()).toContain('5h|0|27700')
    expect(wrapper.text()).toContain('7d|0|27700')
  })

  it('OpenAI OAuth 在行数据刷新但仍无 codex 快照时会重新拉取 usage', async () => {
    getUsage
      .mockResolvedValueOnce({
        five_hour: {
          utilization: 0,
          resets_at: null,
          remaining_seconds: 0,
          window_stats: {
            requests: 1,
            tokens: 100,
            cost: 0.01,
            standard_cost: 0.01,
            user_cost: 0.01
          }
        },
        seven_day: null
      })
      .mockResolvedValueOnce({
        five_hour: {
          utilization: 0,
          resets_at: null,
          remaining_seconds: 0,
          window_stats: {
            requests: 2,
            tokens: 200,
            cost: 0.02,
            standard_cost: 0.02,
            user_cost: 0.02
          }
        },
        seven_day: null
      })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 2003,
          platform: 'openai',
          type: 'oauth',
          updated_at: '2026-03-07T10:00:00Z',
          extra: {}
        })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization', 'resetsAt', 'windowStats', 'color'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ windowStats?.tokens }}</div>'
          },
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()
    expect(wrapper.text()).toContain('5h|0|100')
    expect(getUsage).toHaveBeenCalledTimes(1)

    await wrapper.setProps({
      account: {
        id: 2003,
        platform: 'openai',
        type: 'oauth',
        updated_at: '2026-03-07T10:01:00Z',
        extra: {}
      }
    })

    await flushPromises()
    expect(getUsage).toHaveBeenCalledTimes(2)
    expect(wrapper.text()).toContain('5h|0|200')
  })

  it('OpenAI OAuth 已限额时显示 /usage API 返回的限额数据', async () => {
    getUsage.mockResolvedValue({
      five_hour: {
        utilization: 100,
        resets_at: '2026-03-07T12:00:00Z',
        remaining_seconds: 3600,
        window_stats: {
          requests: 211,
          tokens: 106540000,
          cost: 38.13,
          standard_cost: 38.13,
          user_cost: 38.13
        }
      },
      seven_day: {
        utilization: 100,
        resets_at: '2026-03-13T12:00:00Z',
        remaining_seconds: 3600,
        window_stats: {
          requests: 211,
          tokens: 106540000,
          cost: 38.13,
          standard_cost: 38.13,
          user_cost: 38.13
        }
      }
    })

    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 2004,
          platform: 'openai',
          type: 'oauth',
          rate_limit_reset_at: '2099-03-07T12:00:00Z',
          extra: {
            codex_5h_used_percent: 0,
            codex_7d_used_percent: 0
          }
        })
      },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization', 'resetsAt', 'windowStats', 'color'],
            template: '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ windowStats?.tokens }}</div>'
          },
          AccountQuotaInfo: true
        }
      }
    })

    await flushPromises()

    expect(getUsage).toHaveBeenCalledWith(2004, undefined)
    expect(wrapper.text()).toContain('5h|100|106540000')
    expect(wrapper.text()).toContain('7d|100|106540000')
  })

  it('Key 账号会展示 today stats 徽章并带 A/U 提示', async () => {
		const wrapper = mount(AccountUsageCell, {
		  props: {
		    account: makeAccount({
		      id: 3001,
		      platform: 'anthropic',
		      type: 'apikey'
		    }),
		    todayStats: {
		      requests: 1_000_000,
		      tokens: 1_000_000_000,
		      cost: 12.345,
		      standard_cost: 12.345,
		      user_cost: 6.789
		    }
		  },
		  global: {
		    stubs: {
		      UsageProgressBar: true,
		      AccountQuotaInfo: true
		    }
		  }
		})

		await flushPromises()

		expect(wrapper.text()).toContain('1.0M req')
		expect(wrapper.text()).toContain('1.0B')
		expect(wrapper.text()).toContain('A $12.35')
		expect(wrapper.text()).toContain('U $6.79')

		const badges = wrapper.findAll('span[title]')
		expect(badges.some(node => node.attributes('title') === 'usage.accountBilled')).toBe(true)
		expect(badges.some(node => node.attributes('title') === 'usage.userBilled')).toBe(true)
  })

  it('Key 账号在 today stats loading 时显示骨架屏', async () => {
		const wrapper = mount(AccountUsageCell, {
		  props: {
		    account: makeAccount({
		      id: 3002,
		      platform: 'anthropic',
		      type: 'apikey'
		    }),
		    todayStats: null,
		    todayStatsLoading: true
		  },
		  global: {
		    stubs: {
		      UsageProgressBar: true,
		      AccountQuotaInfo: true
		    }
		  }
		})

		await flushPromises()

		expect(wrapper.findAll('.animate-pulse').length).toBeGreaterThan(0)
  })

  it('Key 账号在无 today stats 且无配额时显示兜底短横线', async () => {
		const wrapper = mount(AccountUsageCell, {
		  props: {
		    account: makeAccount({
		      id: 3003,
		      platform: 'anthropic',
		      type: 'apikey',
		      quota_limit: 0,
		      quota_daily_limit: 0,
		      quota_weekly_limit: 0
		    }),
		    todayStats: null,
		    todayStatsLoading: false
		  },
		  global: {
		    stubs: {
		      UsageProgressBar: true,
		      AccountQuotaInfo: true
		    }
		  }
		})

		await flushPromises()

		expect(wrapper.text().trim()).toBe('-')
  })

  it('Vertex 账号会在 Gemini 用量窗口里展示 today stats 徽章', async () => {
		const wrapper = mount(AccountUsageCell, {
		  props: {
		    account: makeAccount({
		      id: 4001,
		      platform: 'gemini',
		      type: 'service_account',
          credentials: {
            tier_id: 'vertex',
            project_id: 'vertex-proj',
            client_email: 'svc@vertex-proj.iam.gserviceaccount.com',
            location: 'global'
          },
		      extra: {}
		    }),
		    todayStats: {
		      requests: 0,
		      tokens: 0,
		      cost: 0,
		      standard_cost: 0,
		      user_cost: 0
		    }
		  },
		  global: {
		    stubs: {
		      UsageProgressBar: true,
		      AccountQuotaInfo: true
		    }
		  }
		})

		await flushPromises()

		expect(wrapper.text()).toContain('0 req')
		expect(wrapper.text()).toContain('0')
		expect(wrapper.text()).toContain('A $0.00')
		expect(wrapper.text()).toContain('U $0.00')
  })
})

describe('AccountUsageCell request ownership', () => {
  function deferred<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>(r => { resolve = r }); return { promise, resolve } }
  const usage = (value: number) => ({ source: 'passive', five_hour: { utilization: value } })
  function mountUsage(id: number) { return mount(AccountUsageCell, { props: { account: makeAccount({ id, platform: 'anthropic', type: 'oauth' }) }, global: { stubs: { UsageProgressBar: { props: ['utilization'], template: '<div class="usage-fixture">{{utilization}}</div>' }, AccountQuotaInfo: true } } }) }
  beforeEach(() => {
    getUsage.mockReset(); queryOpenAIQuota.mockReset(); queryOpenAIQuota.mockResolvedValue({})
    queryGrokQuota.mockReset()
    resetOpenAIQuota.mockReset(); updateCodexAutoResetMode.mockReset(); sendOpenAICodexInvite.mockReset()
    Object.defineProperty(window, 'matchMedia', { writable: true, value: vi.fn(() => ({ matches: true, addEventListener: vi.fn(), removeEventListener: vi.fn() })) })
  })
  it('offers one explicit Antigravity force query and keeps last-good quota on failure', async () => {
    const quota = { antigravity_quota: { 'gemini-3-pro-high': { utilization: 42, reset_time: null } } }
    let reject!: (error: Error) => void
    const pending = new Promise<never>((_, fail) => { reject = fail })
    getUsage.mockResolvedValueOnce(quota).mockReturnValueOnce(pending)
    const wrapper = mount(AccountUsageCell, {
      props: { account: makeAccount({ id: 99105, platform: 'antigravity', type: 'oauth' }) },
      global: { stubs: { UsageProgressBar: { props: ['utilization'], template: '<div class="usage-fixture">{{utilization}}</div>' }, AccountQuotaInfo: true } }
    })
    await flushPromises()
    const button = wrapper.get('[data-testid="ag-usage-query"]')
    await button.trigger('click')
    expect(getUsage).toHaveBeenLastCalledWith(99105, 'active', true)
    expect(button.attributes('disabled')).toBeDefined()
    await button.trigger('click')
    expect(getUsage).toHaveBeenCalledTimes(2)
    expect(wrapper.get('.usage-fixture').text()).toBe('42')
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {})
    try {
      reject(new Error('fixture'))
      await flushPromises()
      expect(wrapper.get('.usage-fixture').text()).toBe('42')
      expect(wrapper.text()).toContain('admin.accounts.usageRefreshFailed')
      expect(button.attributes('disabled')).toBeUndefined()
    } finally {
      consoleError.mockRestore()
      wrapper.unmount()
    }
  })
  it('keeps last good usage during refresh and ignores passive data after active refresh', async () => {
    const passive = deferred<ReturnType<typeof usage>>()
    getUsage.mockResolvedValueOnce(usage(10)).mockReturnValueOnce(passive.promise).mockResolvedValueOnce(usage(80))
    const wrapper = mountUsage(99101); await flushPromises()
    await wrapper.setProps({ manualRefreshToken: 1 }); await flushPromises()
    expect(wrapper.find('.usage-fixture').text()).toBe('10')
    const active = wrapper.findAll('button').find(button => button.text().includes('activeQuery'))
    expect(active).toBeDefined(); await active!.trigger('click'); await flushPromises()
    expect(wrapper.find('.usage-fixture').text()).toBe('80')
    passive.resolve(usage(20)); await flushPromises(); expect(wrapper.find('.usage-fixture').text()).toBe('80'); wrapper.unmount()
  })
  it('refreshes Grok usage after one active probe without admitting duplicate probes', async () => {
    const probe = deferred<Record<string, never>>()
    queryGrokQuota.mockReturnValueOnce(probe.promise)
    getUsage.mockResolvedValueOnce({ grok_request_quota: { limit: 100, remaining: 80 } })
      .mockResolvedValueOnce({ grok_request_quota: { limit: 100, remaining: 30 } })
    const wrapper = mount(AccountUsageCell, {
      props: { account: makeAccount({ id: 99201, platform: 'grok', type: 'oauth' }) },
      global: { stubs: { UsageProgressBar: { props: ['utilization'], template: '<div class="usage-fixture">{{utilization}}</div>' }, AccountQuotaInfo: true } }
    })
    await flushPromises()
    const button = wrapper.findAll('button').find(item => item.text().includes('activeQuery'))!
    await button.trigger('click')
    await button.trigger('click')
    expect(queryGrokQuota).toHaveBeenCalledTimes(1)
    expect(queryGrokQuota).toHaveBeenCalledWith(99201)
    expect(wrapper.get('.usage-fixture').text()).toBe('80')
    probe.resolve({})
    await flushPromises()
    expect(getUsage).toHaveBeenCalledTimes(2)
    expect(getUsage).toHaveBeenLastCalledWith(99201, undefined)
    expect(wrapper.get('.usage-fixture').text()).toBe('30')
    expect(button.attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })
  it('discards a late Codex quota query after replacing the account', async () => {
    const quota = deferred<{ rate_limit_reset_credits: { available_count: number } }>()
    getUsage.mockResolvedValueOnce(usage(10)).mockResolvedValueOnce(usage(20)).mockResolvedValue(usage(60))
    queryOpenAIQuota.mockReturnValueOnce(quota.promise)
    const wrapper = mount(AccountUsageCell, {
      props: { account: makeAccount({ id: 99202, platform: 'openai', type: 'oauth' }) },
      global: { stubs: { UsageProgressBar: { props: ['utilization'], template: '<div class="usage-fixture">{{utilization}}</div>' }, AccountQuotaInfo: true } }
    })
    await flushPromises()
    await wrapper.findAll('button').find(item => item.text().includes('activeQuery'))!.trigger('click')
    await flushPromises()
    expect(queryOpenAIQuota).toHaveBeenCalledWith(99202)
    await wrapper.setProps({ account: makeAccount({ id: 99203, platform: 'openai', type: 'oauth' }) })
    await flushPromises()
    expect(getUsage).toHaveBeenCalledTimes(3)
    quota.resolve({ rate_limit_reset_credits: { available_count: 99 } })
    await flushPromises()
    expect(wrapper.get('.usage-fixture').text()).toBe('60')
    expect(wrapper.text()).not.toContain('admin.accounts.usageWindow.resetCredits 99')
    wrapper.unmount()
  })
  it('does not refresh another Grok account when the old probe finishes', async () => {
    const probe = deferred<Record<string, never>>()
    queryGrokQuota.mockReturnValueOnce(probe.promise)
    getUsage.mockResolvedValueOnce({ grok_request_quota: { limit: 100, remaining: 80 } })
      .mockResolvedValue({ grok_request_quota: { limit: 100, remaining: 30 } })
    const wrapper = mount(AccountUsageCell, {
      props: { account: makeAccount({ id: 99204, platform: 'grok', type: 'oauth' }) },
      global: { stubs: { UsageProgressBar: { props: ['utilization'], template: '<div class="usage-fixture">{{utilization}}</div>' }, AccountQuotaInfo: true } }
    })
    await flushPromises()
    await wrapper.findAll('button').find(item => item.text().includes('activeQuery'))!.trigger('click')
    await wrapper.setProps({ account: makeAccount({ id: 99205, platform: 'grok', type: 'oauth' }) })
    await flushPromises()
    probe.resolve({}); await flushPromises()
    expect(getUsage).toHaveBeenCalledTimes(2)
    expect(wrapper.get('.usage-fixture').text()).toBe('30')
    wrapper.unmount()
  })
  const codexUsage = (value: number) => ({ ...usage(value), codex_reset_credits_supported: true, codex_reset_credits_available_count: 1 })
  const mountCodex = (id: number) => mount(AccountUsageCell, {
    props: { account: makeAccount({ id, platform: 'openai', type: 'oauth' }) },
    global: { stubs: { UsageProgressBar: { props: ['utilization'], template: '<div class="usage-fixture">{{utilization}}</div>' }, AccountQuotaInfo: true, Teleport: true } }
  })
  it.each(['reset', 'auto-reset'] as const)('discards a late Codex %s result after switching accounts', async (action) => {
    const response = deferred<ReturnType<typeof codexUsage>>()
    const id = action === 'reset' ? 99206 : 99208
    getUsage.mockResolvedValue(codexUsage(10))
    resetOpenAIQuota.mockReturnValueOnce(response.promise)
    updateCodexAutoResetMode.mockReturnValueOnce(response.promise)
    const wrapper = mountCodex(id); await flushPromises()
    if (action === 'reset') {
      const button = wrapper.findAll('button').find(item => item.text().trim() === '重置')!
      await button.trigger('click'); await button.trigger('click')
      expect(resetOpenAIQuota).toHaveBeenCalledWith(id)
    } else {
      await wrapper.findAll('[role="radio"]')[1]!.trigger('click')
      expect(updateCodexAutoResetMode).toHaveBeenCalledWith(id, 'short')
    }
    getUsage.mockResolvedValue(codexUsage(60))
    await wrapper.setProps({ account: makeAccount({ id: id + 1, platform: 'openai', type: 'oauth' }) })
    await flushPromises()
    response.resolve(codexUsage(99)); await flushPromises()
    expect(wrapper.get('.usage-fixture').text()).toBe('60')
    expect(queryOpenAIQuota).not.toHaveBeenCalled()
    wrapper.unmount()
  })
  it.each(['reset', 'auto-reset'] as const)('preserves successful same-account Codex %s actions', async (action) => {
    const id = action === 'reset' ? 99210 : 99211
    getUsage.mockResolvedValue(codexUsage(10))
    resetOpenAIQuota.mockResolvedValueOnce(codexUsage(25))
    updateCodexAutoResetMode.mockResolvedValueOnce({ ...codexUsage(25), codex_auto_reset_mode: 'short' })
    const wrapper = mountCodex(id); await flushPromises()
    if (action === 'reset') {
      const button = wrapper.findAll('button').find(item => item.text().trim() === '重置')!
      await button.trigger('click'); await button.trigger('click'); await flushPromises()
      expect(resetOpenAIQuota).toHaveBeenCalledOnce()
      expect(resetOpenAIQuota).toHaveBeenCalledWith(id)
      expect(queryOpenAIQuota).toHaveBeenCalledWith(id)
    } else {
      await wrapper.findAll('[role="radio"]')[1]!.trigger('click'); await flushPromises()
      expect(updateCodexAutoResetMode).toHaveBeenCalledOnce()
      expect(updateCodexAutoResetMode).toHaveBeenCalledWith(id, 'short')
      expect(wrapper.findAll('[role="radio"]')[1]!.attributes('aria-checked')).toBe('true')
    }
    expect(wrapper.get('.usage-fixture').text()).toBe('25')
    wrapper.unmount()
  })
  it('requires fresh reset confirmation for a replacement account', async () => {
    getUsage.mockResolvedValue(codexUsage(10))
    const wrapper = mountCodex(99212); await flushPromises()
    await wrapper.findAll('button').find(item => item.text().trim() === '重置')!.trigger('click')
    expect(wrapper.text()).toContain('确认重置？')
    await wrapper.setProps({ account: makeAccount({ id: 99213, platform: 'openai', type: 'oauth' }) }); await flushPromises()
    await wrapper.findAll('button').find(item => item.text().trim() === '重置')!.trigger('click')
    expect(resetOpenAIQuota).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('确认重置？')
    wrapper.unmount()
  })
  it('does not restore spent reset credits from an older same-account quota query', async () => {
    const oldQuota = deferred<{ rate_limit_reset_credits: { available_count: number } }>()
    getUsage.mockResolvedValue(codexUsage(10))
    queryOpenAIQuota.mockReturnValueOnce(oldQuota.promise)
      .mockResolvedValueOnce({ rate_limit_reset_credits: { available_count: 0 } })
    resetOpenAIQuota.mockResolvedValueOnce({ ...codexUsage(25), codex_reset_credits_available_count: 0 })
    const wrapper = mountCodex(99218); await flushPromises()
    await wrapper.findAll('button').find(item => item.text().includes('activeQuery'))!.trigger('click'); await flushPromises()
    const resetButton = wrapper.findAll('button').find(item => item.text().trim() === '重置')!
    await resetButton.trigger('click'); await resetButton.trigger('click'); await flushPromises()
    expect(resetOpenAIQuota).toHaveBeenCalledWith(99218)
    expect(queryOpenAIQuota).toHaveBeenCalledTimes(2)
    expect(wrapper.text()).toContain('admin.accounts.usageWindow.resetCredits 0')
    oldQuota.resolve({ rate_limit_reset_credits: { available_count: 99 } }); await flushPromises()
    expect(wrapper.text()).toContain('admin.accounts.usageWindow.resetCredits 0')
    expect(wrapper.text()).not.toContain('admin.accounts.usageWindow.resetCredits 99')
    wrapper.unmount()
  })
  it.each(['passive', 'active'] as const)('does not overwrite a successful reset with older %s usage', async (source) => {
    const oldUsage = deferred<ReturnType<typeof codexUsage>>()
    getUsage.mockResolvedValueOnce(codexUsage(10)).mockReturnValueOnce(oldUsage.promise)
    resetOpenAIQuota.mockResolvedValueOnce({ ...codexUsage(25), codex_reset_credits_available_count: 0 })
    queryOpenAIQuota.mockResolvedValue({ rate_limit_reset_credits: { available_count: 0 } })
    const wrapper = mountCodex(source === 'passive' ? 99219 : 99220); await flushPromises()
    if (source === 'passive') await wrapper.setProps({ manualRefreshToken: 1 })
    else await wrapper.findAll('button').find(item => item.text().includes('activeQuery'))!.trigger('click')
    await flushPromises()
    const resetButton = wrapper.findAll('button').find(item => item.text().trim() === '重置')!
    await resetButton.trigger('click'); await resetButton.trigger('click'); await flushPromises()
    expect(wrapper.get('.usage-fixture').text()).toBe('25')
    oldUsage.resolve(codexUsage(99)); await flushPromises()
    expect(wrapper.get('.usage-fixture').text()).toBe('25')
    expect(wrapper.text()).toContain('admin.accounts.usageWindow.resetCredits 0')
    expect(wrapper.findAll('button').find(item => item.text().includes('activeQuery'))!.attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })
  it('does not overwrite a saved auto-reset mode with older passive usage', async () => {
    const oldUsage = deferred<ReturnType<typeof codexUsage>>()
    getUsage.mockResolvedValueOnce(codexUsage(10)).mockReturnValueOnce(oldUsage.promise)
    updateCodexAutoResetMode.mockResolvedValueOnce({ ...codexUsage(25), codex_auto_reset_mode: 'short' })
    const wrapper = mountCodex(99221); await flushPromises()
    await wrapper.setProps({ manualRefreshToken: 1 }); await flushPromises()
    await wrapper.findAll('[role="radio"]')[1]!.trigger('click'); await flushPromises()
    oldUsage.resolve(codexUsage(99)); await flushPromises()
    expect(wrapper.get('.usage-fixture').text()).toBe('25')
    expect(wrapper.findAll('[role="radio"]')[1]!.attributes('aria-checked')).toBe('true')
    wrapper.unmount()
  })
  it('discards a quota query even after switching back to the original account', async () => {
    const quota = deferred<{ rate_limit_reset_credits: { available_count: number } }>()
    getUsage.mockResolvedValue(codexUsage(10)); queryOpenAIQuota.mockReturnValueOnce(quota.promise)
    const wrapper = mountCodex(99214); await flushPromises()
    await wrapper.findAll('button').find(item => item.text().includes('activeQuery'))!.trigger('click'); await flushPromises()
    await wrapper.setProps({ account: makeAccount({ id: 99215, platform: 'openai', type: 'oauth' }) }); await flushPromises()
    await wrapper.setProps({ account: makeAccount({ id: 99214, platform: 'openai', type: 'oauth' }) }); await flushPromises()
    quota.resolve({ rate_limit_reset_credits: { available_count: 99 } }); await flushPromises()
    expect(wrapper.text()).toContain('admin.accounts.usageWindow.resetCredits 1')
    expect(wrapper.text()).not.toContain('admin.accounts.usageWindow.resetCredits 99')
    wrapper.unmount()
  })
  it('keeps an old invite response out of the replacement account dialog', async () => {
    const invite = deferred<{ sent: boolean; message: string }>()
    getUsage.mockResolvedValue(codexUsage(10)); sendOpenAICodexInvite.mockReturnValueOnce(invite.promise)
    const wrapper = mountCodex(99216); await flushPromises()
    await wrapper.findAll('button').find(item => item.text().trim() === '邀请')!.trigger('click'); await flushPromises()
    await wrapper.get('input[type="email"]').setValue('first@example.com')
    await wrapper.findAll('button').find(item => item.text().trim() === '发送')!.trigger('click')
    expect(sendOpenAICodexInvite).toHaveBeenCalledWith(99216, 'first@example.com')
    await wrapper.setProps({ account: makeAccount({ id: 99217, platform: 'openai', type: 'oauth' }) }); await flushPromises()
    expect(wrapper.find('input[type="email"]').exists()).toBe(false)
    await wrapper.findAll('button').find(item => item.text().trim() === '邀请')!.trigger('click'); await flushPromises()
    await wrapper.get('input[type="email"]').setValue('second@example.com')
    invite.resolve({ sent: true, message: 'previous-account-invite' }); await flushPromises()
    expect(wrapper.text()).not.toContain('previous-account-invite')
    expect((wrapper.get('input[type="email"]').element as HTMLInputElement).value).toBe('second@example.com')
    wrapper.unmount()
  })
  it('ignores a response belonging to a replaced account', async () => {
    const old = deferred<ReturnType<typeof usage>>()
    getUsage.mockReturnValueOnce(old.promise).mockResolvedValueOnce(usage(60))
    const wrapper = mountUsage(99102); await flushPromises()
    await wrapper.setProps({ account: makeAccount({ id: 99103, platform: 'anthropic', type: 'oauth' }) }); await flushPromises()
    old.resolve(usage(99)); await flushPromises(); expect(wrapper.find('.usage-fixture').text()).toBe('60'); wrapper.unmount()
  })
  it('does not publish a pending result after unmount', async () => {
    const old = deferred<ReturnType<typeof usage>>()
    getUsage.mockReturnValueOnce(old.promise)
    const wrapper = mountUsage(99104); await flushPromises(); wrapper.unmount(); old.resolve(usage(99)); await flushPromises()
    getUsage.mockResolvedValueOnce(usage(40)); const next = mountUsage(99104); await flushPromises()
    expect(next.find('.usage-fixture').text()).toBe('40'); next.unmount()
  })
})
