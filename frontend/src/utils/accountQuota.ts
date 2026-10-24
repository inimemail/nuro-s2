/** Account quota capability is independent from pool mode and auth transport. */
export function supportsAccountQuota(account: { platform?: string; type?: string } | null | undefined): boolean {
  return !!account && (account.type === 'apikey' || account.type === 'bedrock' ||
    (account.platform === 'gemini' && account.type === 'service_account'))
}
