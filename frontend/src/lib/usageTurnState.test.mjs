import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

// turn-state 的长度是账号级「降级」标记（实测健康号 292 字符、降级号 312），
// 所以它必须在用量表和账号健康条上都是**可见的数字**，而不是埋在后端日志里。
// 这些断言盯住的就是「数字还在页面上」这件事。
const read = (path) => readFileSync(new URL(path, import.meta.url), 'utf8')

const usageTurnStateSource = read('../components/UsageTurnState.tsx')
const accountLatestSource = read('../components/AccountLatestTurnState.tsx')
const usageSource = read('../pages/Usage.tsx')
const healthBarSource = read('../components/AccountHealthBar.tsx')
const typesSource = read('../types.ts')
const apiSource = read('../api.ts')
const settingsSource = read('../pages/Settings.tsx')
const locales = Object.fromEntries(
  ['zh', 'en', 'zh-TW'].map((name) => [name, JSON.parse(read(`../locales/${name}.json`))]),
)

const TURN_STATE_I18N_KEYS = [
  'notRecorded',
  'missing',
  'received',
  'characters',
  'hint',
  'stripped',
  'clickFilter',
  'echoLine',
  'echoLineStripped',
  'allStates',
  'allEchoes',
  'allStripped',
  'notStripped',
]

const ECHO_CLASSES = ['none', 'same', 'cross', 'unknown', 'substitute']

test('usage log rows and filter params carry the three turn-state columns', () => {
  for (const field of ['turn_state_length', 'turn_state_echo', 'turn_state_stripped']) {
    assert.ok(typesSource.includes(field), `types.ts lacks UsageLog.${field}`)
  }
  assert.match(typesSource, /turn_state_length\??:\s*number\s*\|\s*null/, 'turn_state_length must allow null (= not recorded)')
  assert.ok(typesSource.includes('latest_turn_state?:'), 'AccountRow lacks latest_turn_state')

  for (const param of ['turnState', 'turnStateLength', 'turnStateEcho', 'turnStateStripped']) {
    assert.ok(apiSource.includes(`${param}?:`), `UsageLogQueryParams lacks ${param}`)
  }
  for (const wire of ['turn_state', 'turn_state_length', 'turn_state_echo', 'turn_state_stripped']) {
    assert.ok(apiSource.includes(`search.set('${wire}'`), `buildUsageLogSearchParams never sends ${wire}`)
  }
})

test('UsageTurnState renders the character count through shared ui components only', () => {
  assert.ok(usageTurnStateSource.includes("t('usage.turnState.characters'"), 'the character count must be the label')
  assert.ok(usageTurnStateSource.includes('turn_state_echo'), 'the tooltip must surface the echo class')
  assert.ok(usageTurnStateSource.includes('turn_state_stripped'), 'the tooltip must surface the stripped flag')
  assert.ok(usageTurnStateSource.includes("t('usage.turnState.clickFilter')"), 'the click-to-filter hint is missing')
  assert.ok(usageTurnStateSource.includes('<Tooltip'), 'must use components/ui/tooltip')
  // DESIGN.md:8 的控件表:任何自绘控件都要走 components/ui,组件里不许有裸标签。
  assert.equal(/from '\.\.\/components\//.test(usageTurnStateSource), false, 'imports must stay inside components/')
  for (const raw of [/<select[\s>]/, /<input\s/, /<button\s+className/]) {
    assert.equal(raw.test(usageTurnStateSource), false, `hand-written control ${raw} is not allowed`)
  }
})

test('Usage page shows the turn-state cell in both layouts and filters on all three columns', () => {
  assert.ok(usageSource.includes("from '../components/UsageTurnState'"), 'Usage.tsx must import UsageTurnState')
  const cells = usageSource.match(/<UsageTurnState\b/g) ?? []
  assert.ok(cells.length >= 2, `UsageTurnState must render in both the card and table layouts, found ${cells.length}`)

  for (const state of ['filterTurnState', 'filterTurnStateEcho', 'filterTurnStateStripped']) {
    assert.ok(usageSource.includes(`const [${state},`), `Usage.tsx lacks the ${state} state`)
    assert.ok(usageSource.includes(`set${state[0].toUpperCase()}${state.slice(1)}('')`), `resetLogFilters never clears ${state}`)
  }
  for (const param of ['turnState:', 'turnStateEcho:', 'turnStateStripped:']) {
    assert.ok(usageSource.includes(param), `the log query never sends ${param}`)
  }
  // 三个筛选器必须是共享 Select:DESIGN.md:10 明确禁止页面内手写下拉。
  const panel = usageSource.slice(usageSource.indexOf('showAdvancedFilters ? ('))
  for (const state of ['filterTurnState', 'filterTurnStateEcho', 'filterTurnStateStripped']) {
    const at = panel.indexOf(`value={${state}}`)
    assert.notEqual(at, -1, `the advanced filter panel lacks a control bound to ${state}`)
    assert.ok(panel.lastIndexOf('<Select', at) !== -1, `${state} must be bound to a shared Select`)
  }
  assert.ok(usageSource.includes('not_recorded'), 'clicking a row must be able to select the not_recorded state')
  assert.ok(usageSource.includes('toggleTurnStateFilter'), 'the cell must be click-to-filter')
})

test('the account health bar renders the latest turn-state under the bar', () => {
  assert.ok(healthBarSource.includes('AccountLatestTurnState'), 'AccountHealthBar must render AccountLatestTurnState')
  assert.ok(healthBarSource.includes('latestTurnState'), 'AccountHealthBar needs a latestTurnState prop')
  assert.ok(accountLatestSource.includes("t('accounts.healthBarTurnState'"), 'the widget must use the accounts copy')
  assert.ok(accountLatestSource.includes('formatRelativeTime'), 'the widget must show how old the sample is')
  assert.ok(accountLatestSource.includes("t('usage.turnState.characters'"), 'the widget must show the character count')
  assert.ok(accountLatestSource.includes('return null'), 'the widget must disappear when no sample exists')

  const accountsSource = read('../pages/Accounts.tsx')
  const detailSource = read('../components/AccountDetailSheet.tsx')
  for (const [name, source] of [['Accounts.tsx', accountsSource], ['AccountDetailSheet.tsx', detailSource]]) {
    assert.ok(source.includes('latestTurnState={'), `${name} never feeds the health bar its latest turn-state`)
  }
})

test('turn-state copy exists in every locale', () => {
  for (const [name, locale] of Object.entries(locales)) {
    const turnState = locale.usage?.turnState
    assert.equal(typeof turnState, 'object', `${name}.json lacks usage.turnState`)
    for (const key of TURN_STATE_I18N_KEYS) {
      assert.equal(typeof turnState[key], 'string', `${name}.json usage.turnState.${key} missing`)
    }
    for (const echo of ECHO_CLASSES) {
      assert.equal(typeof turnState.echo?.[echo], 'string', `${name}.json usage.turnState.echo.${echo} missing`)
    }
    assert.ok(turnState.characters.includes('{{count}}'), `${name}.json characters must interpolate the count`)
    assert.ok(turnState.echoLineStripped.includes('{{value}}'), `${name}.json echoLineStripped must interpolate the echo`)
    assert.equal(typeof locale.accounts?.healthBarTurnState, 'string', `${name}.json accounts.healthBarTurnState missing`)
    assert.ok(
      locale.accounts.healthBarTurnState.includes('{{value}}'),
      `${name}.json accounts.healthBarTurnState must interpolate the value`,
    )
  }
})

// 自动锁定只数真正的 server_error:上游容量降载(server_is_overloaded / slow_down)
// 既不计数也不清零,说明文案必须把这条讲清楚,否则运维会以为 500 一律计数。
test('auto-lock copy states that capacity shedding neither counts nor resets', () => {
  const markers = {
    zh: ['server_error', 'server_is_overloaded', 'slow_down'],
    en: ['server_error', 'server_is_overloaded', 'slow_down'],
    'zh-TW': ['server_error', 'server_is_overloaded', 'slow_down'],
  }
  for (const [name, locale] of Object.entries(locales)) {
    const desc = locale.settings?.codexSessionAutoLockDesc
    assert.equal(typeof desc, 'string', `${name}.json settings.codexSessionAutoLockDesc missing`)
    for (const marker of markers[name]) {
      assert.ok(desc.includes(marker), `${name}.json auto-lock copy never mentions ${marker}`)
    }
  }
  assert.ok(settingsSource.includes("t('settings.codexSessionAutoLockDesc')"), 'the auto-lock copy must stay rendered')
})
