import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

// 这个开关的持久化键名没变（codex_preflight_sse_passthrough_enabled），但语义
// 反了：不再提前透传前置元数据、不再提前提交 200，改为在正常提交响应头时附带
// 首响应计时头。文案是运维唯一能看到的说明，所以旧承诺必须从三份 locale 里彻底
// 消失，否则会有人按「提前提交 200」的心智去开它。
const read = (path) => readFileSync(new URL(path, import.meta.url), 'utf8')

const settingsSource = read('../pages/Settings.tsx')
const typesSource = read('../types.ts')
const locales = Object.fromEntries(
  ['zh', 'en', 'zh-TW'].map((name) => [name, JSON.parse(read(`../locales/${name}.json`))]),
)

const FIRST_RESPONSE_I18N_KEYS = [
  'codexPreflightSSEPassthrough',
  'codexPreflightSSEPassthroughDesc',
  'codexPreflightSSEPassthroughEnabled',
  'codexPreflightSSEPassthroughEnabledDesc',
]

// 旧语义的关键承诺：提前透传 / 提前提交 200 / 旧版兼容。
const RETIRED_MARKERS = {
  zh: ['立即透传', '前置帧透传', '提前提交 HTTP 200', '旧版兼容模式'],
  en: ['immediate passthrough', 'preflight passthrough', 'Legacy Compat', 'legacy compatibility mode'],
  'zh-TW': ['立即透傳', '前置幀透傳', '提前提交 HTTP 200', '舊版相容模式'],
}

// 设置分区的副标题是运维在展开开关之前唯一读到的一行，必须跟着改。
const sectionCopy = (locale) => locale.settings?.nav?.codexTransportDesc

test('the first-response timing switch keeps its persisted key and wiring', () => {
  assert.ok(
    typesSource.includes('codex_preflight_sse_passthrough_enabled: boolean'),
    'types.ts lost the persisted settings field',
  )
  assert.ok(
    settingsSource.includes("autoSaveBooleanField('codex_preflight_sse_passthrough_enabled'"),
    'Settings.tsx no longer persists the switch under its unchanged API key',
  )
  for (const key of FIRST_RESPONSE_I18N_KEYS) {
    assert.ok(settingsSource.includes(`settings.${key}`), `Settings.tsx stopped rendering ${key}`)
  }
})

test('first-response timing copy exists in every locale', () => {
  for (const [name, locale] of Object.entries(locales)) {
    for (const key of FIRST_RESPONSE_I18N_KEYS) {
      assert.equal(
        typeof locale.settings?.[key],
        'string',
        `${name}.json settings.${key} missing`,
      )
    }
    assert.equal(
      typeof sectionCopy(locale),
      'string',
      `${name}.json settings.nav.codexTransportDesc missing`,
    )
  }
})

test('the copy promises timing headers instead of an early HTTP 200', () => {
  for (const [name, locale] of Object.entries(locales)) {
    const blob = [...FIRST_RESPONSE_I18N_KEYS.map((key) => locale.settings[key]), sectionCopy(locale)].join('\n')
    assert.ok(
      blob.includes('X-Codex2API-First-Response-Ms'),
      `${name}.json must name the timing header operators will see`,
    )
    assert.ok(blob.includes('NewAPI'), `${name}.json must say who consumes the report`)
    for (const retired of RETIRED_MARKERS[name]) {
      assert.ok(
        !blob.includes(retired),
        `${name}.json still promises the retired behaviour: ${retired}`,
      )
    }
  }
})
