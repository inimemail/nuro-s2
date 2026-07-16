import { describe, it, expect } from 'vitest'
import {
	applyGrokOAuthBaseURL,
  applyHeaderOverride,
  applyInterceptWarmup,
  buildHeaderOverridesObject,
  getHeaderOverrideTemplate,
	isHeaderOverrideCapable,
  isHeaderOverridePlatform,
	isValidHTTPBaseURL,
  splitHeaderOverridesObject,
  validateHeaderOverrideRows
} from '../credentialsBuilder'

describe('applyInterceptWarmup', () => {
  it('create + enabled=true: should set intercept_warmup_requests to true', () => {
    const creds: Record<string, unknown> = { access_token: 'tok' }
    applyInterceptWarmup(creds, true, 'create')
    expect(creds.intercept_warmup_requests).toBe(true)
  })

  it('create + enabled=false: should not add the field', () => {
    const creds: Record<string, unknown> = { access_token: 'tok' }
    applyInterceptWarmup(creds, false, 'create')
    expect('intercept_warmup_requests' in creds).toBe(false)
  })

  it('edit + enabled=true: should set intercept_warmup_requests to true', () => {
    const creds: Record<string, unknown> = { api_key: 'sk' }
    applyInterceptWarmup(creds, true, 'edit')
    expect(creds.intercept_warmup_requests).toBe(true)
  })

  it('edit + enabled=false + field exists: should delete the field', () => {
    const creds: Record<string, unknown> = { api_key: 'sk', intercept_warmup_requests: true }
    applyInterceptWarmup(creds, false, 'edit')
    expect('intercept_warmup_requests' in creds).toBe(false)
  })

  it('edit + enabled=false + field absent: should not throw', () => {
    const creds: Record<string, unknown> = { api_key: 'sk' }
    applyInterceptWarmup(creds, false, 'edit')
    expect('intercept_warmup_requests' in creds).toBe(false)
  })

  it('should not affect other fields', () => {
    const creds: Record<string, unknown> = {
      api_key: 'sk',
      base_url: 'url',
      intercept_warmup_requests: true
    }
    applyInterceptWarmup(creds, false, 'edit')
    expect(creds.api_key).toBe('sk')
    expect(creds.base_url).toBe('url')
    expect('intercept_warmup_requests' in creds).toBe(false)
  })
})

describe('header override credentials helpers', () => {
  it('detects supported platforms only', () => {
    expect(isHeaderOverridePlatform('anthropic')).toBe(true)
    expect(isHeaderOverridePlatform('openai')).toBe(true)
    expect(isHeaderOverridePlatform('grok')).toBe(true)
    expect(isHeaderOverridePlatform('gemini')).toBe(false)
  })

	it('matches backend eligibility for platform and account type', () => {
		expect(isHeaderOverrideCapable('anthropic', 'apikey')).toBe(true)
		expect(isHeaderOverrideCapable('anthropic', 'oauth')).toBe(false)
		expect(isHeaderOverrideCapable('openai', 'apikey')).toBe(true)
		expect(isHeaderOverrideCapable('openai', 'oauth')).toBe(false)
		expect(isHeaderOverrideCapable('grok', 'apikey')).toBe(true)
		expect(isHeaderOverrideCapable('grok', 'oauth')).toBe(true)
		expect(isHeaderOverrideCapable('gemini', 'apikey')).toBe(false)
	})

	it('validates and applies Grok OAuth base URLs with exact off semantics', () => {
		expect(isValidHTTPBaseURL('https://relay.example.com/v1')).toBe(true)
		expect(isValidHTTPBaseURL('http://127.0.0.1:8080')).toBe(true)
		expect(isValidHTTPBaseURL('relay.example.com/v1')).toBe(false)
		expect(isValidHTTPBaseURL('ftp://relay.example.com')).toBe(false)

		const created: Record<string, unknown> = {}
		applyGrokOAuthBaseURL(created, false, '', 'create')
		expect(created).toEqual({})
		applyGrokOAuthBaseURL(created, true, ' https://relay.example.com/v1 ', 'create')
		expect(created.base_url).toBe('https://relay.example.com/v1')

		const edited: Record<string, unknown> = { base_url: 'https://old.example.com/v1', access_token: 'token' }
		applyGrokOAuthBaseURL(edited, false, '', 'edit')
		expect(edited).toEqual({ access_token: 'token' })
	})

  it('builds normalized header override objects', () => {
    expect(
      buildHeaderOverridesObject([
        { name: ' User-Agent ', value: ' codex-cli ' },
        { name: '', value: 'ignored' },
        { name: 'Anthropic-Beta', value: 'context-management-2025-06-27' }
      ])
    ).toEqual({
      'user-agent': 'codex-cli',
      'anthropic-beta': 'context-management-2025-06-27'
    })
  })

  it('splits stored objects into sorted rows and ignores non-string values', () => {
    expect(splitHeaderOverridesObject({
      'x-app': 'cli',
      'user-agent': 'codex',
      invalid: 123
    })).toEqual([
      { name: 'user-agent', value: 'codex' },
      { name: 'x-app', value: 'cli' }
    ])
  })

	it('validates blocked, duplicate, invalid name and invalid value cases', () => {
    expect(validateHeaderOverrideRows([{ name: 'authorization', value: 'Bearer x' }])).toBe('blockedName')
    expect(validateHeaderOverrideRows([
      { name: 'User-Agent', value: 'a' },
      { name: 'user-agent', value: 'b' }
    ])).toBe('duplicateName')
    expect(validateHeaderOverrideRows([{ name: 'bad header', value: 'x' }])).toBe('invalidName')
    expect(validateHeaderOverrideRows([{ name: 'x-app', value: 'bad\nvalue' }])).toBe('invalidValue')
		expect(validateHeaderOverrideRows([{ name: 'x-grok-conv-id', value: 'static-session' }])).toBe('blockedName')
  })

  it('applies header override create/edit semantics', () => {
    const createCreds: Record<string, unknown> = {}
    applyHeaderOverride(createCreds, true, [{ name: 'User-Agent', value: 'codex' }], 'create')
    expect(createCreds).toEqual({
      header_override_enabled: true,
      header_overrides: { 'user-agent': 'codex' }
    })

    const disabledCreate: Record<string, unknown> = {}
    applyHeaderOverride(disabledCreate, false, [], 'create')
    expect(disabledCreate).toEqual({})

    const editCreds: Record<string, unknown> = {
      header_override_enabled: true,
      header_overrides: { 'user-agent': 'old' },
      api_key: 'sk'
    }
    applyHeaderOverride(editCreds, false, [], 'edit')
    expect(editCreds).toEqual({ api_key: 'sk' })
  })

  it('provides platform-specific templates', () => {
    expect(getHeaderOverrideTemplate('openai').map((row) => row.name)).toContain('openai-beta')
    expect(getHeaderOverrideTemplate('anthropic').map((row) => row.name)).toContain('anthropic-beta')
    expect(getHeaderOverrideTemplate('grok').map((row) => row.name)).toContain('x-stainless-package-version')
    expect(getHeaderOverrideTemplate('grok').map((row) => row.name)).not.toContain('anthropic-beta')
  })
})
