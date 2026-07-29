import { apiClient } from './client'
import type { AuthResponse } from '@/types'

export interface PasskeyCredentialSummary {
  id: number
  name: string
  created_at: string
  last_used_at?: string
  backup: boolean
}

interface CeremonyOptionsResponse {
  session_token: string
  options: { publicKey: Record<string, unknown> }
}

function requireSupport(): void {
  if (!window.PublicKeyCredential || !navigator.credentials) {
    throw new Error('Passkeys are not supported by this browser')
  }
}

function decode(value: string): ArrayBuffer {
  const normalized = value.replace(/-/g, '+').replace(/_/g, '/')
  const binary = atob(normalized + '='.repeat((4 - (normalized.length % 4)) % 4))
  return Uint8Array.from(binary, character => character.charCodeAt(0)).buffer
}

function encode(value: ArrayBuffer | null): string | null {
  if (value === null) return null
  let binary = ''
  for (const byte of new Uint8Array(value)) binary += String.fromCharCode(byte)
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/g, '')
}

function creationOptions(value: Record<string, unknown>): PublicKeyCredentialCreationOptions {
  const options = { ...value }
  options.challenge = decode(String(options.challenge))
  const user = { ...(options.user as Record<string, unknown>) }
  user.id = decode(String(user.id))
  options.user = user
  if (Array.isArray(options.excludeCredentials)) {
    options.excludeCredentials = options.excludeCredentials.map(item => ({
      ...(item as Record<string, unknown>),
      id: decode(String((item as Record<string, unknown>).id))
    }))
  }
  return options as unknown as PublicKeyCredentialCreationOptions
}

function requestOptions(value: Record<string, unknown>): PublicKeyCredentialRequestOptions {
  const options = { ...value }
  options.challenge = decode(String(options.challenge))
  if (Array.isArray(options.allowCredentials)) {
    options.allowCredentials = options.allowCredentials.map(item => ({
      ...(item as Record<string, unknown>),
      id: decode(String((item as Record<string, unknown>).id))
    }))
  }
  return options as unknown as PublicKeyCredentialRequestOptions
}

function registrationCredential(credential: PublicKeyCredential): Record<string, unknown> {
  const response = credential.response as AuthenticatorAttestationResponse
  return {
    id: credential.id,
    rawId: encode(credential.rawId),
    type: credential.type,
    authenticatorAttachment: credential.authenticatorAttachment,
    clientExtensionResults: credential.getClientExtensionResults(),
    response: {
      attestationObject: encode(response.attestationObject),
      clientDataJSON: encode(response.clientDataJSON),
      transports: typeof response.getTransports === 'function' ? response.getTransports() : []
    }
  }
}

function assertionCredential(credential: PublicKeyCredential): Record<string, unknown> {
  const response = credential.response as AuthenticatorAssertionResponse
  return {
    id: credential.id,
    rawId: encode(credential.rawId),
    type: credential.type,
    authenticatorAttachment: credential.authenticatorAttachment,
    clientExtensionResults: credential.getClientExtensionResults(),
    response: {
      authenticatorData: encode(response.authenticatorData),
      clientDataJSON: encode(response.clientDataJSON),
      signature: encode(response.signature),
      userHandle: encode(response.userHandle)
    }
  }
}

async function login(): Promise<AuthResponse> {
  requireSupport()
  const { data: begin } = await apiClient.post<CeremonyOptionsResponse>('/auth/passkey/login/begin')
  const credential = await navigator.credentials.get({ publicKey: requestOptions(begin.options.publicKey) })
  if (!(credential instanceof PublicKeyCredential)) throw new Error('Passkey sign-in was cancelled')
  const { data } = await apiClient.post<AuthResponse>('/auth/passkey/login/finish', {
    session_token: begin.session_token,
    credential: assertionCredential(credential)
  })
  return data
}

async function register(name: string, password: string): Promise<PasskeyCredentialSummary> {
  requireSupport()
  const { data: begin } = await apiClient.post<CeremonyOptionsResponse>('/user/passkeys/register/begin', { password })
  const credential = await navigator.credentials.create({ publicKey: creationOptions(begin.options.publicKey) })
  if (!(credential instanceof PublicKeyCredential)) throw new Error('Passkey creation was cancelled')
  const { data } = await apiClient.post<PasskeyCredentialSummary>('/user/passkeys/register/finish', {
    session_token: begin.session_token,
    name,
    credential: registrationCredential(credential)
  })
  return data
}

async function list(): Promise<PasskeyCredentialSummary[]> {
  const { data } = await apiClient.get<PasskeyCredentialSummary[]>('/user/passkeys')
  return data
}

export const passkeyAPI = {
  isSupported: () => Boolean(window.PublicKeyCredential && navigator.credentials),
  login,
  register,
  list,
  rename: async (id: number, name: string) => { await apiClient.patch(`/user/passkeys/${id}`, { name }) },
  remove: async (id: number, password: string) => { await apiClient.delete(`/user/passkeys/${id}`, { data: { password } }) }
}
