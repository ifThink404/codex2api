import assert from 'node:assert/strict'
import test from 'node:test'
import { serviceErrorCollectorHasLoss, serviceErrorNewAPIUserLabel } from './serviceErrors.ts'
import { serviceErrorSearchParams } from '../api.ts'

test('service error filters preserve exact request IDs and opaque cursors', () => {
  const query = new URLSearchParams(serviceErrorSearchParams({ start: '2026-09-10T00:00:00Z', end: '2026-09-10T01:00:00Z', status: '429', stage: 'rate_limit', request_id: ' req+id&x=1 ', cursor: 'opaque/cursor+=' }))
  assert.equal(query.get('request_id'), 'req+id&x=1')
  assert.equal(query.get('cursor'), 'opaque/cursor+=')
  assert.equal(query.get('limit'), '20')
  assert.equal(query.get('stage'), 'rate_limit')
  assert.equal(query.has('x'), false)
})

test('service error empty filters are omitted and collector loss is visible', () => {
  const query = new URLSearchParams(serviceErrorSearchParams({ start: 'start', end: 'end', status: '', cursor: '' }))
  assert.equal(query.has('cursor'), false)
  assert.equal(query.has('status'), false)
  assert.equal(serviceErrorCollectorHasLoss({ dropped: 0, write_failures: 0 }), false)
  assert.equal(serviceErrorCollectorHasLoss({ dropped: 1, write_failures: 0 }), true)
  assert.equal(serviceErrorCollectorHasLoss({ dropped: 0, write_failures: 1 }), true)
})

test('grouped service errors and original group requests use separate queries', () => {
  const grouped = new URLSearchParams(serviceErrorSearchParams({ start: 'start', end: 'end', grouped: true }))
  assert.equal(grouped.get('grouped'), 'true')
  assert.equal(grouped.has('group_key'), false)
  const details = new URLSearchParams(serviceErrorSearchParams({ start: 'start', end: 'end', grouped: false, group_key: 'a'.repeat(64), cursor: 'group-cursor/+=', status: '5xx', request_id: 'newapi+id' }))
  assert.equal(details.get('grouped'), 'false')
  assert.equal(details.get('group_key'), 'a'.repeat(64))
  assert.equal(details.get('cursor'), 'group-cursor/+=')
  assert.equal(details.get('status'), '5xx')
  assert.equal(details.get('request_id'), 'newapi+id')
  assert.equal(details.get('start'), 'start')
  assert.equal(details.get('end'), 'end')
})

test('service error NewAPI caller labels display only verified names and IDs', () => {
  const verified = { newapi_identity_verified: true, newapi_user_name: ' 示例用户 ', newapi_user_id: ' 1881 ' }
  assert.equal(serviceErrorNewAPIUserLabel(verified), '示例用户 #1881')
  assert.equal(serviceErrorNewAPIUserLabel({ ...verified, newapi_user_name: '' }), '#1881')
  assert.equal(serviceErrorNewAPIUserLabel({ ...verified, newapi_user_id: '' }), '示例用户')
  assert.equal(serviceErrorNewAPIUserLabel({ ...verified, newapi_identity_verified: false }), '')
  assert.equal(serviceErrorNewAPIUserLabel({ newapi_user_name: 'unverified' }), '')
  assert.equal(serviceErrorNewAPIUserLabel({ newapi_identity_verified: true }), '')
  assert.equal(serviceErrorNewAPIUserLabel({}), '')
})
