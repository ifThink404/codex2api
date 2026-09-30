import type { ReactNode } from 'react'
import { CircleHelp } from 'lucide-react'
import ChannelScopeBadges from './ChannelScopeBadges'
import type { UpstreamChannel } from '../types'
import { Card, CardContent } from '@/components/ui/card'
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/ui/tooltip'
import { cn } from '@/lib/utils'

// Settings layout primitives shared by the system settings page and plugin
// configuration pages (DESIGN.md §2): SettingsCard, SettingField, SettingHelp
// and the form grid constants.

/** Shared form grids — explicit columns so col-span / alignment stay predictable. */
export const SETTINGS_FIELD_GRID = 'grid grid-cols-1 gap-x-4 gap-y-4 sm:grid-cols-2'
export const SETTINGS_FIELD_GRID_3 = 'grid grid-cols-1 gap-x-4 gap-y-4 sm:grid-cols-2 xl:grid-cols-3'
export const SETTINGS_SWITCH_GRID = 'grid grid-cols-1 gap-3 sm:grid-cols-2'
// 卡片里只有一个开关时用整行，放进双列栅格会挤成半宽、标签折行。
export const SETTINGS_SWITCH_ROW = 'grid grid-cols-1 gap-3'
// 一组只含开关的相关设置合并成一张卡，用 SettingField layout="row" 逐行排列，说明文字直接外显。
export const SETTINGS_ROW_LIST = 'divide-y divide-border/60'
// 卡片级双列栅格：卡片高度不一，必须顶对齐，否则矮卡被拉高留下大片空白。
export const SETTINGS_CARD_GRID_2 = 'grid gap-4 lg:grid-cols-2 lg:items-stretch'

export function SettingsCard({
  title,
  description,
  children,
  className,
  contentClassName,
  footer,
  icon,
  badge,
  channels,
  tone = 'default',
}: {
  title: string
  description?: string
  children: ReactNode
  className?: string
  contentClassName?: string
  footer?: ReactNode
  icon?: ReactNode
  badge?: ReactNode
  channels?: readonly UpstreamChannel[]
  tone?: 'default' | 'danger'
}) {
  return (
    <Card
      className={cn(
        'gap-0 py-0 border-border/60 bg-card shadow-2xs',
        tone === 'danger' && 'border-destructive/30 bg-destructive/[0.02]',
        className,
      )}
    >
      <CardContent className={cn('p-4.5 sm:p-5.5', contentClassName)}>
        <div className="mb-4.5 flex shrink-0 items-start gap-3">
          {icon ? (
            <div
              className={cn(
                'flex size-8 shrink-0 items-center justify-center rounded-lg ring-1 ring-inset',
                tone === 'danger'
                  ? 'bg-destructive/10 text-destructive ring-destructive/20'
                  : 'bg-muted/70 text-muted-foreground ring-border/60',
              )}
              aria-hidden="true"
            >
              <span className="[&_svg]:size-4">{icon}</span>
            </div>
          ) : null}
          <div className="min-w-0 flex-1 pt-0.5">
            <div className="flex flex-wrap items-center gap-x-2.5 gap-y-1">
              <h3 className="text-sm font-semibold leading-snug tracking-tight text-foreground sm:text-[15px]">
                {title}
              </h3>
              {badge}
              {channels && channels.length > 0 ? <ChannelScopeBadges channels={channels} /> : null}
            </div>
            {description ? (
              <p className="mt-1 text-xs leading-relaxed text-muted-foreground/90">{description}</p>
            ) : null}
          </div>
        </div>
        {children}
        {footer ? <div className="mt-4.5 border-t border-border/60 pt-4 sm:mt-5">{footer}</div> : null}
      </CardContent>
    </Card>
  )
}

export function SettingHelp({ text }: { text: string }) {
  return (
    <TooltipProvider delayDuration={200}>
      <Tooltip>
        <TooltipTrigger asChild>
          <button
            type="button"
            className="inline-flex size-4 shrink-0 items-center justify-center rounded-full text-muted-foreground/80 transition-colors hover:bg-muted hover:text-foreground"
            aria-label={text}
          >
            <CircleHelp className="size-3.5" />
          </button>
        </TooltipTrigger>
        <TooltipContent
          side="top"
          sideOffset={6}
          className="max-w-[280px] bg-popover px-3 py-2 text-left text-xs leading-relaxed text-popover-foreground shadow-md"
        >
          {text}
        </TooltipContent>
      </Tooltip>
    </TooltipProvider>
  )
}

export function SettingField({
  label,
  description,
  help,
  warning,
  children,
  className,
  layout = 'stack',
  suffix,
  channels,
  stretch = false,
}: {
  label: string
  description?: string
  // row 布局下 description 直接外显，help 才进问号 tooltip；其他布局 help 与 description 合并进 tooltip。
  help?: string
  warning?: string
  // stretch:stack 布局下让控件撑满剩余高度(等高卡片里的 textarea)。
  stretch?: boolean
  children: ReactNode
  className?: string
  layout?: 'stack' | 'switch' | 'row'
  suffix?: string
  channels?: readonly UpstreamChannel[]
}) {
  const scope = channels && channels.length > 0 ? <ChannelScopeBadges channels={channels} size="xs" /> : null
  const control = suffix ? (
    <div className="relative min-w-0">
      <div className="[&_[data-slot=input]]:pr-11 [&_[data-slot=select-trigger]]:pr-11 [&_input]:pr-11">
        {children}
      </div>
      <span className="pointer-events-none absolute right-3 top-1/2 -translate-y-1/2 text-[11px] font-medium tabular-nums text-muted-foreground">
        {suffix}
      </span>
    </div>
  ) : (
    children
  )

  if (layout === 'row') {
    return (
      <div className={cn('flex min-w-0 items-start justify-between gap-4 py-4 first:pt-0 last:pb-0', className)}>
        <div className="min-w-0 flex-1 space-y-1">
          <div className="flex flex-wrap items-center gap-1.5">
            <label className="text-[13px] font-semibold leading-snug text-foreground sm:text-sm">{label}</label>
            {help ? <SettingHelp text={help} /> : null}
            {scope}
          </div>
          {description ? (
            <p className="max-w-3xl text-xs leading-relaxed text-muted-foreground">{description}</p>
          ) : null}
          {warning ? (
            <p className="text-[11px] leading-relaxed text-amber-600 dark:text-amber-400 sm:text-xs">{warning}</p>
          ) : null}
        </div>
        <div className="flex shrink-0 items-center pt-0.5">{control}</div>
      </div>
    )
  }

  const tooltip = [description, help].filter(Boolean).join(' ')

  if (layout === 'switch') {
    return (
      <div
        className={cn(
          'flex min-h-[52px] min-w-0 items-center justify-between gap-3 rounded-xl border border-border/70 bg-card p-3.5 shadow-2xs transition-colors hover:border-border/90',
          className,
        )}
      >
        <div className="min-w-0 flex-1 space-y-0.5">
          <div className="flex items-center gap-1.5">
            <label className="block text-[13px] font-semibold leading-snug text-foreground sm:text-sm">
              {label}
            </label>
            {tooltip ? <SettingHelp text={tooltip} /> : null}
            {scope}
          </div>
          {warning ? (
            <p className="text-[11px] leading-relaxed text-amber-600 dark:text-amber-400 sm:text-xs">
              {warning}
            </p>
          ) : null}
        </div>
        <div className="flex shrink-0 items-center self-center">{control}</div>
      </div>
    )
  }

  return (
    <div className={cn('flex min-w-0 flex-col gap-1.5', stretch && 'flex-1', className)}>
      <div className="flex min-h-5 items-center gap-1.5">
        <label className="block text-[13px] font-semibold leading-none text-foreground sm:text-sm">
          {label}
        </label>
        {tooltip ? <SettingHelp text={tooltip} /> : null}
        {scope}
      </div>
      <div className={cn('min-w-0', stretch && 'flex flex-1 flex-col [&>*]:flex-1')}>{control}</div>
      {warning ? (
        <p className="text-[11px] leading-relaxed text-amber-600 dark:text-amber-400 sm:text-xs">
          {warning}
        </p>
      ) : null}
    </div>
  )
}
