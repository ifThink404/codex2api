import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const componentSource = readFileSync(new URL('../components/PromptFilterNewAPIBindings.tsx', import.meta.url), 'utf8')
const apiSource = readFileSync(new URL('../api.ts', import.meta.url), 'utf8')

test('platform binding editor keeps signed identity opt-in and shows Chinese policy labels', () => {
  assert.equal(componentSource.includes('requireSignedIdentity: false'), true)
  for (const label of ['继承全局模式', '影子记录', '警告（放行并提示）', '执行（命中时拦截）', '均衡', '严格', '研究']) {
    assert.equal(componentSource.includes(label), true, `missing Chinese binding label: ${label}`)
  }
})

test('platform binding list never reads plaintext secret from a persisted binding', () => {
  assert.equal(componentSource.includes('binding.secret_masked'), true)
  assert.equal(componentSource.includes('binding.secret}'), false)
  assert.equal(componentSource.includes('binding.secret ??'), false)
  assert.equal(componentSource.includes('getPromptFilterNewAPISecret'), false)
})

test('platform binding UI documents one-to-one isolation and one-time secret reveal', () => {
  for (const fragment of ['一个 Key 只能绑定一个平台', '不回退到全局共享密钥', '最长 32 字符且全局唯一', 'maxLength={32}', '明文只在本次响应中展示', '确认关闭密钥窗口', 'min={60}', '旧密钥仍可用于恢复']) {
    assert.equal(componentSource.includes(fragment), true, `missing isolation warning: ${fragment}`)
  }
})

test('enabling required signatures is protected by a 401 safety confirmation', () => {
  for (const fragment of [
    '!editing.require_signed_identity && editForm.requireSignedIdentity',
    '确认开启强制签名身份',
    '请求会立即返回 401',
    '不会记为 Prompt 违规，也不会触发违规处罚',
    'if (!approved) return',
  ]) {
    assert.equal(componentSource.includes(fragment), true, `missing required-signature safety guard: ${fragment}`)
  }
})

test('platform binding API client covers CRUD and secret rotation endpoints', () => {
  for (const fragment of [
    "'/prompt-filter/newapi-bindings'",
    '/secret/generate',
    "method: 'PATCH'",
    "method: 'DELETE'",
  ]) {
    assert.equal(apiSource.includes(fragment), true, `missing binding API operation: ${fragment}`)
  }
})
