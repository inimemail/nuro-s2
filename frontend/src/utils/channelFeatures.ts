export function channelFeatureBoolean(value: unknown): boolean {
  return value === true || value === 1 || value === '1' || value === 'true'
}

/** Editing another platform must not reset an existing Anthropic setting. */
export function channelBedrockCompatibility(
  existing: Record<string, unknown>,
  sections: Array<{ enabled: boolean; platform: string; bedrock_cc_compat: boolean }>
): Record<string, unknown> {
  const anthropic = sections.find(section => section.enabled && section.platform === 'anthropic')
  return anthropic ? { ...existing, bedrock_cc_compat: anthropic.bedrock_cc_compat === true } : { ...existing }
}
