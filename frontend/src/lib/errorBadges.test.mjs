import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { ERROR_KIND_TONES, ERROR_KIND_TONE_BADGE_CLASSES, classifyStatus, errorKindBadgeClassName, errorKindLabel, errorKindTone, errorStatusBadgeClassName } from './errorBadges.ts'

const read = (p) => readFileSync(new URL(p, import.meta.url), 'utf8')
const locales = ['zh', 'en', 'zh-TW'].map((name) => JSON.parse(read(`../locales/${name}.json`)))

test('error kinds map to tones', () => {
  for (const kind of ['bps_rate_limited', 'rate_limit', 'rate_limited']) assert.equal(errorKindTone(kind), 'rate', kind)
  for (const kind of ['bps_policy_blocked', 'basispoints_model_access_changed', 'cyber_policy']) assert.equal(errorKindTone(kind), 'policy', kind)
  for (const kind of ['bps_unavailable', 'bps_model_unavailable', 'bps_concurrency_full', 'bps_budget_exhausted']) assert.equal(errorKindTone(kind), 'availability', kind)
  assert.equal(errorKindTone('basispoints_cutoff_completed'), 'info')
  for (const kind of ['server_error', 'upstream_error', 'upstream_timeout', 'transport_error', 'unauthorized']) assert.equal(errorKindTone(kind), 'error', kind)
  assert.equal(errorKindTone('some_new_rate_limit_kind'), 'rate', 'unknown kinds fall back to patterns')
  assert.equal(errorKindTone('mystery'), 'error')
  assert.equal(errorKindTone(''), 'error')
})

test('tone badges are distinct and themed for light and dark', () => {
  const classes = ERROR_KIND_TONES.map((tone) => ERROR_KIND_TONE_BADGE_CLASSES[tone])
  assert.equal(new Set(classes).size, ERROR_KIND_TONES.length)
  for (const value of classes) assert.match(value, /dark:text-/)
  assert.equal(errorKindBadgeClassName('bps_rate_limited'), ERROR_KIND_TONE_BADGE_CLASSES.rate)
  assert.equal(errorStatusBadgeClassName(502), ERROR_KIND_TONE_BADGE_CLASSES.error)
  assert.equal(errorStatusBadgeClassName(429), ERROR_KIND_TONE_BADGE_CLASSES.rate)
  assert.equal(errorStatusBadgeClassName(499), ERROR_KIND_TONE_BADGE_CLASSES.availability)
})

test('kindless rows use the status class, matching the server', () => {
  assert.equal(errorKindLabel({ upstream_error_kind: '', status_code: 502 }), 'server_error')
  assert.equal(errorKindLabel({ upstream_error_kind: 'bps_policy_blocked', status_code: 403 }), 'bps_policy_blocked')
  assert.equal(classifyStatus(499), 'client_closed')
  const server = read('../../../database/postgres.go')
  for (const [status, kind] of [[401, 'unauthorized'], [403, 'forbidden'], [429, 'rate_limit'], [499, 'client_closed']]) {
    assert.equal(classifyStatus(status), kind)
    assert.ok(server.includes(`return "${kind}"`), `server fallback ${kind}`)
  }
})

test('ops errors: clickable tiles, colored kinds, per-account view and account filter', () => {
  const page = read('../pages/OperationsErrors.tsx')
  for (const needle of [
    'active={!hasActiveFilters}', 'onClick={resetFilters}',
    "active={quickFilter === 'status5xx'}", "onClick={() => toggleQuickFilter('status5xx')}",
    "active={quickFilter === 'status401'}", "active={quickFilter === 'status429'}",
    "active={quickFilter === 'timeout'}", "active={quickFilter === 'retry'}",
    'errorKindBadgeClassName(errorKindLabel(log))', 'errorKindBadgeClassName(kind)', '<Card key={group.account_id} className="min-w-0',
    "{ value: 'account', label: t('opsErrors.viewByAccount') }", '<AccountErrorGroups', 'api.getOpsErrorsByAccount(',
    "usePersistedPageSize('ops_errors_accounts', 20, pageSizeOptions)", 'accountId: accountFilter', "timeout: timeoutFilter ? 'true' : ''",
  ]) assert.ok(page.includes(needle), needle)
  assert.ok(!page.includes('bg-slate-500/10 text-slate-600 dark:bg-slate-500/20 dark:text-slate-300">\n'), 'no flat slate kind badges')
  const api = read('../api.ts')
  for (const needle of ["search.set('account_id', params.accountId)", "search.set('retry', params.retry)", "search.set('timeout', params.timeout)", '/ops/errors/by-account?']) {
    assert.ok(api.includes(needle), needle)
  }
  assert.ok(read('../pages/Usage.tsx').includes("from '../lib/errorBadges'"), 'the usage log shares the tones')
  const handler = read('../../../admin/handler.go')
  assert.ok(handler.includes('api.GET("/ops/errors/by-account", h.GetOpsErrorsByAccount)'))
  for (const locale of locales) {
    for (const key of ['viewIndividual', 'viewByAccount', 'allAccounts', 'accountFilter', 'accountsCount', 'byAccountHint', 'accountErrors', 'errorBreakdown', 'lastError', 'expand', 'collapse', 'filterAccount', 'recentErrors']) {
      assert.equal(typeof locale.opsErrors[key], 'string', `opsErrors.${key}`)
    }
  }
})
