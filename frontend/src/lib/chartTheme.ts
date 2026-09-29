// Shared recharts styling: margins, grid/axis colors and tooltip surfaces
// follow the theme tokens so charts read the same in light and dark.
export const chartMargin = { top: 8, right: 12, left: 8, bottom: 0 }
export const gridColor = 'var(--color-border)'
export const axisColor = 'var(--color-muted-foreground)'
export const tooltipContentStyle = {
  backgroundColor: 'var(--color-card)',
  border: '1px solid var(--color-border)',
  borderRadius: '12px',
  boxShadow: '0 10px 30px rgba(0, 0, 0, 0.12)',
  padding: '10px 14px',
}
export const tooltipLabelStyle = { color: 'var(--color-foreground)', fontWeight: 600 }
export const tooltipItemStyle = { color: 'var(--color-foreground)' }
