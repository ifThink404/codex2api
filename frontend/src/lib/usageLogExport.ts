export async function confirmedUsageLogDownload(
  confirm: () => Promise<boolean>,
  download: () => Promise<Blob>,
  save: (blob: Blob) => void,
): Promise<boolean> {
  if (!await confirm()) return false
  const blob = await download()
  save(blob)
  return true
}

export function saveUsageLogExport(blob: Blob, scope: 'filtered' | 'all') {
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = `usage-logs-${scope}-${new Date().toISOString().replace(/[:.]/g, '-')}.json`
  document.body.appendChild(anchor)
  try {
    anchor.click()
  } finally {
    anchor.remove()
    setTimeout(() => URL.revokeObjectURL(url), 1000)
  }
}
