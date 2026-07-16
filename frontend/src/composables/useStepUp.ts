import { ref } from 'vue'

const required = 'STEP_UP_REQUIRED'
const blocked = new Set(['STEP_UP_TOTP_NOT_ENABLED', 'STEP_UP_ADMIN_API_KEY_FORBIDDEN'])

export class StepUpCancelledError extends Error {
  readonly code = 'STEP_UP_CANCELLED'
  constructor() { super('step-up verification cancelled'); this.name = 'StepUpCancelledError' }
}

function marker(error: unknown): string {
  const value = (error || {}) as { code?: unknown; reason?: unknown }
  for (const candidate of [value.code, value.reason]) {
    if (typeof candidate === 'string' && candidate.startsWith('STEP_UP')) return candidate
  }
  return ''
}

export const isStepUpRequired = (error: unknown) => marker(error) === required
export const isStepUpBlocked = (error: unknown) => blocked.has(marker(error))
export const isStepUpCancelled = (error: unknown) => error instanceof StepUpCancelledError
export const stepUpBlockReason = marker
export type StepUpController = ReturnType<typeof useStepUp>

export function useStepUp() {
  const visible = ref(false)
  let resolver: ((verified: boolean) => void) | null = null
  const prompt = () => { visible.value = true; return new Promise<boolean>((resolve) => { resolver = resolve }) }
  const finish = (verified: boolean) => { visible.value = false; resolver?.(verified); resolver = null }
  async function run<T>(action: () => Promise<T>): Promise<T> {
    try { return await action() } catch (error) {
      if (isStepUpBlocked(error) || !isStepUpRequired(error)) throw error
      if (!await prompt()) throw new StepUpCancelledError()
      return action()
    }
  }
  return { visible, prompt, onVerified: () => finish(true), onCancel: () => finish(false), run }
}
