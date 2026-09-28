import type { ServiceErrorPage, ServiceErrorQuery } from './serviceErrors'

export function buildServiceErrorPageExport(page: ServiceErrorPage, query: ServiceErrorQuery, pageNumber: number, exportedAt = new Date()) {
  const timestamp = exportedAt.toISOString()
  const payload = {
    format: 'codex2api-service-errors',
    version: 1,
    exported_at: timestamp,
    scope: 'current_page',
    view: page.grouped ? 'grouped_latest' : 'individual',
    page_number: pageNumber,
    item_count: page.items.length,
    has_more: Boolean(page.next_cursor),
    query,
    summary_scope: 'filtered_time_range',
    summary: page.summary,
    collector: page.collector,
    // Keep the full server diagnostics, including fields not rendered by the
    // table or declared in the frontend's minimal event interface.
    items: page.items,
  }
  return {
    filename: `service-errors-page-${pageNumber}-${timestamp.replace(/[:.]/g, '-')}.json`,
    blob: new Blob([JSON.stringify(payload, null, 2)], { type: 'application/json;charset=utf-8' }),
  }
}

export function saveServiceErrorPageExport(file: ReturnType<typeof buildServiceErrorPageExport>) {
  const anchor = document.createElement('a')
  const url = URL.createObjectURL(file.blob)
  try {
    anchor.href = url
    anchor.download = file.filename
    document.body.appendChild(anchor)
    anchor.click()
  } finally {
    anchor.remove()
    setTimeout(() => URL.revokeObjectURL(url), 1000)
  }
}
