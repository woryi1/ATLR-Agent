<script lang="ts">
  // Version badge. This build has no update channel, so the badge is purely
  // informational: it shows the running server version (and the profile name,
  // when one is active) and nothing else.
  //
  // Reading /api/version is still load-bearing beyond the number itself: it
  // carries native/local/os/os_version, which decide whether the page behaves
  // as the desktop shell, whether it may use real filesystem paths, and how far
  // the titlebar rows must be inset past the macOS traffic lights.
  import { onMount } from 'svelte'
  import * as api from '../../lib/api'
  import { t } from '../../lib/i18n'
  import { nativeShell, localAccess, isDesktopShell, macosMajor } from '../../lib/stores'
  import { applyTitlebarLift } from '../../lib/nativeWindow'

  let current = $state('')
  let profile = $state('')

  let versionLabel = $derived(current ? `v${current}` : '')

  async function loadVersion() {
    try {
      const d = await api.getVersion() as any
      current = d.current ?? (d.version ?? '').replace(/^v/, '')
      profile = d.profile ?? ''
      nativeShell.set(d.native === true && isDesktopShell)
      localAccess.set(d.local === true)
      macosMajor.set(d.os === 'darwin' ? parseInt(d.os_version, 10) || 0 : 0)
      applyTitlebarLift()
    } catch { /* badge stays minimal */ }
  }

  onMount(loadVersion)
</script>

<div class="vb-wrap">
  {#if versionLabel}
    <span class="vb-badge" title={versionLabel}>
      <span class="vb-text">{versionLabel}</span>
      {#if profile}
        <span class="vb-profile" title={$t('version.profile').replace('{name}', profile)} aria-label={$t('version.profile').replace('{name}', profile)}>{profile}</span>
      {/if}
    </span>
  {/if}
</div>

<style>
.vb-wrap { position: relative; margin-left: auto; }
.vb-badge {
  display: flex; align-items: center; gap: 5px;
  padding: 2px 4px;
  font-size: 11px; color: var(--text-tertiary);
  font-family: inherit; border-radius: 4px;
}
.vb-text { font-variant-numeric: tabular-nums; }
.vb-profile {
  padding: 1px 4px; border: 1px solid var(--border); border-radius: 3px;
  color: var(--text-secondary); font-size: 10px; line-height: 1.2;
}
</style>
