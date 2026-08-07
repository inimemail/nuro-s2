import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import axios from 'axios'

vi.mock('axios', () => ({ default: { post: vi.fn() } }))

const mockedPost = vi.mocked(axios.post)

function seedSession(): void {
  localStorage.setItem('auth_token', 'old-access')
  localStorage.setItem('refresh_token', 'old-refresh')
  localStorage.setItem('token_expires_at', String(Date.now() - 1))
  localStorage.setItem('auth_user', JSON.stringify({ id: 7, email: 'admin@example.com' }))
}

function refreshedResponse() {
  return {
    data: {
      code: 0,
      message: 'ok',
      data: { access_token: 'new-access', refresh_token: 'new-refresh', expires_in: 3600, token_type: 'Bearer' }
    }
  }
}

describe('refreshAuthTokens', () => {
  beforeEach(() => {
    localStorage.clear()
    mockedPost.mockReset()
    vi.resetModules()
    Object.defineProperty(navigator, 'locks', { configurable: true, value: undefined })
  })

  afterEach(() => vi.useRealTimers())

  it('shares one refresh request between concurrent callers in one document', async () => {
    seedSession()
    let resolveRequest!: (value: ReturnType<typeof refreshedResponse>) => void
    mockedPost.mockImplementationOnce(() => new Promise((resolve) => { resolveRequest = resolve }))
    const { refreshAuthTokens } = await import('@/api/tokenRefresh')

    const first = refreshAuthTokens({ failedAccessToken: 'old-access' })
    const second = refreshAuthTokens({ failedAccessToken: 'old-access' })
    expect(mockedPost).toHaveBeenCalledTimes(1)
    resolveRequest(refreshedResponse())

    await expect(first).resolves.toMatchObject({ access_token: 'new-access' })
    await expect(second).resolves.toMatchObject({ refresh_token: 'new-refresh' })
  })

  it('adopts a token rotated by another tab after acquiring the Web Lock', async () => {
    seedSession()
    const request = vi.fn(async (_name: string, callback: () => Promise<unknown>) => {
      localStorage.setItem('auth_token', 'peer-access')
      localStorage.setItem('token_expires_at', String(Date.now() + 3600_000))
      localStorage.setItem('refresh_token', 'peer-refresh')
      return callback()
    })
    Object.defineProperty(navigator, 'locks', { configurable: true, value: { request } })
    const { refreshAuthTokens } = await import('@/api/tokenRefresh')

    await expect(refreshAuthTokens({ failedAccessToken: 'old-access' })).resolves.toMatchObject({
      access_token: 'peer-access', refresh_token: 'peer-refresh'
    })
    expect(mockedPost).not.toHaveBeenCalled()
  })

  it('does not adopt a token from another signed-in user', async () => {
    vi.useFakeTimers()
    seedSession()
    mockedPost.mockRejectedValueOnce(new Error('refresh token already used'))
    const { refreshAuthTokens } = await import('@/api/tokenRefresh')
    window.setTimeout(() => {
      localStorage.setItem('auth_user', JSON.stringify({ id: 8 }))
      localStorage.setItem('auth_token', 'other-access')
      localStorage.setItem('token_expires_at', String(Date.now() + 3600_000))
      localStorage.setItem('refresh_token', 'other-refresh')
    }, 10)

    const rejection = expect(refreshAuthTokens({ failedAccessToken: 'old-access' })).rejects.toThrow('refresh token already used')
    await vi.advanceTimersByTimeAsync(1_100)
    await rejection
  })

  it('does not restore a session logged out while refresh is in flight', async () => {
    vi.useFakeTimers()
    seedSession()
    let resolveRequest!: (value: ReturnType<typeof refreshedResponse>) => void
    mockedPost.mockImplementationOnce(() => new Promise((resolve) => { resolveRequest = resolve }))
    const { refreshAuthTokens } = await import('@/api/tokenRefresh')

    const pending = refreshAuthTokens({ failedAccessToken: 'old-access' })
    localStorage.clear()
    resolveRequest(refreshedResponse())
    const rejection = expect(pending).rejects.toThrow('Session changed during token refresh')
    await vi.advanceTimersByTimeAsync(1_100)
    await rejection
    expect(localStorage.getItem('auth_token')).toBeNull()
  })
})
