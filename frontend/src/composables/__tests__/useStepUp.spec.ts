import { describe, expect, it, vi } from 'vitest'
import { effectScope } from 'vue'
import { isStepUpBlocked, isStepUpCancelled, isStepUpRequired, StepUpCancelledError, useStepUp } from '../useStepUp'

describe('useStepUp', () => {
  it('shares verification and settles every waiter on cancel', async () => {
    const controller = useStepUp()
    const first = controller.prompt()
    const second = controller.prompt()
    expect(second).toBe(first)
    controller.onCancel()
    await expect(first).resolves.toBe(false)
    await expect(second).resolves.toBe(false)
    const next = controller.prompt()
    expect(next).not.toBe(first)
    controller.onVerified()
    await expect(next).resolves.toBe(true)
  })

  it('does not open a prompt after an in-flight action outlives its scope', async () => {
    const scope = effectScope()
    const controller = scope.run(() => useStepUp())!
    let reject!: (error: unknown) => void
    const action = vi.fn(() => new Promise<never>((_, fail) => { reject = fail }))
    const result = controller.run(action)
    const assertion = expect(result).rejects.toBeInstanceOf(StepUpCancelledError)
    scope.stop()
    reject({ code: 'STEP_UP_REQUIRED' })
    await assertion
    expect(controller.visible.value).toBe(false)
    expect(action).toHaveBeenCalledTimes(1)
  })
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
