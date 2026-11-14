// Bilibili account state for the music panel.
//
// The login itself is a QR flow: ask the host for a code, render it as an image,
// and poll until the account's cookies arrive. Everything account-scoped lives
// server-side; this module only mirrors the avatar/name for the UI.
import { writable } from 'svelte/store'

export interface BiliMe {
  loggedIn: boolean
  name: string
  face: string
}

export const biliMe = writable<BiliMe>({ loggedIn: false, name: '', face: '' })

export async function refreshBiliMe(): Promise<void> {
  try {
    const res = await fetch('/api/bili/me')
    const body = await res.json()
    biliMe.set({
      loggedIn: !!body?.logged_in,
      name: body?.name ?? '',
      face: body?.face ?? '',
    })
  } catch { /* offline: keep the last state */ }
}

export async function startBiliQR(): Promise<{ url: string; key: string } | null> {
  try {
    const res = await fetch('/api/bili/qr')
    if (!res.ok) return null
    const body = await res.json()
    if (!body?.url || !body?.key) return null
    return { url: body.url, key: body.key }
  } catch {
    return null
  }
}

/** Returns waiting | scanned | expired | ok. */
export async function pollBiliQR(key: string): Promise<string> {
  try {
    const res = await fetch(`/api/bili/qr/poll?key=${encodeURIComponent(key)}`)
    if (!res.ok) return 'waiting'
    const body = await res.json()
    if (body?.status === 'ok') {
      biliMe.set({
        loggedIn: !!body.logged_in,
        name: body.name ?? '',
        face: body.face ?? '',
      })
    }
    return body?.status ?? 'waiting'
  } catch {
    return 'waiting'
  }
}

export async function logoutBili(): Promise<void> {
  try {
    await fetch('/api/bili/logout', { method: 'POST' })
  } catch { /* ignore */ }
  biliMe.set({ loggedIn: false, name: '', face: '' })
}
