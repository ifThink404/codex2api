import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const componentSource = readFileSync(new URL('../components/PromptFilterNewAPIBindings.tsx', import.meta.url), 'utf8')
const pageSource = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
const apiSource = readFileSync(new URL('../api.ts', import.meta.url), 'utf8')
const typesSource = readFileSync(new URL('../types.ts', import.meta.url), 'utf8')

test('identity binding cannot override the unified GuardPipeline policy', () => {
  assert.equal(componentSource.includes('requireSignedIdentity: false'), true)
  assert.equal(componentSource.includes('拦截模式和审核档位统一由 GuardPipeline 管理'), true)
  for (const fragment of ['policyMode', 'policyProfile', 'policy_mode', 'policy_profile', '策略模式']) {
    assert.equal(componentSource.includes(fragment), false, `binding editor still exposes policy control: ${fragment}`)
  }
  const bindingTypes = typesSource.slice(
    typesSource.indexOf('export interface PromptFilterNewAPIBinding'),
    typesSource.indexOf('export interface CreateAPIKeyRequest'),
  )
  assert.equal(bindingTypes.includes('policy_mode'), false)
  assert.equal(bindingTypes.includes('policy_profile'), false)
})

test('platform binding list never reads plaintext secret from a persisted binding', () => {
  assert.equal(componentSource.includes('binding.secret_masked'), true)
  assert.equal(componentSource.includes('binding.secret}'), false)
  assert.equal(componentSource.includes('binding.secret ??'), false)
  assert.equal(componentSource.includes('getPromptFilterNewAPISecret'), false)
})

test('key binding UI documents one-to-one isolation and one-time secret reveal', () => {
  for (const fragment of ['一个 Key 只能绑定一个调用方', '未绑定 Key 不接受 NewAPI 签名身份', '最长 32 字符且全局唯一', 'maxLength={32}', '明文只在本次响应中展示', '确认关闭密钥窗口', 'min={60}', '旧密钥仍可用于恢复']) {
    assert.equal(componentSource.includes(fragment), true, `missing isolation warning: ${fragment}`)
  }
  for (const deploymentSpecificCopy of ['凡人', 'buycodekey', '例如 fanren']) {
    assert.equal(componentSource.includes(deploymentSpecificCopy), false, `deployment-specific copy leaked into generic UI: ${deploymentSpecificCopy}`)
  }
})

test('key bindings are embedded in the single NewAPI audit integration panel', () => {
  const panelIndex = pageSource.indexOf("title={t('promptFilter.newapi.title')}")
  const bindingIndex = pageSource.indexOf('<PromptFilterNewAPIBindings />')
  const intelligenceIndex = pageSource.indexOf("title={t('promptFilter.intelligence.configTitle')}")

  assert.ok(panelIndex >= 0, 'missing NewAPI audit integration panel')
  assert.ok(bindingIndex > panelIndex, 'Key bindings must be rendered inside the NewAPI integration panel')
  assert.ok(intelligenceIndex > bindingIndex, 'Key bindings must remain inside the NewAPI panel before the next integration panel')
  assert.equal(pageSource.match(/<PromptFilterNewAPIBindings \/>/g)?.length, 1)
  assert.equal(componentSource.includes('<Card'), false, 'embedded binding editor must not create a second top-level card')
  assert.equal(pageSource.includes('getPromptFilterNewAPISecret'), false)
  assert.equal(pageSource.includes("setBool('newapi', 'enabled'"), false)
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
