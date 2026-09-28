import assert from 'node:assert/strict'
import test from 'node:test'
import { buildServiceErrorPageExport, saveServiceErrorPageExport } from './serviceErrorExport.ts'

test('current-page export retains full diagnostics and the loaded filter snapshot', async () => {
  const query = { start: '2026-09-28T03:00:00Z', end: '2026-09-28T04:00:00Z', status: '5xx', stage: 'dispatch', request_id: 'exact+request', grouped: true, cursor: 'page-two/+=' }
  const event = {
    id: 'event-1', message: '当前对话暂时无法继续处理请求',
    group: { key: 'test-group', count: 3, first_seen: query.start, last_seen: query.end },
    account_failover: { result: 'no_safe_candidate', selection: { rejection_counts: { account_identity_ineligible: 38 } } },
    dispatch_selection: { pinned_account_id: 200 },
    tool_protocol: { missing_namespaces: 9 },
  }
  const page = {
    grouped: true, items: [event], next_cursor: 'page-three',
    summary: { total: 100, groups: 40, status_429: 0, status_4xx: 0, status_5xx: 100 },
    collector: { pending: 1, dropped: 2, write_failures: 3 },
  }
  const file = buildServiceErrorPageExport(page, query, 2, new Date('2026-09-28T04:10:00Z'))
  // A refresh after the click must not change the downloaded snapshot.
  query.status = '429'
  event.account_failover.result = 'switched'
  const exported = JSON.parse(await file.blob.text())
  assert.equal(file.filename, 'service-errors-page-2-2026-09-28T04-10-00-000Z.json')
  assert.equal(exported.scope, 'current_page')
  assert.equal(exported.view, 'grouped_latest')
  assert.equal(exported.page_number, 2)
  assert.equal(exported.item_count, 1)
  assert.equal(exported.items.length, 1)
  assert.equal(exported.has_more, true)
  assert.equal(exported.query.status, '5xx')
  assert.equal(exported.query.cursor, 'page-two/+=')
  assert.equal(exported.query.request_id, 'exact+request')
  assert.equal(exported.summary_scope, 'filtered_time_range')
  assert.equal(exported.summary.total, 100)
  assert.equal(exported.items[0].group.count, 3)
  assert.equal(exported.items[0].account_failover.result, 'no_safe_candidate')
  assert.equal(exported.items[0].account_failover.selection.rejection_counts.account_identity_ineligible, 38)
  assert.deepEqual(exported.items[0].dispatch_selection, { pinned_account_id: 200 })
  assert.deepEqual(exported.items[0].tool_protocol, { missing_namespaces: 9 })
  assert.equal(exported.items[0].message, '当前对话暂时无法继续处理请求')
  assert.deepEqual(exported.collector, page.collector)
})

test('individual export includes exactly the visible page, even within a group', async () => {
  const page = { grouped: false, items: [{ id: 'first' }, { id: 'second' }], summary: { total: 12 }, collector: {} }
  const query = { start: 'start', end: 'end', grouped: false, group_key: 'group', cursor: '' }
  const { blob } = buildServiceErrorPageExport(page, query, 1)
  const exported = JSON.parse(await blob.text())
  assert.equal(exported.view, 'individual')
  assert.equal(exported.item_count, 2)
  assert.equal(exported.has_more, false)
  assert.deepEqual(exported.items.map(item => item.id), ['first', 'second'])
  assert.deepEqual(exported.query, query)
})

test('download triggers the file save and releases resources even if the click fails', context => {
  const previous = Object.getOwnPropertyDescriptor(globalThis, 'document')
  const actions = []
  let fail = false
  const anchor = { click() { actions.push('click'); if (fail) throw new Error('blocked') }, remove() { actions.push('remove') } }
  Object.defineProperty(globalThis, 'document', { configurable: true, value: {
    createElement: () => anchor,
    body: { appendChild: () => actions.push('append') },
  } })
  context.after(() => {
    if (previous) Object.defineProperty(globalThis, 'document', previous)
    else delete globalThis.document
  })
  context.mock.method(URL, 'createObjectURL', () => 'blob:test-service-errors')
  context.mock.method(URL, 'revokeObjectURL', () => actions.push('revoke'))
  context.mock.method(globalThis, 'setTimeout', callback => { callback(); return 0 })
  const file = { filename: 'service-errors-page-2.json', blob: new Blob(['{}']) }
  saveServiceErrorPageExport(file)
  assert.equal(anchor.download, file.filename)
  assert.equal(anchor.href, 'blob:test-service-errors')
  assert.deepEqual(actions, ['append', 'click', 'remove', 'revoke'])
  fail = true
  actions.length = 0
  assert.throws(() => saveServiceErrorPageExport(file), /blocked/)
  assert.deepEqual(actions, ['append', 'click', 'remove', 'revoke'])
})
