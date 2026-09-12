import type { ClaudeTestEvent } from './claudeConnectionTest'
import type { CodexTestDiagnostics } from './codexConnectionTest'

export const PELICAN_PROMPT = '创建一个 HTML，内容是用 SVG 绘制一个鹈鹕骑自行车的 2D 动画。'
export const QUALITY_TEST_OUTPUT_LIMIT = 1024 * 1024

export interface QualityTestJob {
  id: number
  account_id: number
  account_name: string
  plan_type: string
  channel: string
  model: string
  reasoning_effort: string
  prompt?: string
  output?: string
  status: 'running' | 'cancelling' | 'completed' | 'error' | 'stopped' | 'interrupted'
  error?: string
  created_at: string
  updated_at: string
  completed_at?: string
  response_model?: string
  duration_ms: number
  first_content_ms?: number
  input_tokens?: number
  output_tokens?: number
  reasoning_tokens?: number
}

export interface QualityTestJobsResponse {
  jobs: QualityTestJob[]
  active_jobs: QualityTestJob[]
  total: number
  concurrency_limit: number
}

export function isQualityTestActive(job?: Pick<QualityTestJob, 'status'> | null): boolean {
  return job?.status === 'running' || job?.status === 'cancelling'
}

export function qualityTestPlanTone(plan: string): string {
  const value = plan.trim().toLowerCase()
  if (value.includes('pro')) return 'pro'
  if (value === 'plus') return 'plus'
  if (value === 'team' || value === 'business') return 'team'
  if (['enterprise', 'edu', 'education', 'k12'].includes(value)) return 'enterprise'
  return value === 'free' ? 'free' : 'other'
}

export interface QualityTestEvent extends ClaudeTestEvent {
  codex_diagnostics?: CodexTestDiagnostics
}

// Accept complete documents, Markdown-wrapped HTML, and standalone SVG/fragments.
// Never put the raw model reply into the admin document's DOM.
export function extractQualityTestHTML(output: string): string {
  const blocks = [...output.matchAll(/```(?:html|svg|xml)?[^\S\r\n]*\r?\n([\s\S]*?)```/gi)]
  const candidate = blocks.find((block) => /<!doctype\s+html|<html[\s>]/i.test(block[1]))?.[1]
    ?? blocks.find((block) => /<(?:svg|div|style|main|body)[\s>]/i.test(block[1]))?.[1]
    ?? output.replace(/^\s*```(?:html|svg|xml)?\s*\r?\n/i, '').replace(/\s*```\s*$/, '')
  const start = candidate.search(/<!doctype\s+html|<html[\s>]|<(?:svg|div|style|main|body|section|canvas)[\s>]/i)
  if (start < 0) return ''
  const html = candidate.slice(start).trim()
  const end = html.toLowerCase().lastIndexOf('</html>')
  return end >= 0 ? html.slice(0, end + 7) : html
}

// Pull the largest top-level <svg> out of the generated markup as a standalone file.
// Page-level <style> blocks are copied inside so class-based CSS animations keep running.
export function extractQualityTestSVG(html: string): string {
  const tags = [...html.matchAll(/<svg[\s>]|<\/svg\s*>/gi)]
  let depth = 0, start = -1, best = ''
  for (const tag of tags) {
    if (tag[0].toLowerCase().startsWith('<svg')) {
      if (depth === 0) start = tag.index
      depth++
    } else if (depth > 0 && --depth === 0 && start >= 0) {
      const candidate = html.slice(start, tag.index + tag[0].length)
      if (candidate.length > best.length) best = candidate
      start = -1
    }
  }
  if (!best) return ''
  const outer = html.slice(0, html.indexOf(best)) + html.slice(html.indexOf(best) + best.length)
  // Rules aimed at html/body (page background, fonts) have no host in a standalone
  // SVG; point them at the root element so the viewport keeps the same look.
  const styles = [...outer.matchAll(/<style[^>]*>([\s\S]*?)<\/style>/gi)]
    .map((match) => match[1].trim().replace(/(^|[\s,}])(?:html|body|:root)(?=[\s,{.:>[])/g, '$1svg:root'))
    .filter(Boolean)
  const openEnd = best.indexOf('>')
  let open = best.slice(0, openEnd + 1)
  if (!/\sxmlns=/.test(open)) open = open.replace(/^<svg/i, '<svg xmlns="http://www.w3.org/2000/svg"')
  if (/xlink:/.test(best) && !/\sxmlns:xlink=/.test(open)) open = open.replace(/^<svg/i, '<svg xmlns:xlink="http://www.w3.org/1999/xlink"')
  const styleBlock = styles.length ? `<style>${styles.join('\n')}</style>` : ''
  return `<?xml version="1.0" encoding="UTF-8"?>\n${open}${styleBlock}${best.slice(openEnd + 1)}`
}

// The policy precedes every byte of generated markup. The iframe must additionally
// use sandbox="allow-scripts" (never allow-same-origin). Inline animation is allowed;
// network requests, external resources, forms, workers and nested frames are blocked.
export function qualityTestPreviewDocument(html: string): string {
  const policy = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; media-src data: blob:; connect-src 'none'; frame-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'"
  return `<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="${policy}"><meta name="referrer" content="no-referrer"><meta name="viewport" content="width=device-width, initial-scale=1"><style>html,body{margin:0;min-height:100%;}svg{max-width:100%;}</style></head><body>${html}</body></html>`
}
