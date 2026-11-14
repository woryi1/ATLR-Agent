// Encrypted backup package: Settings → 数据管理 → 数据备份.
//
// The password never leaves this module: it goes straight into the request
// (JSON body for export, a header for uploads) and is not stored anywhere.

export interface BackupSection {
  id: string
  label: string
  hint: string
}

export interface ExportResult {
  path: string
  name: string
  size: number
  files: number
  sections: string[]
  created_at: string
}

export interface BackupSummary {
  sections: string[]
  restorable: string[]
  created_at: string
  files: number
  bytes: number
  app: string
  version: number
  size: number
}

export interface ApplyResult {
  sections: string[]
  files: number
}

async function jsonOrThrow<T>(res: Response): Promise<T> {
  const body = await res.json().catch(() => null)
  if (!res.ok) throw new Error(body?.error || `HTTP ${res.status}`)
  return body as T
}

export async function backupSections(): Promise<BackupSection[]> {
  const res = await fetch('/api/data/backup/sections')
  const body = await jsonOrThrow<{ sections: BackupSection[] }>(res)
  return body.sections ?? []
}

export async function exportBackup(sections: string[], password: string): Promise<ExportResult> {
  const res = await fetch('/api/data/backup/export', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ sections, password }),
  })
  return jsonOrThrow<ExportResult>(res)
}

export async function previewBackup(file: File, password: string): Promise<BackupSummary> {
  const fd = new FormData()
  fd.append('file', file, file.name)
  const res = await fetch('/api/data/backup/preview', {
    method: 'POST',
    headers: { 'X-Backup-Password': password },
    body: fd,
  })
  return jsonOrThrow<BackupSummary>(res)
}

export async function applyBackup(file: File, password: string, sections: string[]): Promise<ApplyResult> {
  const fd = new FormData()
  fd.append('file', file, file.name)
  const q = encodeURIComponent(sections.join(','))
  const res = await fetch(`/api/data/backup/apply?sections=${q}`, {
    method: 'POST',
    headers: { 'X-Backup-Password': password },
    body: fd,
  })
  return jsonOrThrow<ApplyResult>(res)
}

export async function openBackupFolder(dir?: string): Promise<void> {
  const q = dir ? `?dir=${encodeURIComponent(dir)}` : ''
  const res = await fetch(`/api/data/backup/open${q}`, { method: 'POST' })
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
}

/** Ask the desktop shell to relaunch the app (the server runs in-process). */
export async function restartApp(): Promise<void> {
  try {
    await fetch('/api/app/restart', { method: 'POST' })
  } catch { /* the process may already be gone */ }
}

/** True when the dropped file looks like one of our packages. */
export function isBackupFile(file: File): boolean {
  return file.name.toLowerCase().endsWith('.atlrbak')
}

export function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB']
  let i = 0
  let v = n
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++ }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`
}
