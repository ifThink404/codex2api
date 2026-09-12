import assert from 'node:assert/strict'
import test from 'node:test'
import { extractQualityTestHTML, extractQualityTestSVG, qualityTestPreviewDocument, isQualityTestActive, qualityTestPlanTone } from './qualityTest.ts'
import { readClaudeTestEvents } from './claudeConnectionTest.ts'

test('quality preview extracts documents and SVG without rendering explanatory prose', () => {
  const html = '<!DOCTYPE html><html><body><svg><text>鹈鹕</text></svg></body></html>'
  assert.equal(extractQualityTestHTML(html), html)
  assert.equal(extractQualityTestHTML(`Here is the result:\n\`\`\`html\n${html}\n\`\`\`\nDone.`), html)
  assert.equal(extractQualityTestHTML(`Example:\n${html}\nExplanations.`), html)
  assert.equal(extractQualityTestHTML('```svg\n<svg viewBox="0 0 10 10"><circle r="3" /></svg>\n```'), '<svg viewBox="0 0 10 10"><circle r="3" /></svg>')
  assert.equal(extractQualityTestHTML('```html\n<html><body>partial'), '<html><body>partial')
  assert.equal(extractQualityTestHTML('No HTML was generated.'), '')
  assert.equal(extractQualityTestHTML(''), '')
})

test('svg export picks the largest top-level svg, adds xmlns and carries page styles', () => {
  const page = '<html><head><style>.wheel{animation:spin 1s linear infinite}</style></head><body><svg viewBox="0 0 4 4"><circle r="1"/></svg><svg viewBox="0 0 800 600"><g class="wheel"><svg width="2" height="2"><rect/></svg></g></svg></body></html>'
  const svg = extractQualityTestSVG(page)
  assert.ok(svg.startsWith('<?xml version="1.0" encoding="UTF-8"?>\n<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 800 600"><style>.wheel{animation:spin 1s linear infinite}</style>'))
  assert.ok(svg.endsWith('<g class="wheel"><svg width="2" height="2"><rect/></svg></g></svg>'))
  assert.ok(!svg.includes('viewBox="0 0 4 4"'))
  assert.equal(extractQualityTestSVG('<svg xmlns="http://www.w3.org/2000/svg"><use xlink:href="#a"/></svg>'), '<?xml version="1.0" encoding="UTF-8"?>\n<svg xmlns:xlink="http://www.w3.org/1999/xlink" xmlns="http://www.w3.org/2000/svg"><use xlink:href="#a"/></svg>')
  assert.equal(extractQualityTestSVG('<html><body><canvas></canvas></body></html>'), '')
  assert.equal(extractQualityTestSVG('<svg><g>unterminated'), '')
})

test('preview policy is installed before untrusted content, allowing inline animation only', () => {
  const malicious = '<html><head><script src="https://example.invalid/x.js"></script></head><body><svg /></body></html>'
  const document = qualityTestPreviewDocument(malicious)
  assert.ok(document.indexOf('Content-Security-Policy') < document.indexOf(malicious))
  for (const policy of ["default-src 'none'", "script-src 'unsafe-inline'", "style-src 'unsafe-inline'", "connect-src 'none'", "frame-src 'none'", "base-uri 'none'", "form-action 'none'"]) assert.ok(document.includes(policy))
  assert.ok(document.includes(malicious))
})

test('quality stream preserves split Unicode, whitespace and diagnostics arriving after completion', async () => {
  const html = '<html>\n  <svg><text>鹈鹕</text></svg>\n</html>'
  const wire = [{ type: 'content', text: html.slice(0, 6) }, { type: 'content', text: '\n  ' }, { type: 'content', text: html.slice(9) }, { type: 'test_complete', success: true }, { type: 'diagnostics', codex_diagnostics: { model: 'test', duration_ms: 23, usage: { reasoning_output_tokens: 42 } } }].map((event) => `data: ${JSON.stringify(event)}\r\n\r\n`).join('')
  const bytes = new TextEncoder().encode(wire)
  const stream = new ReadableStream({ start(controller) { for (let i = 0; i < bytes.length; i += 2) controller.enqueue(bytes.slice(i, i + 2)); controller.close() } })
  const events = []
  assert.equal(await readClaudeTestEvents(stream, (event) => events.push(event)), true)
  assert.equal(events.filter((event) => event.type === 'content').map((event) => event.text).join(''), html)
  assert.equal(events.at(-1).codex_diagnostics.usage.reasoning_output_tokens, 42)
})

test('stopping tasks still occupy a slot and subscription variants keep their display family', () => {
  for (const status of ['running', 'cancelling']) assert.equal(isQualityTestActive({ status }), true)
  for (const status of ['completed', 'error', 'stopped', 'interrupted']) assert.equal(isQualityTestActive({ status }), false)
  assert.equal(isQualityTestActive(null), false)
  assert.equal(qualityTestPlanTone('Pro'), 'pro')
  assert.equal(qualityTestPlanTone('plus'), 'plus')
  assert.equal(qualityTestPlanTone('business'), 'team')
  assert.equal(qualityTestPlanTone('team'), 'team')
  assert.equal(qualityTestPlanTone('enterprise'), 'enterprise')
  assert.equal(qualityTestPlanTone('free'), 'free')
  assert.equal(qualityTestPlanTone('custom'), 'other')
})

test('svg export retargets html/body rules to the root element without touching similar class or id names', () => {
  const page = '<html><head><style>html, body { margin: 0; background: linear-gradient(#8fd7ff, #71bd50); }\nbody .stage { width: 100vw; }\n.body-part, #html { fill: red; }</style></head><body><svg class="stage"><rect fill="url(#missing)"/></svg></body></html>'
  assert.ok(extractQualityTestSVG(page).includes('<style>svg:root, svg:root { margin: 0; background: linear-gradient(#8fd7ff, #71bd50); }\nsvg:root .stage { width: 100vw; }\n.body-part, #html { fill: red; }</style>'))
})
