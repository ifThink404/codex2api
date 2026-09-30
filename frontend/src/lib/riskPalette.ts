// riskPalette is the risk-tone hero palette (soft wash, foreground, pill and
// dot) shared by the pool runway card and the BPS health hero.
import type { RiskLevel } from './poolRunway'

export function riskPalette(level: RiskLevel) {
  if (level === 'high') {
    return {
      wash: 'bg-[radial-gradient(ellipse_at_top_left,color-mix(in_oklab,var(--color-destructive)_14%,transparent),transparent_55%)]',
      fg: 'text-destructive',
      pill: 'bg-destructive/12 text-destructive',
      dot: 'bg-destructive',
    }
  }
  if (level === 'medium') {
    return {
      wash: 'bg-[radial-gradient(ellipse_at_top_left,color-mix(in_oklab,#f59e0b_14%,transparent),transparent_55%)]',
      fg: 'text-amber-600 dark:text-amber-400',
      pill: 'bg-amber-500/12 text-amber-700 dark:text-amber-300',
      dot: 'bg-amber-500',
    }
  }
  return {
    wash: 'bg-[radial-gradient(ellipse_at_top_left,color-mix(in_oklab,#22c55e_12%,transparent),transparent_55%)]',
    fg: 'text-emerald-600 dark:text-emerald-400',
    pill: 'bg-emerald-500/12 text-emerald-700 dark:text-emerald-300',
    dot: 'bg-emerald-500',
  }
}
