import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const { get, post, patch, remove, credentialGet, credentialCreate } = vi.hoisted(() => ({
  get: vi.fn(), post: vi.fn(), patch: vi.fn(), remove: vi.fn(),
  credentialGet: vi.fn(), credentialCreate: vi.fn()
}))

vi.mock('@/api/client', () => ({
  apiClient: { get, post, patch, delete: remove }
}))

import { passkeyAPI } from '@/api/passkey'

class FakePublicKeyCredential {
  id = 'credential-id'
  rawId = Uint8Array.from([1, 2, 3]).buffer
  type = 'public-key'
  authenticatorAttachment = 'platform'
  response: Record<string, unknown> = {
    authenticatorData: Uint8Array.from([4, 5]).buffer,
    clientDataJSON: Uint8Array.from([6, 7]).buffer,
    signature: Uint8Array.from([8, 9]).buffer,
    userHandle: Uint8Array.from([10, 11]).buffer
  }
  getClientExtensionResults(): AuthenticationExtensionsClientOutputs { return {} }
}

class FakeRegistrationCredential extends FakePublicKeyCredential {
  constructor() {
    super()
    this.response = {
      attestationObject: Uint8Array.from([12, 13]).buffer,
      clientDataJSON: Uint8Array.from([6, 7]).buffer,
      getTransports: () => ['internal']
    }
  }
}

describe('passkey API', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.stubGlobal('PublicKeyCredential', FakePublicKeyCredential)
    Object.defineProperty(window, 'PublicKeyCredential', { configurable: true, value: FakePublicKeyCredential })
    Object.defineProperty(navigator, 'credentials', {
      configurable: true,
      value: { get: credentialGet, create: credentialCreate }
    })
  })

  afterEach(() => { vi.unstubAllGlobals() })

  it('converts login challenge and assertion bytes with base64url encoding', async () => {
    post
      .mockResolvedValueOnce({ data: { session_token: 'one-time', options: { publicKey: { challenge: 'AQID', userVerification: 'required' } } } })
      .mockResolvedValueOnce({ data: { access_token: 'access', token_type: 'Bearer', user: { id: 1 } } })
    credentialGet.mockResolvedValue(new FakePublicKeyCredential())

    await passkeyAPI.login()

    const options = credentialGet.mock.calls[0][0] as CredentialRequestOptions
    expect(Array.from(new Uint8Array(options.publicKey!.challenge))).toEqual([1, 2, 3])
    expect(post).toHaveBeenNthCalledWith(2, '/auth/passkey/login/finish', {
      session_token: 'one-time',
      credential: {
        id: 'credential-id', rawId: 'AQID', type: 'public-key', authenticatorAttachment: 'platform', clientExtensionResults: {},
        response: { authenticatorData: 'BAU', clientDataJSON: 'Bgc', signature: 'CAk', userHandle: 'Cgs' }
      }
    })
  })

  it('sends the account password only to registration begin and deletion', async () => {
    post
      .mockResolvedValueOnce({ data: { session_token: 'register', options: { publicKey: { challenge: 'AQID', user: { id: 'BAU', name: 'u@example.com', displayName: 'u' } } } } })
      .mockResolvedValueOnce({ data: { id: 3, name: 'Laptop', created_at: '2026-07-28T00:00:00Z', backup: false } })
    credentialCreate.mockResolvedValue(new FakeRegistrationCredential())

    await passkeyAPI.register('Laptop', 'secret')
    expect(post).toHaveBeenNthCalledWith(1, '/user/passkeys/register/begin', { password: 'secret' })
    expect(post.mock.calls[1][1]).not.toHaveProperty('password')

    remove.mockResolvedValue({ data: null })
    await passkeyAPI.remove(3, 'secret')
    expect(remove).toHaveBeenCalledWith('/user/passkeys/3', { data: { password: 'secret' } })
  })

  it('does not make API calls when WebAuthn is unsupported', async () => {
    Object.defineProperty(window, 'PublicKeyCredential', { configurable: true, value: undefined })
    expect(passkeyAPI.isSupported()).toBe(false)
    await expect(passkeyAPI.login()).rejects.toThrow()
    expect(post).not.toHaveBeenCalled()
    expect(credentialGet).not.toHaveBeenCalled()
  })
})
