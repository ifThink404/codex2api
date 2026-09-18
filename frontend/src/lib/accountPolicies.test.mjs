import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const read = (rel) => readFileSync(new URL(rel, import.meta.url), 'utf8')

const typesSource = read('../types.ts')
const accountsSource = read('../pages/Accounts.tsx')
const runtimeSource = read('../pages/RuntimeStatus.tsx')
const proxyFieldSource = read('../components/ProxyField.tsx')
const quickEditorSource = read('../components/AccountProxyQuickEditor.tsx')
const timezoneHintSource = read('../components/ProxyTimezoneHint.tsx')
const bindingSource = read('./accountProxyBinding.ts')
const apiSource = read('../api.ts')

const locales = Object.fromEntries(
  ['zh', 'en', 'zh-TW'].map((name) => [name, JSON.parse(read(`../locales/${name}.json`))]),
)

const POLICY_FIELDS = ['prompt_filter_policy', 'egress_policy', 'session_guards_policy']

const ACCOUNT_I18N_KEYS = [
  'policyPromptFilterLabel', 'policyPromptFilterHint', 'policyPromptFilterInherit', 'policyPromptFilterExempt',
  'policyEgressLabel', 'policyEgressHint', 'policyEgressInherit', 'policyEgressDirect',
  'policySessionGuardsLabel', 'policySessionGuardsHint', 'policySessionGuardsInherit', 'policySessionGuardsOff',
  'badgePromptExempt', 'badgeEgressDirect', 'badgeGuardsOff', 'badgeTimezoneMismatch',
  'proxyTimezoneMismatch', 'proxyTimezoneSync', 'proxyTimezoneSynced', 'proxyTimezoneSyncFailed',
  'proxyTimezoneDrafted',
]

const RUNTIME_I18N_KEYS = ['promptPolicy', 'promptExempted', 'promptBlockedAfterSelection']

test('account policy fields are typed on the row, the PATCH payload and the proxy pool entry', () => {
  for (const field of POLICY_FIELDS) {
    assert.ok(typesSource.includes(`${field}?:`), `types.ts lacks ${field}`)
  }
  assert.ok(typesSource.includes("prompt_filter_policy?: 'inherit' | 'exempt'"), 'prompt filter policy union missing')
  assert.ok(typesSource.includes("egress_policy?: 'inherit' | 'direct'"), 'egress policy union missing')
  assert.ok(typesSource.includes("session_guards_policy?: 'inherit' | 'off'"), 'session guards policy union missing')
  // PATCH /accounts/:id/scheduler 接受这三个字段;类型缺了就只能靠 any 绕过。
  const schedulerRequest = typesSource.slice(
    typesSource.indexOf('export interface UpdateAccountSchedulerRequest'),
  )
  const schedulerBlock = schedulerRequest.slice(0, schedulerRequest.indexOf('}'))
  for (const field of POLICY_FIELDS) {
    assert.ok(schedulerBlock.includes(`${field}?:`), `UpdateAccountSchedulerRequest lacks ${field}`)
  }
  // 代理池条目的出口时区:ProxyRow(api.ts) 与徽章判定用的 ProxyBindingProxy 两处都要有。
  assert.ok(apiSource.includes('test_timezone'), 'api.ts ProxyRow lacks test_timezone')
  assert.ok(bindingSource.includes('test_timezone?:'), 'ProxyBindingProxy lacks test_timezone')
  assert.ok(typesSource.includes('prompt_policy?:'), 'RuntimeStatusResponse.session_guards lacks prompt_policy')
})

test('the scheduler dialog edits the three policies with the shared Select', () => {
  assert.ok(
    accountsSource.includes("import { Select } from \"@/components/ui/select\"") ||
      accountsSource.includes("from '@/components/ui/select'"),
    'Accounts.tsx must use the shared Select',
  )
  for (const state of ['setPromptFilterPolicy', 'setEgressPolicy', 'setSessionGuardsPolicy']) {
    assert.ok(accountsSource.includes(`${state}(`), `Accounts.tsx lacks the ${state} state setter`)
  }
  for (const field of POLICY_FIELDS) {
    assert.ok(accountsSource.includes(`${field}:`), `Accounts.tsx scheduler payload lacks ${field}`)
    assert.ok(accountsSource.includes(`account.${field}`), `Accounts.tsx never seeds the editor from account.${field}`)
  }
  // DESIGN.md:40 要求断言"用了哪个共享组件":三个策略下拉必须是 components/ui/select。
  for (const labelKey of ['policyPromptFilterLabel', 'policyEgressLabel', 'policySessionGuardsLabel']) {
    const at = accountsSource.indexOf(`t("accounts.${labelKey}")`)
    assert.notEqual(at, -1, `Accounts.tsx lacks the ${labelKey} field`)
    const block = accountsSource.slice(at, at + 1400)
    assert.ok(block.includes('<Select'), `${labelKey} must be rendered with the shared Select`)
    assert.ok(block.includes('onValueChange'), `${labelKey} must drive state through onValueChange`)
  }
  assert.equal(/<select[\s>]/.test(accountsSource), false, 'no hand-written <select> in Accounts.tsx')
  // 整页的裸 checkbox 断言会被列表行的历史遗留选择框绊住，这里只守新加的策略卡片。
  const cardAt = accountsSource.indexOf('t("accounts.policySectionTitle")')
  assert.notEqual(cardAt, -1, 'Accounts.tsx lacks the account policy card')
  const policyCard = accountsSource.slice(cardAt, accountsSource.indexOf('t("accounts.autoPauseTitle")', cardAt))
  assert.ok(policyCard.length > 0, 'the policy card must sit before the dispatch-count card')
  assert.equal(/<input[\s>]/.test(policyCard), false, 'no raw <input> in the account policy card')
  assert.equal(/<button[\s>]/.test(policyCard), false, 'no raw <button> in the account policy card')
})

test('the account list badges non-inherit policies and a proxy/account timezone mismatch', () => {
  for (const key of ['badgePromptExempt', 'badgeEgressDirect', 'badgeGuardsOff', 'badgeTimezoneMismatch']) {
    assert.ok(accountsSource.includes(`accounts.${key}`), `Accounts.tsx never renders accounts.${key}`)
  }
  assert.ok(accountsSource.includes('AccountPolicyBadges'), 'the policy badges must live in one shared renderer')
  // 只在非 inherit 时出徽章,否则每一行都会挂三个"继承"噪声徽章。
  for (const gate of ['=== "exempt"', '=== "direct"', '=== "off"']) {
    assert.ok(accountsSource.includes(gate), `policy badges must be gated on ${gate}`)
  }
  // 时区不一致只对 OAuth 行判定,且比的是绑定池条目的出口时区。
  assert.ok(accountsSource.includes('test_timezone'), 'the timezone mismatch badge must read the pool entry test_timezone')
  // 出口被直连策略或 Resin 整层覆盖时,绑定的那条代理根本不是出口,不该再比时区。
  assert.ok(
    accountsSource.includes('account.egress_policy === "direct" || binding.kind === "resin"'),
    'the timezone badge must stand down when the bound proxy is not the effective egress',
  )
})

test('ProxyTimezoneHint renders with shared components and both proxy editors mount it', () => {
  assert.ok(timezoneHintSource.includes("from '@/components/ui/button'") || timezoneHintSource.includes('from "@/components/ui/button"'), 'the sync action must be a shared Button')
  assert.ok(timezoneHintSource.includes('accounts.proxyTimezoneMismatch'), 'the hint copy must be i18n')
  assert.ok(timezoneHintSource.includes('accounts.proxyTimezoneSync'), 'the sync button copy must be i18n')
  assert.equal(/<button[\s>]/.test(timezoneHintSource), false, 'no hand-written <button> in ProxyTimezoneHint.tsx')

  assert.ok(proxyFieldSource.includes('ProxyTimezoneHint'), 'ProxyField.tsx must render the hint')
  assert.ok(proxyFieldSource.includes('accountTimezone'), 'ProxyField.tsx must accept accountTimezone')
  assert.ok(proxyFieldSource.includes('onSyncTimezone'), 'ProxyField.tsx must accept onSyncTimezone')

  assert.ok(quickEditorSource.includes('ProxyTimezoneHint'), 'AccountProxyQuickEditor.tsx must render the hint')
  assert.ok(
    quickEditorSource.includes('api.updateAccountScheduler(account.id, { timezone'),
    'the sync action must PATCH the account timezone',
  )
  assert.ok(quickEditorSource.includes('accounts.proxyTimezoneSynced'), 'a successful sync must toast')
  // 按已绑定的池条目比,而不是输入框草稿:同步当场落库,草稿可能根本没保存。
  assert.ok(quickEditorSource.includes('boundPoolEntry'), 'the quick editor must compare the bound pool entry')
  assert.equal(
    quickEditorSource.includes('proxyTimezone={poolEntry?.test_timezone}'),
    false,
    'the quick editor must not compare the unsaved draft URL',
  )
  // account 是快照,onSaved() 刷新列表不会更新它;同步成功后要自己把提示收掉。
  assert.ok(quickEditorSource.includes('setSyncedTimezone(timezone)'), 'a successful sync must clear the hint')
  // 弹窗里的同步只写草稿,文案不能和快捷弹窗那条当场 PATCH 的混用。
  assert.ok(accountsSource.includes('t("accounts.proxyTimezoneDrafted")'), 'the in-dialog sync must toast the draft copy')
  assert.equal(
    accountsSource.includes('t("accounts.proxyTimezoneSynced")'),
    false,
    'the in-dialog sync must not claim the timezone is already saved',
  )
  assert.equal(/<select[\s>]/.test(quickEditorSource), false, 'no hand-written <select> in AccountProxyQuickEditor.tsx')
})

test('runtime status reports the deferred prompt policy counters', () => {
  assert.ok(runtimeSource.includes("t('runtime.promptPolicy')"), 'the prompt policy row is missing')
  assert.ok(runtimeSource.includes('prompt_policy?.exempted'), 'the exempted counter is missing')
  assert.ok(runtimeSource.includes('prompt_policy?.blocked_after_selection'), 'the blocked-after-selection counter is missing')
  // 这一行必须跟在自动锁定行之后,和后端面板顺序一致。
  assert.ok(
    runtimeSource.indexOf("t('runtime.promptPolicy')") > runtimeSource.indexOf("t('runtime.autoLock')"),
    'the prompt policy row must follow the auto-lock row',
  )
})

test('every new string exists in all three locales', () => {
  for (const [name, locale] of Object.entries(locales)) {
    for (const key of ACCOUNT_I18N_KEYS) {
      assert.equal(typeof locale.accounts?.[key], 'string', `${name}.json accounts.${key} missing`)
    }
    for (const key of RUNTIME_I18N_KEYS) {
      assert.equal(typeof locale.runtime?.[key], 'string', `${name}.json runtime.${key} missing`)
    }
    for (const key of ['proxyTimezoneMismatch']) {
      assert.ok(
        locale.accounts[key].includes('{{proxy}}') && locale.accounts[key].includes('{{account}}'),
        `${name}.json accounts.${key} must interpolate {{proxy}} and {{account}}`,
      )
    }
    assert.ok(locale.accounts.proxyTimezoneSync.includes('{{tz}}'), `${name}.json accounts.proxyTimezoneSync must interpolate {{tz}}`)
  }
})
