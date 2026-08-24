import type { BillingMode, PricingInterval, ChannelTimePricing } from '@/api/admin/channels'
export type { ChannelTimePricing }

export interface IntervalFormEntry {
  min_tokens: number
  max_tokens: number | null
  tier_label: string
  input_price: number | string | null
  output_price: number | string | null
  cache_write_price: number | string | null
  cache_read_price: number | string | null
  input_multiplier: number | string | null
  output_multiplier: number | string | null
  cache_write_multiplier: number | string | null
  cache_read_multiplier: number | string | null
  per_request_price: number | string | null
  sort_order: number
}

export interface PricingFormEntry {
  models: string[]
  billing_mode: BillingMode
  input_price: number | string | null
  output_price: number | string | null
  cache_write_price: number | string | null
  cache_read_price: number | string | null
  fast_multiplier?: number | string | null
  flex_multiplier?: number | string | null
  image_input_price: number | string | null
  image_output_price: number | string | null
  per_request_price: number | string | null
  intervals: IntervalFormEntry[]
  time_pricing?: ChannelTimePricing | null
}

// 价格转换：后端存 per-token，前端显示 per-MTok ($/1M tokens)
const MTOK = 1_000_000

export function toNullableNumber(val: number | string | null | undefined): number | null {
  if (val === null || val === undefined || val === '') return null
  const num = Number(val)
  return isNaN(num) ? null : num
}

/** 前端显示值($/MTok) → 后端存储值(per-token) */
export function mTokToPerToken(val: number | string | null | undefined): number | null {
  const num = toNullableNumber(val)
  return num === null ? null : parseFloat((num / MTOK).toPrecision(10))
}

/** 后端存储值(per-token) → 前端显示值($/MTok) */
export function perTokenToMTok(val: number | null | undefined): number | null {
  if (val === null || val === undefined) return null
  // toPrecision(10) 消除 IEEE 754 浮点乘法精度误差，如 5e-8 * 1e6 = 0.04999...96 → 0.05
  return parseFloat((val * MTOK).toPrecision(10))
}

export function apiIntervalsToForm(intervals: PricingInterval[]): IntervalFormEntry[] {
  return (intervals || []).map(iv => ({
    min_tokens: iv.min_tokens,
    max_tokens: iv.max_tokens,
    tier_label: iv.tier_label || '',
    input_price: perTokenToMTok(iv.input_price),
    output_price: perTokenToMTok(iv.output_price),
    cache_write_price: perTokenToMTok(iv.cache_write_price),
    cache_read_price: perTokenToMTok(iv.cache_read_price),
    input_multiplier: iv.input_multiplier,
    output_multiplier: iv.output_multiplier,
    cache_write_multiplier: iv.cache_write_multiplier,
    cache_read_multiplier: iv.cache_read_multiplier,
    per_request_price: iv.per_request_price,
    sort_order: iv.sort_order
  }))
}

export function formIntervalsToAPI(intervals: IntervalFormEntry[]): PricingInterval[] {
  return (intervals || []).map(iv => ({
    min_tokens: iv.min_tokens,
    max_tokens: iv.max_tokens,
    tier_label: iv.tier_label,
    input_price: mTokToPerToken(iv.input_price),
    output_price: mTokToPerToken(iv.output_price),
    cache_write_price: mTokToPerToken(iv.cache_write_price),
    cache_read_price: mTokToPerToken(iv.cache_read_price),
    input_multiplier: toNullableNumber(iv.input_multiplier),
    output_multiplier: toNullableNumber(iv.output_multiplier),
    cache_write_multiplier: toNullableNumber(iv.cache_write_multiplier),
    cache_read_multiplier: toNullableNumber(iv.cache_read_multiplier),
    per_request_price: toNullableNumber(iv.per_request_price),
    sort_order: iv.sort_order
  }))
}

// ── 模型模式冲突检测 ──────────────────────────────────────

interface ModelPattern {
  pattern: string
  prefix: string  // lowercase, 通配符去掉尾部 *
  wildcard: boolean
}

function toModelPattern(model: string): ModelPattern {
  const lower = model.toLowerCase()
  const wildcard = lower.endsWith('*')
  return {
    pattern: model,
    prefix: wildcard ? lower.slice(0, -1) : lower,
    wildcard,
  }
}

function patternsConflict(a: ModelPattern, b: ModelPattern): boolean {
  if (!a.wildcard && !b.wildcard) return a.prefix === b.prefix
  if (a.wildcard && !b.wildcard) return b.prefix.startsWith(a.prefix)
  if (!a.wildcard && b.wildcard) return a.prefix.startsWith(b.prefix)
  // 双通配符：任一前缀是另一前缀的前缀即冲突
  return a.prefix.startsWith(b.prefix) || b.prefix.startsWith(a.prefix)
}

/** 检测模型模式列表中的冲突，返回冲突的两个模式名；无冲突返回 null */
export function findModelConflict(models: string[]): [string, string] | null {
  const patterns = models.map(toModelPattern)
  for (let i = 0; i < patterns.length; i++) {
    for (let j = i + 1; j < patterns.length; j++) {
      if (patternsConflict(patterns[i], patterns[j])) {
        return [patterns[i].pattern, patterns[j].pattern]
      }
    }
  }
  return null
}

// ── 区间校验 ──────────────────────────────────────────────

/** 校验区间列表的合法性，返回错误消息；通过则返回 null
 *
 * mode 决定区间语义：
 * - token：区间是上下文 token 数分段 (min, max]，不能重叠，无上限段必须放最后
 * - per_request / image / video：区间是按 tier_label 分层，后端按 label
 *   匹配，不依赖 min/max，因此跳过重叠 / last-unlimited 校验
 */
export function validateIntervals(
  intervals: IntervalFormEntry[],
  mode: BillingMode = 'token',
): string | null {
  if (!intervals || intervals.length === 0) return null

  // 按 min_tokens 排序（不修改原数组）
  const sorted = [...intervals].sort((a, b) => a.min_tokens - b.min_tokens)

  for (let i = 0; i < sorted.length; i++) {
    const err = validateSingleInterval(sorted[i], i)
    if (err) return err
  }

  // 非 token 模式按 tier_label 匹配，不做 token 区间重叠校验
  if (mode !== 'token') return null
  return checkIntervalOverlap(sorted)
}

/** Validate optional timezone-aware multiplier windows before submitting a channel. */
export function validateChannelTimePricing(config: ChannelTimePricing | null | undefined): string | null {
  if (!config) return null
  const timezone = String(config.timezone || '').trim()
  if (!timezone) return '分时倍率必须填写 IANA 时区'
  try {
    new Intl.DateTimeFormat('en-US', { timeZone: timezone }).format()
  } catch {
    return `无效的 IANA 时区：${timezone}`
  }
  const parsed: Array<{ start: number; end: number }> = []
  for (let i = 0; i < (config.periods || []).length; i += 1) {
    const period = config.periods[i]
    const parse = (value: string, allowMidnightEnd: boolean) => {
      const match = /^(\d{2}):(\d{2})(?::(\d{2}))?$/.exec(String(value || '').trim())
      if (!match) return null
      const hour = Number(match[1])
      const minute = Number(match[2])
      const second = match[3] == null ? 0 : Number(match[3])
      if (allowMidnightEnd && hour === 0 && minute === 0 && second === 0) return 86400
      if (hour > 23 || minute > 59 || second > 59) return null
      return hour * 3600 + minute * 60 + second
    }
    const start = parse(period.start_time, false)
    const end = parse(period.end_time, true)
    if (start == null || end == null) return `时段 #${i + 1} 必须使用 HH:mm 或 HH:mm:ss 格式`
    if (start >= end) return `时段 #${i + 1} 的开始时间必须早于结束时间`
    if (!Number.isFinite(period.multiplier) || period.multiplier < 0.01 ||
      Math.abs(period.multiplier * 100 - Math.round(period.multiplier * 100)) > 1e-9) {
      return `时段 #${i + 1} 的倍率必须是至少 0.01 且最多两位小数`
    }
    parsed.push({ start, end })
  }
  parsed.sort((a, b) => a.start - b.start)
  for (let i = 1; i < parsed.length; i += 1) {
    if (parsed[i].start < parsed[i - 1].end) return '分时倍率时段不能重叠'
  }
  return null
}

function validateSingleInterval(iv: IntervalFormEntry, idx: number): string | null {
  if (iv.min_tokens < 0) {
    return `区间 #${idx + 1}: 最小 token 数 (${iv.min_tokens}) 不能为负数`
  }
  if (iv.max_tokens != null) {
    if (iv.max_tokens <= 0) {
      return `区间 #${idx + 1}: 最大 token 数 (${iv.max_tokens}) 必须大于 0`
    }
    if (iv.max_tokens <= iv.min_tokens) {
      return `区间 #${idx + 1}: 最大 token 数 (${iv.max_tokens}) 必须大于最小 token 数 (${iv.min_tokens})`
    }
  }
  return validateIntervalPrices(iv, idx)
}

function validateIntervalPrices(iv: IntervalFormEntry, idx: number): string | null {
  const prices: [string, number | string | null][] = [
    ['输入价格', iv.input_price],
    ['输出价格', iv.output_price],
    ['缓存写入价格', iv.cache_write_price],
    ['缓存读取价格', iv.cache_read_price],
    ['输入倍率', iv.input_multiplier],
    ['输出倍率', iv.output_multiplier],
    ['缓存写入倍率', iv.cache_write_multiplier],
    ['缓存读取倍率', iv.cache_read_multiplier],
    ['单次价格', iv.per_request_price],
  ]
  for (const [name, val] of prices) {
    if (val != null && val !== '' && Number(val) <= 0 && name.includes('倍率')) {
      return `区间 #${idx + 1}: ${name}必须大于 0`
    }
    if (val != null && val !== '' && Number(val) < 0) {
      return `区间 #${idx + 1}: ${name}不能为负数`
    }
  }
  return null
}

function checkIntervalOverlap(sorted: IntervalFormEntry[]): string | null {
  for (let i = 0; i < sorted.length; i++) {
    // 无上限区间必须是最后一个
    if (sorted[i].max_tokens == null && i < sorted.length - 1) {
      return `区间 #${i + 1}: 无上限区间（最大 token 数为空）只能是最后一个`
    }
    if (i === 0) continue
    const prev = sorted[i - 1]
    // (min, max] 语义：前一个区间上界 > 当前区间下界则重叠
    if (prev.max_tokens == null || prev.max_tokens > sorted[i].min_tokens) {
      const prevMax = prev.max_tokens == null ? '∞' : String(prev.max_tokens)
      return `区间 #${i} 和 #${i + 1} 重叠：前一个区间上界 (${prevMax}) 大于当前区间下界 (${sorted[i].min_tokens})`
    }
  }
  return null
}

/** 平台对应的模型 tag 样式（背景+文字） */
export function getPlatformTagClass(platform: string): string {
  switch (platform) {
    case 'anthropic': return 'bg-orange-100 text-orange-700 dark:bg-orange-900/30 dark:text-orange-400'
    case 'openai': return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-400'
    case 'gemini': return 'bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400'
    case 'antigravity': return 'bg-purple-100 text-purple-700 dark:bg-purple-900/30 dark:text-purple-400'
    default: return 'bg-gray-100 text-gray-700 dark:bg-gray-900/30 dark:text-gray-400'
  }
}

/** 平台对应的模型文字色（仅 text-*，用于 input/text 场景）— 与 getPlatformTagClass 同色系 */
export function getPlatformTextClass(platform: string): string {
  switch (platform) {
    case 'anthropic': return 'text-orange-700 dark:text-orange-400'
    case 'openai': return 'text-emerald-700 dark:text-emerald-400'
    case 'gemini': return 'text-blue-700 dark:text-blue-400'
    case 'antigravity': return 'text-purple-700 dark:text-purple-400'
    default: return ''
  }
}
