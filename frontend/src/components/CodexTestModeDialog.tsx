import { useState } from 'react'
import Modal from './Modal'
import { Button } from './ui/button'
import { Select } from './ui/select'

export const codexTestModeOptions = [
  { value: 'auto', label: '按账号配置' },
  { value: 'codex', label: 'Codex' },
  { value: 'bps', label: 'BPS' },
]

export default function CodexTestModeDialog({ onClose, onStart }: { onClose: () => void; onStart: (mode: string) => void }) {
  const [mode, setMode] = useState('auto')
  return <Modal show title="选择测试路径" onClose={onClose} footer={<>
    <Button variant="outline" onClick={onClose}>取消</Button>
    <Button onClick={() => onStart(mode)}>开始测试</Button>
  </>}>
    <Select value={mode} onValueChange={setMode} options={codexTestModeOptions} />
    <p className="mt-3 text-sm text-muted-foreground">仅用于本次测试，不修改账号配置或已有会话。不支持指定路径的账号会明确报错。</p>
  </Modal>
}
