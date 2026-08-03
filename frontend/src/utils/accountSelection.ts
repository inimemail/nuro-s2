export interface AccountSelectionFilters {
  platform?: string
  type?: string
  status?: string
  group?: string
  search?: string
  privacy_mode?: string
  pool_mode?: string
  sort_by?: string
  sort_order?: 'asc' | 'desc'
}

export interface AccountSelectionSnapshot {
  mode: 'ids' | 'filtered'
  accountIds: number[]
  filters?: AccountSelectionFilters
  total?: number
}

interface AccountIDRow {
  id: number
}

interface AccountListPage {
  items: AccountIDRow[]
  total: number
  pages?: number
}

type AccountPageFetcher = (
  page: number,
  pageSize: number,
  filters: Record<string, unknown>
) => Promise<AccountListPage>

const SELECT_ALL_PAGE_SIZE = 1000

export async function fetchAllAccountIds(
  fetchPage: AccountPageFetcher,
  filters: object
): Promise<number[]> {
  const requestFilters = {
    ...filters,
    lite: '1',
    include_scheduler_score: '0'
  }
  const firstPage = await fetchPage(1, SELECT_ALL_PAGE_SIZE, requestFilters)
  const pageCount = Math.max(
    firstPage.pages ?? 0,
    Math.ceil(firstPage.total / SELECT_ALL_PAGE_SIZE)
  )
  const ids = firstPage.items.map((account) => account.id)

  for (let page = 2; page <= pageCount; page += 1) {
    const result = await fetchPage(page, SELECT_ALL_PAGE_SIZE, requestFilters)
    ids.push(...result.items.map((account) => account.id))
  }

  const uniqueIds = [...new Set(ids.filter((id) => Number.isInteger(id) && id > 0))]
  if (uniqueIds.length !== firstPage.total) {
    throw new Error('Account list changed while selecting all results')
  }
  return uniqueIds
}

export function normalizeAccountSelectionFilters(filters: AccountSelectionFilters): AccountSelectionFilters {
  const normalized: AccountSelectionFilters = {}
  for (const [key, value] of Object.entries(filters)) {
    if (typeof value !== 'string') {
      if (key === 'sort_order' && (value === 'asc' || value === 'desc')) normalized.sort_order = value
      continue
    }
    const trimmed = value.trim()
    if (trimmed) normalized[key as keyof AccountSelectionFilters] = trimmed as never
  }
  if (normalized.sort_order !== 'desc') normalized.sort_order = 'asc'
  return normalized
}

export function createAccountSelectionSnapshot(
  accountIds: number[],
  filters?: AccountSelectionFilters,
  total?: number
): AccountSelectionSnapshot {
  const uniqueIds = [...new Set(accountIds.filter((id) => Number.isInteger(id) && id > 0))]
  if (filters) {
    return {
      mode: 'filtered',
      accountIds: uniqueIds,
      filters: normalizeAccountSelectionFilters(filters),
      total: Number.isFinite(total) ? Math.max(0, Math.trunc(total as number)) : undefined
    }
  }
  return { mode: 'ids', accountIds: uniqueIds }
}

export function chunkAccountIds(ids: number[], chunkSize = 8): number[][] {
  const normalized = [...new Set(ids.filter((id) => Number.isInteger(id) && id > 0))]
  const size = Math.max(1, Math.trunc(chunkSize))
  const chunks: number[][] = []
  for (let index = 0; index < normalized.length; index += size) {
    chunks.push(normalized.slice(index, index + size))
  }
  return chunks
}
