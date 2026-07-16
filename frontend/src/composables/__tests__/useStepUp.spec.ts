import { describe, expect, it, vi } from 'vitest'
import { isStepUpBlocked, isStepUpCancelled, isStepUpRequired, StepUpCancelledError, useStepUp } from '../useStepUp'

describe('useStepUp', () => {
  it('classifies backend markers', () => {
    expect(isStepUpRequired({ code: 'STEP_UP_REQUIRED' })).toBe(true)
    expect(isStepUpRequired({ reason: 'STEP_UP_REQUIRED' })).toBe(true)
    expect(isStepUpBlocked({ code: 'STEP_UP_TOTP_NOT_ENABLED' })).toBe(true)
    expect(isStepUpBlocked({ reason: 'STEP_UP_ADMIN_API_KEY_FORBIDDEN' })).toBe(true)
  })

  it('prompts and retries once after verification', async () => {
    const controller = useStepUp()
    let calls = 0
    const action = async () => { calls++; if (calls === 1) throw { code: 'STEP_UP_REQUIRED' }; return 'ok' }
    const result = controller.run(action)
    await vi.waitFor(() => expect(controller.visible.value).toBe(true))
    controller.onVerified()
    await expect(result).resolves.toBe('ok')
    expect(calls).toBe(2)
  })

  it('uses a distinct cancellation error', async () => {
    const controller = useStepUp()
    const result = controller.run(async () => { throw { code: 'STEP_UP_REQUIRED' } })
    await vi.waitFor(() => expect(controller.visible.value).toBe(true))
    controller.onCancel()
    await expect(result).rejects.toBeInstanceOf(StepUpCancelledError)
    try { await result } catch (error) { expect(isStepUpCancelled(error)).toBe(true) }
  })
})
