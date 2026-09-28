import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const settingsPage = readFileSync(new URL('../pages/Settings.tsx', import.meta.url), 'utf8')
const locales = ['zh', 'en', 'zh-TW'].map((name) => JSON.parse(readFileSync(new URL(`../locales/${name}.json`, import.meta.url), 'utf8')))

test('lightweight metering switch uses the shared Switch and auto-saves its setting', () => {
  assert.match(settingsPage, /<SettingField label=\{t\('settings\.usageMeteringEnabled'\)\}[^>]*layout="switch">\s*<Switch\s+checked=\{settingsForm\.usage_metering_enabled\}/)
  assert.match(settingsPage, /autoSaveBooleanField\('usage_metering_enabled', checked\)/)
  assert.match(settingsPage, /usage_metering_enabled: true,/)
})

test('lightweight metering copy exists in every locale', () => {
  for (const locale of locales) {
    assert.equal(typeof locale.settings.usageMeteringEnabled, 'string')
    assert.equal(typeof locale.settings.usageMeteringEnabledDesc, 'string')
  }
})
