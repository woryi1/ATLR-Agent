<script lang="ts">
  // Compact one-line balance + pricing strip at the top of the model menu.
  //
  // Fetch policy (deliberate, see .audit/14-feature-spec.md S3.3):
  //   • The component only mounts while the menu is open, so nothing polls on
  //     page load.
  //   • A module-level cache keeps a successful response for 60s — reopening
  //     the menu inside that window re-renders instantly without a request.
  //   • Manual refresh sends ?refresh=1 and is disabled for 10s afterwards
  //     (the server also rate-limits it; this just keeps the UI honest).
  //   • Every failure degrades silently: no toast, no dialog.
  //
  // The response shape is fixed by the backend contract but treated as
  // partial here — a missing field must never blank the strip or throw.
  //
  // Show policy: only what the backend can actually prove. supported:false
  // (provider has no balance API), an `error` without a usable balance, or a
  // dead request all mean there is nothing truthful to print, so the text line
  // is simply not rendered — no grey "not available" placeholder, no reserved
  // gap. The bolt stays regardless: it is the only door to Super mode.
  //
  // Super mode (the lightning at the far left) is a PROJECT-level switch:
  //   GET  /api/super-mode?group=<id>        → { enabled, scope }
  //   PUT  /api/super-mode?group=<id> {enabled} → { enabled, scope }
  // Without a project id the request falls back to the session's project (or,
  // with neither, the global config value), so a loose task behaves as before.
  // It is read whenever the project context changes and written on click. The
  // backend may not be deployed yet, so every failure degrades silently on
  // GET, and rolls the optimistic UI back (plus one error toast) on PUT.
  import { onDestroy, untrack } from 'svelte'
  import { t } from '../../lib/i18n'
  import { showToast, superMode, playSuperUnlock } from '../../lib/stores'

  let { sessionId = '', projectId = '', endpointId = '', model = '' } = $props()

  let superOn = $state(false)
  // Mirror the switch app-wide: the reasoning-tier artwork and the unlock
  // animation both need to know it, and this strip lives inside a menu that
  // closes the moment the user clicks away.
  $effect(() => { superMode.set(superOn) })
  let superBusy = $state(false)
  // Enable asks first — the warning is about cost, so it must be a deliberate
  // click. Disabling never asks. Inline bubble rather than ConfirmModal:
  // ConfirmModal is bound to the tool-permission store and only renders the
  // ACTIVE session's ws confirmation, so it cannot host a local prompt.
  let superAsk = $state(false)

  // Sequence stamp for Super reads/writes. Switch projects while a request is
  // in flight and the late answer must not paint this project's bolt with the
  // previous project's value.
  let superSeq = 0

  function superURL(proj: string, sid: string): string {
    const q = new URLSearchParams()
    // The SESSION wins over the project id: the switch a turn composes
    // against is the one belonging to the session's OWN project, and the
    // project id the view carries can be a different one (the user may be
    // browsing a project while chatting in a loose session, or a session
    // filed under another project). Sending both used to let the project id
    // decide, so the bolt could write to — and read from — a project the
    // running turn never consults, and the toggle appeared to do nothing.
    if (sid) q.set('session', sid)
    else if (proj) q.set('group', proj)
    const s = q.toString()
    return s ? `/api/super-mode?${s}` : '/api/super-mode'
  }

  // Runs on mount and whenever the project/session context changes. The bolt is
  // project state, so the old value is cleared first: a value from another
  // project must never flash on this one while the new GET is in flight.
  $effect(() => {
    const proj = projectId
    const sid = sessionId
    untrack(() => {
      superSeq++
      superOn = false
      superAsk = false
      void loadSuper(proj, sid)
    })
  })

  async function loadSuper(proj: string, sid: string) {
    const my = ++superSeq
    try {
      const res = await fetch(superURL(proj, sid))
      if (!res.ok) return
      const body = await res.json()
      if (my !== superSeq) return
      if (typeof body?.enabled === 'boolean') superOn = body.enabled
    } catch {
      // Backend not ready: stay grey and say nothing. Balance still renders.
    }
  }

  async function writeSuper(next: boolean) {
    const proj = projectId
    const sid = sessionId
    const my = ++superSeq
    const prev = superOn
    superOn = next
    superBusy = true
    try {
      const res = await fetch(superURL(proj, sid), {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ enabled: next }),
      })
      if (!res.ok) throw new Error(`${res.status}`)
      const body = await res.json().catch(() => null)
      if (my === superSeq && typeof body?.enabled === 'boolean') superOn = body.enabled
      // Switching Super ON earns the unlock animation; switching it off
      // does not.
      if (my === superSeq && next && superOn) playSuperUnlock('super')
    } catch {
      if (my === superSeq) superOn = prev
      showToast($t('chat.super.fail'), 'error')
    } finally {
      if (my === superSeq) superBusy = false
    }
  }

  function clickSuper() {
    if (superBusy) return
    if (superOn) {
      superAsk = false
      void writeSuper(false)
      return
    }
    superAsk = true
  }

  function confirmSuper() {
    superAsk = false
    void writeSuper(true)
  }

  const CACHE_MS = 60_000
  const COOLDOWN_MS = 10_000
  const cache = new Map<string, { at: number; data: any }>()

  let data = $state<any>(null)
  let loading = $state(false)
  let cooling = $state(false)
  let seq = 0
  let cooldownTimer: ReturnType<typeof setTimeout> | undefined

  // A session id is the preferred handle; without one the model ref (composite
  // endpoint_id + model) is the only thing the backend can resolve. An unknown
  // model (no list yet) leaves the key empty — nothing is requested.
  const key = $derived(
    sessionId ? `s:${sessionId}` : (model ? `m:${endpointId}::${model}` : ''),
  )

  async function load(refresh = false) {
    const k = key
    if (!k) return
    const hit = cache.get(k)
    if (!refresh && hit && Date.now() - hit.at < CACHE_MS) {
      data = hit.data
      return
    }
    const my = ++seq
    if (!data) loading = true
    try {
      const q = new URLSearchParams()
      if (sessionId) q.set('session_id', sessionId)
      else {
        if (endpointId) q.set('endpoint_id', endpointId)
        if (model) q.set('model', model)
      }
      if (refresh) q.set('refresh', '1')
      const res = await fetch(`/api/balance?${q.toString()}`)
      if (!res.ok) throw new Error(`${res.status}`)
      const body = await res.json()
      if (my !== seq) return
      data = body
      cache.set(k, { at: Date.now(), data: body })
    } catch {
      // Silent degrade: keep whatever we had, otherwise render nothing.
      if (my === seq) {
        if (!hit) data = null
      }
    } finally {
      if (my === seq) loading = false
    }
  }

  // Runs on mount and whenever the identity changes. Only the key is tracked;
  // the request itself touches state and must stay out of the dependency set.
  $effect(() => {
    const k = key
    if (!k) return
    untrack(() => { void load(false) })
  })

  function refresh() {
    if (cooling || loading) return
    cooling = true
    void load(true)
    cooldownTimer = setTimeout(() => { cooling = false }, COOLDOWN_MS)
  }

  onDestroy(() => clearTimeout(cooldownTimer))

  function num(v: any): string {
    if (v === null || v === undefined || v === '') return ''
    const n = Number(v)
    if (!Number.isFinite(n)) return String(v)
    return n.toFixed(2).replace(/(\.\d)0$/, '$1').replace(/\.00$/, '')
  }

  function currencySymbol(c: string): string {
    if (c === 'USD') return '$'
    if (c === 'CNY' || c === 'RMB' || !c) return '¥'
    return ''
  }

  function providerLabel(p: string): string {
    if (!p) return ''
    if (/deepseek/i.test(p)) return 'DeepSeek'
    if (/minimax/i.test(p)) return 'MiniMax'
    return p[0].toUpperCase() + p.slice(1)
  }

  function clock(iso: string): string {
    if (!iso) return ''
    try {
      const d = new Date(iso)
      if (Number.isNaN(d.getTime())) return ''
      // The window is defined in Beijing time; show it in the same zone
      // regardless of where this client runs.
      return new Intl.DateTimeFormat('en-GB', {
        hour: '2-digit', minute: '2-digit', hour12: false, timeZone: 'Asia/Shanghai',
      }).format(d)
    } catch { return '' }
  }

  // A stale snapshot still carries the last good value plus `error` — it is a
  // real number with an age tag, not a miss, so it must pass the checks below.
  const supported = $derived(data ? data.supported !== false : false)
  const amount = $derived(data?.supported === false ? '' : num(data?.balance?.available))
  const sym = $derived(currencySymbol(data?.currency ?? data?.pricing?.currency ?? 'CNY'))
  const provider = $derived(providerLabel(data?.provider ?? ''))

  const isPeak = $derived.by(() => {
    const p = data?.pricing?.period
    return p === 'peak'
  })
  const hasPricing = $derived(!!(data?.pricing && (data.pricing.output != null || data.pricing.input_cache_miss != null)))
  const periodText = $derived(
    data?.pricing
      ? (isPeak ? $t('balance.period.peak') : $t('balance.period.off_peak'))
      : '',
  )
  const ioText = $derived.by(() => {
    if (!hasPricing) return ''
    const inp = num(data.pricing.input_cache_miss ?? data.pricing.input)
    const out = num(data.pricing.output)
    if (!inp && !out) return ''
    return $t('balance.io').replace('{input}', inp || '?').replace('{output}', out || '?')
  })
  const planText = $derived.by(() => {
    const p = data?.plan
    if (!p) return ''
    if (typeof p.label === 'string' && p.label) return p.label
    const used = num(p.used)
    const total = num(p.total)
    if (!total) return typeof p.name === 'string' ? p.name : ''
    return $t('balance.plan').replace('{used}', used || '0').replace('{total}', total)
  })

  // Whether the text line has anything truthful to say. Order matters: a
  // provider without a balance API wins even if a stray field came along, and
  // a failed read is a miss unless it still carries a value worth showing
  // (`balance.available`, which is exactly what a stale replay carries).
  const available = $derived.by(() => {
    if (!data) return false
    if (!supported) return false
    if (data.error && data.balance?.available == null) return false
    return !!(amount || planText || hasPricing)
  })

  const tip = $derived.by(() => {
    if (!data) return ''
    const lines: string[] = []
    const b = data.balance
    if (b) {
      const parts: string[] = []
      if (b.cash != null) parts.push(`cash ${num(b.cash)}`)
      if (b.granted != null && num(b.granted) !== '0') parts.push(`granted ${num(b.granted)}`)
      if (b.topped_up != null && num(b.topped_up) !== '0') parts.push(`topped up ${num(b.topped_up)}`)
      if (b.owed != null && num(b.owed) !== '0') parts.push(`owed ${num(b.owed)}`)
      if (parts.length) lines.push(parts.join(' · '))
    }
    if (hasPricing) {
      lines.push($t('balance.cache_hit_tip').replace('{price}', num(data.pricing.input_cache_hit)))
      const at = clock(data.pricing.next_switch_at ?? '')
      if (at) lines.push($t('balance.next_switch').replace('{time}', at))
    }
    if (data.stale) lines.push($t('balance.stale'))
    return lines.join('\n')
  })
</script>

<!-- Rendered unconditionally: it lives in the model menu, and the bolt is the
     only entry to Super mode — it must not disappear along with a failed read. -->
<div class="bal">
  <div class="super-wrap">
    <button
      class="super"
      class:on={superOn}
      aria-pressed={superOn}
      aria-label={$t('chat.super.label')}
      title={available ? $t('chat.super.hint') : $t('chat.super.label')}
      disabled={superBusy}
      onclick={(e) => { e.stopPropagation(); clickSuper() }}
    >
      <iconify-icon icon={superOn ? 'ph:lightning-fill' : 'ph:lightning'} width="13"></iconify-icon>
    </button>
    {#if superAsk}
      <div class="super-confirm" role="dialog" onclick={(e) => e.stopPropagation()}>
        <div class="sc-title">{$t('chat.super.confirm_title')}</div>
        <div class="sc-body">{$t('chat.super.confirm_body')}</div>
        <div class="sc-actions">
          <button class="sc-cancel" onclick={() => { superAsk = false }}>{$t('common.cancel')}</button>
          <button class="sc-ok" onclick={confirmSuper}>{$t('chat.super.confirm_ok')}</button>
        </div>
      </div>
    {/if}
  </div>
  <!-- No balance to prove → the line does not exist: no grey placeholder and
       no reserved width. The row keeps its 20px height, so the menu does not
       jump when a value lands later. -->
  {#if available}
    <div class="bal-line" title={tip}>
      {#if provider}<span class="prov">{provider}</span>{/if}
      {#if amount}<span class="amt">{sym}{amount}</span>{/if}
      {#if planText}<span class="sep">·</span><span class="plan">{planText}</span>{/if}
      {#if periodText}
        <span class="sep">·</span>
        <span class="period" class:peak={isPeak} class:off={!isPeak}>{periodText}</span>
      {/if}
      {#if ioText}<span class="sep">·</span><span class="io">{ioText}</span>{/if}
      {#if data?.stale}<span class="tag">{$t('balance.stale')}</span>{/if}
      <button
        class="rf"
        class:cooling
        title={$t('balance.refresh')}
        aria-label={$t('balance.refresh')}
        disabled={cooling || loading}
        onclick={(e) => { e.stopPropagation(); refresh() }}
      >
        <iconify-icon icon="lucide:refresh-cw" width="11"></iconify-icon>
      </button>
    </div>
  {/if}
</div>

<style>
/* The row's 10px indent comes from .mm-head (padding: 9px 10px) in
   Composer.svelte. It adds none of its own on purpose: with the text line
   gone, only the bolt is left, and any extra padding here would push it off
   the vertical line shared by the effort slider underneath. */
.bal { min-height: 20px; display: flex; align-items: center; gap: 6px; }
.super-wrap { position: relative; flex: 0 0 auto; display: flex; }
/* Off by default: quiet grey, no chrome. This is a mode switch, not a badge —
   it must not compete with the balance it sits next to. */
.super {
  display: inline-flex; align-items: center; justify-content: center;
  width: 20px; height: 20px; padding: 0; border: none; border-radius: 5px;
  background: transparent; color: var(--text-tertiary); opacity: 0.45;
  cursor: pointer;
  transition: opacity 0.18s ease, color 0.18s ease, background 0.18s ease,
              transform 0.12s ease;
}
.super:hover:not(:disabled) { opacity: 0.8; color: var(--text-secondary); background: var(--row-hover); }
/* 120ms press feedback; the scale is the only thing that moves. */
.super:active:not(:disabled) { transform: scale(0.86); }
.super.on {
  opacity: 1; color: var(--warning); background: var(--warning-bg);
  animation: super-breathe 2.6s ease-in-out infinite;
}
.super.on:hover:not(:disabled) { opacity: 1; color: var(--warning); }
.super:focus-visible { outline: none; box-shadow: 0 0 0 2px var(--focus-ring); }
.super:disabled { cursor: default; }
@keyframes super-breathe {
  0%, 100% { box-shadow: 0 0 0 0 transparent; }
  50% { box-shadow: 0 0 7px 0 var(--warning-bg); }
}
/* Inline confirm. Anchored under the bolt and kept inside the menu's width —
   .menu.model-menu clips overflow, so a popover hanging outside would vanish. */
.super-confirm {
  position: absolute; top: 22px; left: 0; z-index: 6;
  width: 250px; padding: 10px 12px;
  background: var(--bg-container); border: 1px solid var(--border);
  border-radius: 10px; box-shadow: 0 10px 28px rgba(15,23,42,0.18);
  display: flex; flex-direction: column; gap: 7px;
}
.sc-title { font-size: 12.5px; font-weight: 600; color: var(--text); }
.sc-body { font-size: 12px; line-height: 1.55; color: var(--text-secondary); }
.sc-actions { display: flex; justify-content: flex-end; gap: 6px; }
.sc-cancel, .sc-ok {
  height: 26px; padding: 0 10px; border-radius: 6px;
  font-size: 12px; font-family: inherit; cursor: pointer;
}
.sc-cancel {
  border: 1px solid var(--border); background: var(--bg-container); color: var(--text-secondary);
}
.sc-cancel:hover { border-color: var(--blue-6); color: var(--blue-6); }
.sc-ok { border: none; background: var(--warning); color: #fff; }
.sc-ok:hover { filter: brightness(1.05); }
.bal-line {
  flex: 1 1 auto; min-width: 0;
  display: flex; align-items: center; gap: 4px; flex-wrap: nowrap;
  font-size: 11.5px; line-height: 20px; color: var(--text-secondary);
  white-space: nowrap; overflow: hidden;
}
.prov, .amt, .period, .tag { flex: 0 0 auto; }
.prov { color: var(--text); font-weight: 600; }
.amt { color: var(--text); font-family: var(--font-mono); }
.sep { flex: 0 0 auto; color: var(--text-tertiary); }
/* Only the price tail may truncate — provider, amount and period stay whole. */
.plan, .io { min-width: 0; flex: 0 1 auto; overflow: hidden; text-overflow: ellipsis; color: var(--text-secondary); }
.io { color: var(--text-tertiary); }
.period { font-weight: 600; }
.period.peak { color: var(--warning-text); }
.period.off { color: var(--success-text); }
.tag {
  padding: 0 4px; border-radius: 4px; font-size: 10px;
  color: var(--text-tertiary); border: 1px solid var(--border-secondary);
}
.rf {
  margin-left: auto; display: inline-flex; align-items: center; justify-content: center;
  width: 18px; height: 18px; padding: 0; border: none; border-radius: 5px;
  background: transparent; color: var(--text-tertiary); cursor: pointer;
  transition: color 0.15s ease, background 0.15s ease;
}
.rf:hover:not(:disabled) { color: var(--text); background: var(--row-hover); }
.rf:disabled { cursor: default; opacity: 0.45; }
/* The breathing glow is decoration; drop it when motion is unwelcome. */
@media (prefers-reduced-motion: reduce) {
  .super { transition-duration: 0.01ms !important; animation: none !important; }
  .super.on { animation: none !important; }
}
</style>
