export type OpenCodeMode = 'go' | 'zen'
// Fallback catalog from the selected upstream v0.2.5 revision. Account mappings
// and live model discovery remain authoritative.
export const openCodeModels = [
  'grok-4.6', 'gpt-5.6-luna', 'glm-5.3-flash', 'glm-5.3', 'glm-5.2', 'glm-5.1',
  'kimi-k3', 'kimi-k2.7-code', 'kimi-k2.6', 'longcat-2.0',
  'deepseek-v4-pro', 'deepseek-v4-flash', 'deepseek-v4-flash-vision-exp',
  'mimo-v2.5', 'mimo-v2.5-pro', 'minimax-m3', 'minimax-m2.7', 'minimax-m2.5',
  'muse-spark-1.3-contributor', 'muse-spark-1.2-contributor',
  'qwen3.8-max', 'qwen3.8-flash', 'qwen3.7-max', 'qwen3.7-plus', 'qwen3.6-plus',
  'hy4-preview', 'hy3', 'omen-alpha'
]
export interface OpenCodeRule { pattern: string; protocol: 'responses' | 'anthropic' | 'chat_completions' }

export function openCodeBaseURL(mode: OpenCodeMode, protocol = 'chat_completions'): string {
  return `https://opencode.ai/zen${mode === 'go' ? '/go' : ''}/${protocol === 'anthropic' ? 'anthropic' : 'v1'}`
}

export function defaultOpenCodeRules(mode: OpenCodeMode): OpenCodeRule[] {
  return [
    { pattern: 'grok-*', protocol: 'responses' },
    { pattern: 'gpt-*', protocol: 'responses' },
    { pattern: 'muse-spark-*', protocol: 'responses' },
    { pattern: 'qwen*', protocol: 'anthropic' },
    { pattern: mode === 'zen' ? 'claude-*' : 'minimax-*', protocol: 'anthropic' }
  ]
}

export function validOpenCodeRules(rules: OpenCodeRule[] | null): boolean {
  return rules === null || (rules.length <= 64 && rules.every(rule =>
    rule.pattern.trim().length > 0 && rule.pattern.trim().length <= 128 &&
    /^[^\s*]+\*?$|^\*$/.test(rule.pattern.trim()) &&
    ['responses', 'anthropic', 'chat_completions'].includes(rule.protocol)))
}
