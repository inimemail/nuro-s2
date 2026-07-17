import { describe, expect, it } from 'vitest'
import { promptAuditErrorMessage } from '../errors'

describe('Prompt Audit error presentation', () => {
  it('maps known validation codes without echoing server text', () => {
    const error = {
      code: 'PROMPT_AUDIT_ENDPOINT_HTTPS_REQUIRED',
      message: 'https://internal.guard.example/v1 returned Authorization: Bearer secret'
    }

    const message = promptAuditErrorMessage(error, 'zh', '操作失败')

    expect(message).toContain('HTTPS')
    expect(message).not.toContain('internal.guard.example')
    expect(message).not.toContain('Bearer')
  })

  it('uses a fixed fallback for unknown errors and HTML responses', () => {
    const error = {
      code: 'UNKNOWN_UPSTREAM_ERROR',
      message: '<html>cloud proxy failure at https://private.example</html>'
    }

    expect(promptAuditErrorMessage(error, 'en', 'Unable to complete the operation'))
      .toBe('Unable to complete the operation')
  })
})
