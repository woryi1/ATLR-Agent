// Frontend half of the video debug log.
//
// Every call appends one line to Data/Cache/video-debug.log through
// /api/debug-log, next to the Go proxy's own lines, so one playback session can
// be read as a single chronological trace. Console output is kept as well for
// in-page devtools.
let seq = 0

export function videoLog(event: string, detail?: Record<string, unknown>): void {
  const payload = { seq: ++seq, t: new Date().toISOString(), ...(detail ?? {}) }
  try {
    console.debug('[video]', event, payload)
  } catch { /* devtools closed */ }
  try {
    void fetch('/api/debug-log', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ event, detail: payload }),
      keepalive: true,
    }).catch(() => { /* diagnostic only */ })
  } catch { /* ignore */ }
}
