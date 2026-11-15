<script lang="ts">
  import Segment from '../ui/Segment.svelte'
  import ThemePackPicker from '../ui/ThemePackPicker.svelte'
  import Switch from '../ui/Switch.svelte'
  import EndpointsSection from '../settings/EndpointsSection.svelte'
  import MobileSection from '../settings/MobileSection.svelte'
  import { storageKey } from '../../lib/storageKey'

  const FONT_SIZE_KEY = storageKey('fontSize')
  import ProfilesSection from '../settings/ProfilesSection.svelte'
  import FileRecallView from '../../views/FileRecallView.svelte'
  import ProfileView from '../../views/ProfileView.svelte'
  import { get } from 'svelte/store'
  import { showToast, nativeShell, settingsModalOpen, settingsTarget, onboardPhase, sessions, sessionGroups, collapsedSessions, activeSessionId, view, clearPendingSessionOpts } from '../../lib/stores'
  import type { Session, SessionGroup } from '../../lib/types'
  import { setLocale, t, tr } from '../../lib/i18n'
  import { getMode, setMode, getBackground, setBackground, getBackgroundAppearance, setBackgroundAppearance, getStartupAnim, setStartupAnim, getBackgroundVideo, setBackgroundVideo, getBackgroundVideoSound, setBackgroundVideoSound, type BackgroundFit, type BackgroundID, type BackgroundVideoID, type ThemeMode } from '../../lib/theme'
  import { notificationsEnabled, setNotificationsEnabled } from '../../lib/notifications'
  import { openUrl } from '../../lib/externalLinks'
  import { confirmDialog } from '../../lib/confirm'
  import { ago, clockTick } from '../../lib/relTime'
  import * as api from '../../lib/api'
  import {
    backupSections, exportBackup, previewBackup, applyBackup, openBackupFolder, restartApp,
    isBackupFile, formatBytes,
    type BackupSection, type BackupSummary, type ExportResult, type ApplyResult,
  } from '../../lib/backup'

  const LICENSE_URL = 'https://github.com/open-octo/octo-agent/blob/main/LICENSE.txt'
  const RELEASES_URL = 'https://github.com/open-octo/octo-agent/releases'
  const WEBSITE_URL = 'https://octo-agent.dev/'

  const fontZoomMap: Record<string, string> = { Small: '0.9', Medium: '1', Large: '1.1' }
  const modeToThemeLabel: Record<string, string> = { light: 'Light', dark: 'Dark', system: 'System' }

  // This modal is mounted unconditionally at app root, so the applying
  // $effects at the bottom run at boot, not on open. Seeding fontSize/theme
  // from the persisted prefs (not hardcoded defaults) is what keeps a page
  // reload from overwriting the stored choice before Settings is ever opened
  // (#2087) — and since nothing else reads the fontSize key, this seed is also
  // the font-size restore path.
  function storedFontSize(): string {
    const v = localStorage.getItem(FONT_SIZE_KEY)
    return v === 'Small' || v === 'Medium' || v === 'Large' ? v : 'Medium'
  }

  // --- local state ---
  let language      = $state('en')
  let fontSize      = $state(storedFontSize())
  let theme         = $state(modeToThemeLabel[getMode()] ?? 'Light')
  let bg            = $state<BackgroundID>(getBackground())
  let bgVideo       = $state<BackgroundVideoID>(getBackgroundVideo())
  let bgVideoSound  = $state(getBackgroundVideoSound())
  // One control for the two wallpaper sources: the server refuses to keep a
  // still image and a video at the same time, so a single three-way choice
  // states that rule instead of leaving it to two controls to agree.
  let bgKind        = $state<'off' | 'static' | 'video'>(getBackgroundVideo() ? 'video' : getBackground() && getBackground() !== 'off' ? 'static' : 'off')
  let bgFileInput   = $state<HTMLInputElement | null>(null)
  let bgFit         = $state<BackgroundFit>(getBackgroundAppearance().fit)
  let bgOpacity     = $state(getBackgroundAppearance().opacity)
  let bgBlur        = $state(getBackgroundAppearance().blur)
  let startupAnim   = $state(getStartupAnim())
  // 省 Token 技能开关：默认开，服务端 config.yml 的 force_token_skill 为准。
  let forceTokenSkill = $state(true)
  let tokenSkillLoaded = false
  $effect(() => {
    if (tokenSkillLoaded) return
    tokenSkillLoaded = true
    void api.getForceTokenSkill().then(v => { forceTokenSkill = v }).catch(() => {})
  })
  // 歌曲跳转使用系统浏览器（否则用应用内新窗口）。
  let musicOpenSystem = $state(true)
  let musicModeLoaded = false
  $effect(() => {
    if (musicModeLoaded) return
    musicModeLoaded = true
    void api.getMusicOpenMode().then(v => { musicOpenSystem = v }).catch(() => {})
  })
  let autostart     = $state(false) // desktop shell only
  let serverOs      = $state('')    // /api/version's os — gates the experimental tab
  let computerUse   = $state(false) // tools.computer.enabled toggle (experimental tab)
  const computerPlatform = $derived(serverOs === 'darwin' || serverOs === 'windows')
  // macOS prompts for two system grants; Windows has none but blocks input
  // into elevated apps — the hint under the toggle says which applies.
  const computerHintKey = $derived(serverOs === 'windows'
    ? 'settings.experimental.computer_use_hint_windows'
    : 'settings.experimental.computer_use_hint')
  let versionStr    = $state('')
  let loading       = $state(true)

  let cat = $state<'general' | 'endpoints' | 'agent' | 'mobile' | 'experimental' | 'data' | 'about'>('general')
  let modalEl = $state<HTMLDivElement | null>(null)

  // 数据管理 has its own two-level nav — a list of managed things, and one
  // sub-view per thing — because unlike every other category here it isn't a
  // handful of settings but potentially long lists (archived sessions, trashed
  // files). Reset whenever the category (or the whole modal) changes, so
  // leaving and returning to 数据管理 always lands on the list, not wherever
  // you left off.
  let dataSubView = $state<'none' | 'archived' | 'trash' | 'memory' | 'profiles' | 'backup'>('none')
  let archiveSearch = $state('')
  // '' = every project, 'none' = no project, else a project's group id.
  let archiveProjectFilter = $state('')
  let archiveSel = $state<Record<string, true>>({})

  function resetDataView() {
    dataSubView = 'none'
    archiveSearch = ''
    archiveProjectFilter = ''
    archiveSel = {}
  }

  // ── 数据备份 / 恢复 ──────────────────────────────────────────────────────
  // The password lives only in these variables and in the request body; it is
  // cleared after use and never written anywhere.
  let backupCatalog = $state<BackupSection[]>([])
  let backupMode = $state<'export' | 'import'>('export')
  let exportSel = $state<Record<string, boolean>>({ user: true, records: true, tools: true, work: false })
  let exportPwd = $state('')
  let exportPwd2 = $state('')
  let exportBusy = $state(false)
  let exportDone = $state<ExportResult | null>(null)
  let importFile = $state<File | null>(null)
  let importPwd = $state('')
  let importBusy = $state(false)
  let importSummary = $state<BackupSummary | null>(null)
  let importSel = $state<Record<string, boolean>>({})
  let importDone = $state<ApplyResult | null>(null)
  let importError = $state('')
  let dropActive = $state(false)
  let backupLoaded = false
  let backupInput = $state<HTMLInputElement | null>(null)

  async function loadBackupCatalog(): Promise<void> {
    if (backupLoaded) return
    try {
      backupCatalog = await backupSections()
      backupLoaded = true
    } catch {
      // Keep the panel usable if the sections call fails.
      backupCatalog = [
        { id: 'user', label: '设置与凭据', hint: 'API Key、模型配置、登录态、主题、上传附件' },
        { id: 'records', label: '会话与记忆', hint: '聊天记录、项目记忆、定时任务、工作流日志' },
        { id: 'tools', label: '技能与智能体', hint: '技能、子智能体、MCP 配置与令牌' },
        { id: 'work', label: '工作目录', hint: 'Work 下的项目文件，可能很大' },
      ]
    }
  }

  function sectionLabel(id: string): string {
    return backupCatalog.find(s => s.id === id)?.label ?? id
  }

  async function runExport(): Promise<void> {
    const ids = Object.keys(exportSel).filter(id => exportSel[id])
    if (ids.length === 0) { showToast('至少选择一项要导出的数据', 'error'); return }
    if (exportPwd.length < 6) { showToast('密码至少 6 位', 'error'); return }
    if (exportPwd !== exportPwd2) { showToast('两次输入的密码不一致', 'error'); return }
    exportBusy = true
    exportDone = null
    try {
      exportDone = await exportBackup(ids, exportPwd)
      exportPwd = ''
      exportPwd2 = ''
      showToast('加密备份已导出', 'success')
    } catch (e: any) {
      showToast(e?.message || '导出失败', 'error')
    } finally {
      exportBusy = false
    }
  }

  /** Accept a picked or dropped package and jump straight to the import form. */
  function acceptPackage(file: File | null | undefined): void {
    if (!file) return
    if (!isBackupFile(file)) { showToast('这不是 ATLR 备份包（.atlrbak）', 'error'); return }
    importFile = file
    importPwd = ''
    importSummary = null
    importDone = null
    importError = ''
    backupMode = 'import'
    cat = 'data'
    dataSubView = 'backup'
    void loadBackupCatalog()
  }

  async function runPreview(): Promise<void> {
    if (!importFile) { showToast('请选择备份包', 'error'); return }
    if (!importPwd) { showToast('请输入备份密码', 'error'); return }
    importBusy = true
    importError = ''
    try {
      const sum = await previewBackup(importFile, importPwd)
      importSummary = sum
      const sel: Record<string, boolean> = {}
      for (const id of sum.restorable ?? []) sel[id] = true
      importSel = sel
    } catch (e: any) {
      importSummary = null
      importError = e?.message || '解析失败'
    } finally {
      importBusy = false
    }
  }

  async function runImport(): Promise<void> {
    if (!importFile || !importSummary) return
    const ids = Object.keys(importSel).filter(id => importSel[id])
    if (ids.length === 0) { showToast('至少选择一项要恢复的数据', 'error'); return }
    const personal = ids.some(id => id !== 'work')
    const warning = personal
      ? '⚠️ 你正在导入个人数据（设置 / 会话 / 技能）。这会直接替换并删除现有的重要数据，且不会生成任何备份，无法恢复。\n\n确定继续吗？'
      : '将用备份包替换当前的 Work 工作目录，现有文件会被删除。确定继续吗？'
    if (!(await confirmDialog(warning))) return
    importBusy = true
    try {
      importDone = await applyBackup(importFile, importPwd, ids)
      showToast('数据已恢复，3 秒后自动重启', 'success')
      scheduleRestart()
    } catch (e: any) {
      showToast(e?.message || '恢复失败', 'error')
    } finally {
      importBusy = false
    }
  }

  // ── 导入后自动重启 ──────────────────────────────────────────────────────
  let restartCountdown = $state(0)
  let restartTimer: ReturnType<typeof setTimeout> | null = null

  function scheduleRestart(): void {
    cancelRestart()
    restartCountdown = 3
    const tick = () => {
      restartCountdown -= 1
      if (restartCountdown <= 0) {
        restartTimer = null
        void restartApp()
        return
      }
      restartTimer = setTimeout(tick, 1000)
    }
    restartTimer = setTimeout(tick, 1000)
  }

  function cancelRestart(): void {
    if (restartTimer) clearTimeout(restartTimer)
    restartTimer = null
    restartCountdown = 0
  }

  function onBackupDrop(e: DragEvent): void {
    e.preventDefault()
    dropActive = false
    acceptPackage(e.dataTransfer?.files?.[0])
  }

  const archivedSessions = $derived($sessions.filter(s => $collapsedSessions.includes(s.id)))

  // Archiving keeps a session's project membership (see handleSetSessionCollapsed
  // on the server), so an archived session's project is looked up the same way
  // Sidebar.svelte does it for a live one: by scanning which project's
  // session_ids includes it. Needed both to group the list and to offer the
  // project filter.
  const projectBySession = $derived.by(() => {
    const m = new Map<string, SessionGroup>()
    for (const g of $sessionGroups) {
      if (!g.working_dir) continue
      for (const id of g.session_ids) m.set(id, g)
    }
    return m
  })

  const archivedFiltered = $derived.by(() => {
    const q = archiveSearch.trim().toLowerCase()
    return archivedSessions.filter(s => {
      if (q && !nameOf(s).toLowerCase().includes(q)) return false
      if (archiveProjectFilter === 'none') return !projectBySession.get(s.id)
      if (archiveProjectFilter) return projectBySession.get(s.id)?.id === archiveProjectFilter
      return true
    })
  })

  // One bucket per project, in the sidebar's own project order, then a final
  // "no project" bucket — same shape as the sidebar's own Tasks-after-Projects
  // ordering, just projects first here since a filtered-to-one-project view is
  // the common case this list exists for.
  const archivedGroups = $derived.by(() => {
    const buckets = new Map<string, { group: SessionGroup | null; items: Session[] }>()
    for (const s of archivedFiltered) {
      const g = projectBySession.get(s.id) ?? null
      const key = g ? g.id : ''
      if (!buckets.has(key)) buckets.set(key, { group: g, items: [] })
      buckets.get(key)!.items.push(s)
    }
    const ordered: { group: SessionGroup | null; items: Session[] }[] = []
    for (const g of $sessionGroups) {
      if (buckets.has(g.id)) ordered.push(buckets.get(g.id)!)
    }
    if (buckets.has('')) ordered.push(buckets.get('')!)
    return ordered
  })

  // Only projects that actually have an archived session offer a filter for —
  // an empty option nobody could ever pick is not a choice, it's clutter.
  const archiveProjectOptions = $derived(
    $sessionGroups.filter(g => !!g.working_dir && archivedSessions.some(s => projectBySession.get(s.id)?.id === g.id)),
  )

  function nameOf(s: Session): string {
    return (s as any).name || (s as any).title || s.id
  }

  function toggleArchiveSel(id: string) {
    const n = { ...archiveSel }
    if (n[id]) delete n[id]
    else n[id] = true
    archiveSel = n
  }

  function toggleArchiveSelAll() {
    const ids = archivedFiltered.map(s => s.id)
    const allSelected = ids.length > 0 && ids.every(id => archiveSel[id])
    if (allSelected) {
      archiveSel = {}
    } else {
      const n: Record<string, true> = {}
      for (const id of ids) n[id] = true
      archiveSel = n
    }
  }

  async function unarchiveSession(id: string) {
    const before = get(collapsedSessions)
    collapsedSessions.set(before.filter(x => x !== id))
    try {
      await api.setSessionCollapsed(id, false)
    } catch (e: any) {
      collapsedSessions.set(before)
      showToast(e?.message ?? tr('sidebar.collapse_failed'), 'error')
    }
  }

  async function deleteArchivedSession(id: string) {
    if (!(await confirmDialog(tr('sidebar.confirm_delete')))) return
    try {
      await api.deleteSession(id)
      sessions.update(ss => ss.filter(s => s.id !== id))
      collapsedSessions.update(ids => ids.filter(x => x !== id))
      const { [id]: _drop, ...rest } = archiveSel
      archiveSel = rest
      if (get(activeSessionId) === id) {
        activeSessionId.set(null)
        clearPendingSessionOpts()
        view.set('chat')
      }
    } catch (e: any) {
      showToast(e.message, 'error')
    }
  }

  // Deletes every selected session in one confirmation rather than one
  // per-row — the whole point of selecting several at once. Sessions that
  // fail to delete are reported and kept selected, so retrying is just
  // pressing the button again on what's left.
  async function deleteSelectedArchived() {
    const ids = Object.keys(archiveSel)
    if (ids.length === 0) return
    if (!(await confirmDialog(tr('settings.data.confirm_delete_selected').replace('{n}', String(ids.length))))) return
    const failed: string[] = []
    for (const id of ids) {
      try {
        await api.deleteSession(id)
        sessions.update(ss => ss.filter(s => s.id !== id))
        collapsedSessions.update(cs => cs.filter(x => x !== id))
        if (get(activeSessionId) === id) {
          activeSessionId.set(null)
          clearPendingSessionOpts()
          view.set('chat')
        }
      } catch {
        failed.push(id)
      }
    }
    if (failed.length) {
      const n: Record<string, true> = {}
      for (const id of failed) n[id] = true
      archiveSel = n
      showToast(tr('settings.data.delete_selected_partial').replace('{n}', String(failed.length)), 'error')
    } else {
      archiveSel = {}
    }
  }

  // ── Agent defaults — each control saves immediately on change (see the
  // save* functions below), no separate Save button.
  let reasoningEffort  = $state('off')
  let permissionMode   = $state('interactive')
  let showReasoningVal = $state(true)
  let coauthorVal      = $state(true)
  let workspaceDir        = $state('')
  // Resolved default new sessions get when workspaceDir is empty (~/Octo,
  // expanded server-side) — shown as the input's placeholder instead of a
  // bare "auto" that's easy to mistake for an actually-saved value.
  let workspaceDirDefault = $state('')

  const langOptions = [
    { value: 'en', label: 'English' },
    { value: 'zh', label: '简体中文' },
  ]

  const categories: { key: typeof cat, icon: string, label: string }[] = $derived([
    { key: 'general',   icon: 'ant-design:sliders-outlined',       label: 'settings.general' },
    { key: 'endpoints', icon: 'ant-design:api-outlined',           label: 'settings.endpoints.title' },
    { key: 'agent',     icon: 'ant-design:robot-outlined',         label: 'settings.agent' },
    { key: 'mobile',    icon: 'ant-design:mobile-outlined',        label: 'settings.mobile' },
    // Experimental features (computer-use) need a platform with a substrate —
    // macOS (AX/CGEvent) or Windows (UI Automation/SendInput) on the machine
    // running octo. Shown on web too: a CLI/serve octo works the same, its
    // macOS Screen Recording / Accessibility grants just go to the hosting
    // terminal app instead of the desktop app.
    ...(computerPlatform
      ? [{ key: 'experimental' as const, icon: 'ant-design:experiment-outlined', label: 'settings.experimental' }]
      : []),
    { key: 'data',      icon: 'ant-design:database-outlined',       label: 'settings.data' },
    { key: 'about',     icon: 'ant-design:info-circle-outlined',   label: 'settings.about' },
  ])

  // Re-seed on every open, same as the other global modals — reflects
  // whatever config was saved elsewhere (agent chat, another window) since
  // the modal last closed.
  $effect(() => {
    if ($settingsModalOpen) {
      // A deep link (command palette, in-app shortcut) picks the category and
      // 数据管理 sub-view; a plain open falls back to the default landing.
      const target = get(settingsTarget)
      cat = (target?.cat as typeof cat) ?? 'general'
      resetDataView()
      if (target?.dataSubView) dataSubView = target.dataSubView as typeof dataSubView
      if (target) settingsTarget.set(null)
      loadConfig()
      loadVersion()
      if (get(nativeShell)) api.getAutostart().then(v => (autostart = v)).catch(() => {})
      theme = modeToThemeLabel[getMode()] ?? 'Light'
      fontSize = storedFontSize()
      // Theme and wallpapers are reconciled with the server after boot, so the
      // boot seed can be behind by the time this opens — re-read the live
      // values instead of showing whatever the seed captured.
      bg = getBackground()
      bgVideo = getBackgroundVideo()
      bgVideoSound = getBackgroundVideoSound()
      bgKind = bgVideo ? 'video' : bg && bg !== 'off' ? 'static' : 'off'
      modalEl?.focus()
    }
  })

  async function loadConfig() {
    loading = true
    try {
      const cfg = await api.getConfig() as any
      // Absent = old server dropping "" through omitempty = reasoning off.
      reasoningEffort  = cfg.reasoning_effort ?? 'off'
      permissionMode   = cfg.permission_mode ?? 'interactive'
      showReasoningVal = cfg.show_reasoning ?? true
      coauthorVal      = cfg.coauthor ?? true
      computerUse      = (cfg.computer_enabled ?? '') === 'on'
      // Legacy installer-seeded "auto" resolves to the same default as ""
      // (see tools.ResolveWorkspaceDir) — show it as the empty input with
      // the resolved-default placeholder, not as a literal "auto" the user
      // would mistake for a real path.
      const wd = cfg.workspace_dir ?? ''
      workspaceDir        = wd.trim().toLowerCase() === 'auto' ? '' : wd
      workspaceDirDefault = cfg.workspace_dir_default ?? ''
      if (cfg.language) language = cfg.language
      setLocale(cfg.language === 'zh' || cfg.language === 'zh-TW' ? 'zh' : 'en')
    } catch (e: any) {
      showToast(`Failed to load config: ${e.message}`, 'error')
    } finally {
      loading = false
    }
  }

  async function loadVersion() {
    try {
      const v = await api.getVersion() as any
      versionStr = v.current ?? v.version ?? ''
      serverOs = v.os ?? ''
    } catch { /* non-critical */ }
  }

  async function toggleAutostart(v: boolean) {
    try {
      await api.setAutostart(v)
      autostart = v
    } catch (e: any) {
      showToast(e.message ?? 'Failed to change autostart', 'error')
      autostart = !v
    }
  }

  // ── agent defaults — each saves immediately via its own PUT endpoint
  // (internal/skills/defaults/config-setup/SKILL.md documents the same 5
  // endpoints for the conversational path; this is the identical contract,
  // just called directly instead of through the agent).
  async function saveReasoningEffort(v: string) {
    try {
      await api.updateReasoningEffort(v)
      reasoningEffort = v
    } catch (e: any) {
      showToast(e.message ?? 'Failed to update reasoning effort', 'error')
    }
  }

  async function savePermissionMode(v: string) {
    try {
      await api.updatePermissionMode(v)
      permissionMode = v
    } catch (e: any) {
      showToast(e.message ?? 'Failed to update permission mode', 'error')
    }
  }

  async function saveShowReasoning(v: boolean) {
    try {
      await api.updateShowReasoning(v)
      showReasoningVal = v
    } catch (e: any) {
      showToast(e.message ?? 'Failed to update show-reasoning', 'error')
    }
  }

  async function saveCoauthor(v: boolean) {
    try {
      await api.updateCoauthor(v)
      coauthorVal = v
    } catch (e: any) {
      showToast(e.message ?? 'Failed to update coauthor', 'error')
    }
  }

  async function saveComputerUse(v: boolean) {
    try {
      await api.updateComputerEnabled(v)
      computerUse = v
    } catch (e: any) {
      showToast(e.message ?? 'Failed to update computer-use', 'error')
    }
  }

  async function saveWorkspaceDir(v: string) {
    try {
      await api.updateWorkspaceDir(v)
      workspaceDir = v
    } catch (e: any) {
      showToast(e.message ?? 'Failed to update workspace directory', 'error')
    }
  }

  // Persist the language like every other config-backed field in this modal.
  // The $effect below already applies it to the live locale store; without
  // this PUT the choice only lived in memory and a refresh reverted to the
  // server's stored language (#2076). FirstRunSetup persists through the same
  // endpoint.
  async function saveLanguage(v: string) {
    try {
      await api.updateLanguage(v)
    } catch (e: any) {
      showToast(e.message ?? 'Failed to update language', 'error')
    }
  }

  function close() {
    settingsModalOpen.set(false)
  }

  // Re-runs the same blocking first-run panel a fresh install shows (App.svelte
  // gates <FirstRunSetup /> on this store) — lets a configured user redo the
  // model/browser setup + personalization chat without wiping any config first.
  function rerunFirstRun() {
    settingsModalOpen.set(false)
    onboardPhase.set('key_setup')
  }

  $effect(() => {
    const zoom = fontZoomMap[fontSize] ?? '1'
    ;(document.documentElement.style as any).zoom = zoom
    // Viewport units are not compensated for zoom — publish the factor so
    // vh/vw-sized boxes can divide themselves back onto the real viewport.
    document.documentElement.style.setProperty('--font-zoom', zoom)
    localStorage.setItem(FONT_SIZE_KEY, fontSize)
  })

  const themeLabelToMode: Record<string, ThemeMode> = { Light: 'light', Dark: 'dark', System: 'system' }
  $effect(() => {
    const m = themeLabelToMode[theme] ?? 'light'
    // Only write on a real change: at boot the seed mirrors localStorage while
    // loadThemeSelection may have just reconciled the server's different
    // choice — re-persisting the stale seed would clobber it.
    if (m !== getMode()) setMode(m)
  })

  // "custom" doesn't apply immediately — it opens the file picker first, and
  // the wallpaper lands only after a successful upload (onBgFilePicked).
  function onBackgroundChange(v: BackgroundID) {
    // Re-seed the appearance controls from the current truth: the boot-time
    // seed may predate the server reconciliation (loadThemeSelection).
    ;({ fit: bgFit, opacity: bgOpacity, blur: bgBlur } = getBackgroundAppearance())
    if (v === 'custom') {
      bgFileInput?.click()
      return
    }
    setBackground(v)
  }

  function applyBgAppearance() {
    setBackgroundAppearance(bgFit, bgOpacity, bgBlur)
  }

  // The three-way control: off / still image / animated. Both setters already
  // clear the other source, which is exactly the server's rule, so this only
  // routes to the right one.
  function onBgKindChange(v: string) {
    if (v === 'off') {
      bg = 'off'
      bgVideo = ''
      setBackground('off')
      return
    }
    if (v === 'video') {
      bgVideo = 'sakura'
      setBackgroundVideo('sakura')
      return
    }
    // Restore the remembered still pick, falling back to the first built-in so
    // switching to "still" always lands on something visible.
    const next: BackgroundID = bg && bg !== 'off' ? bg : 'bloom'
    if (next === 'custom') {
      // The uploaded file is still served; re-picking would force a fresh
      // upload for no reason.
      bg = 'custom'
      setBackground('custom')
      return
    }
    onBackgroundChange(next)
  }

  async function onBgFilePicked(e: Event) {
    const input = e.currentTarget as HTMLInputElement
    const file = input.files?.[0]
    input.value = '' // allow re-picking the same file after a failed upload
    if (!file) return
    try {
      await api.uploadCustomBackground(file)
      showToast(tr('settings.background.uploaded'), 'success')
      bg = 'custom'
      setBackground('custom')
    } catch {
      showToast(tr('settings.background.upload_fail'), 'error')
    }
  }

  $effect(() => {
    setLocale(language === 'zh' || language === 'zh-TW' ? 'zh' : 'en')
  })
</script>

{#if $settingsModalOpen}
<div class="backdrop" role="presentation" onclick={close}>
  <div
    class="modal"
    bind:this={modalEl}
    role="dialog"
    aria-modal="true"
    tabindex="-1"
    onclick={(e) => e.stopPropagation()}
    ondragover={(e) => { e.preventDefault(); dropActive = true }}
    ondragleave={() => (dropActive = false)}
    ondrop={onBackupDrop}
    onkeydown={(e) => { if (e.key === 'Escape') { e.preventDefault(); close() } }}
  >
    <div class="modal-header">
      <span class="modal-title">{$t('settings.title')}</span>
      <button class="modal-close" onclick={close} aria-label={$t('common.close')}>
        <iconify-icon icon="ant-design:close-outlined" width="14"></iconify-icon>
      </button>
    </div>
    {#if dropActive}
      <div class="backup-drop-overlay">
        <iconify-icon icon="lucide:package-open" width="34"></iconify-icon>
        <span>松开以导入备份包</span>
      </div>
    {/if}

    <div class="modal-body">
      <div class="rail">
        {#each categories as c (c.key)}
          <div class="scat" class:on={cat === c.key} onclick={() => { cat = c.key; resetDataView() }}>
            <iconify-icon icon={c.icon} width="15"></iconify-icon>
            <span>{$t(c.label)}</span>
          </div>
        {/each}
      </div>

      <div class="pane">
        {#if loading}
          <div class="loading-state">{$t('settings.loading')}</div>
        {:else if cat === 'general'}
          <div class="setrow">
            <div class="seti">
              <span class="setl">{$t('settings.language')}</span>
              <span class="setd">{$t('settings.language_desc')}</span>
            </div>
            <select class="sinput" bind:value={language} onchange={() => saveLanguage(language)}>
              {#each langOptions as o}
                <option value={o.value}>{o.label}</option>
              {/each}
            </select>
          </div>
          <div class="setrow">
            <div class="seti">
              <span class="setl">{$t('settings.font_size')}</span>
              <span class="setd">{$t('settings.font_size_desc')}</span>
            </div>
            <Segment options={['Small', 'Medium', 'Large']} labels={{ Small: $t('settings.fs_small'), Medium: $t('settings.fs_medium'), Large: $t('settings.fs_large') }} bind:value={fontSize} />
          </div>
          <!-- Theme before Appearance: the pack is the bigger choice, and
               appearance reads as a modifier of it rather than the reverse. -->
          <div class="setrow">
            <div class="seti">
              <span class="setl">{$t('settings.pack')}</span>
              <span class="setd">{$t('settings.pack_desc')}</span>
            </div>
            <ThemePackPicker />
          </div>
          <div class="setrow">
            <div class="seti">
              <span class="setl">{$t('settings.theme')}</span>
              <span class="setd">{$t('settings.theme_desc')}</span>
            </div>
            <Segment options={['Light', 'Dark', 'System']} labels={{ Light: $t('settings.theme_light'), Dark: $t('settings.theme_dark'), System: $t('settings.theme_system') }} bind:value={theme} />
          </div>
          <!-- Wallpaper: three exclusive states — off, a still image, or the
               built-in animated one. The server refuses to keep an image and a
               video at once, so one control states that rule outright. -->
          <div class="setrow">
            <div class="seti">
              <span class="setl">{$t('settings.background')}</span>
              <span class="setd">{$t('settings.background_desc')}</span>
            </div>
            <Segment
              options={['off', 'static', 'video']}
              labels={{
                off: $t('settings.background.kind_off'),
                static: $t('settings.background.kind_static'),
                video: $t('settings.background.kind_video'),
              }}
              value={bgKind}
              onchange={(v) => onBgKindChange(v)}
            />
          </div>
          {#if bgKind === 'static'}
            <!-- "custom" opens a picker first; the wallpaper applies only after
                 a successful upload, so the select never shows a choice the
                 server can't serve. -->
            <div class="setrow">
              <div class="seti">
                <span class="setl">{$t('settings.background.kind_static')}</span>
              </div>
              <select class="sinput" bind:value={bg} onchange={() => onBackgroundChange(bg)}>
                <option value="bloom">{$t('settings.background.bloom')}</option>
                <option value="mystery">{$t('settings.background.mystery')}</option>
                <option value="meadow">{$t('settings.background.meadow')}</option>
                <option value="custom">{$t('settings.background.custom')}</option>
              </select>
            </div>
            {#if bg === 'custom'}
              <div class="setrow">
                <div class="seti">
                  <span class="setl"></span>
                  <span class="setd">{$t('settings.background.upload')}</span>
                </div>
                <button class="link-btn" onclick={() => bgFileInput?.click()}>{$t('settings.background.upload')}</button>
              </div>
            {/if}
          {/if}
          {#if bgKind === 'video'}
            <div class="setrow">
              <div class="seti">
                <span class="setl">{$t('settings.background.video_name')}</span>
                <span class="setd">{$t('settings.background.video_meta')}</span>
              </div>
              <video
                class="bg-video-thumb"
                muted
                playsinline
                preload="metadata"
                src="/api/backgrounds/video/sakura"
              ></video>
            </div>
            <div class="setrow">
              <div class="seti">
                <span class="setl">{$t('settings.background.video_sound')}</span>
                <span class="setd">{$t('settings.background.video_sound_desc')}</span>
              </div>
              <Switch checked={bgVideoSound} onchange={(v) => { bgVideoSound = v; setBackgroundVideoSound(v) }} />
            </div>
          {/if}
          <input
            type="file"
            accept="image/jpeg,image/png,image/webp"
            style="display: none"
            bind:this={bgFileInput}
            onchange={(e) => onBgFilePicked(e)}
          />
          <!-- Boot splash: the vendored dsh-startup-screen runtime plays the
               DeepSeek-style boot sequence on launch. Off skips straight to
               the app (the index.html inline script reads the same flag). -->
          <div class="setrow">
            <div class="seti">
              <span class="setl">{$t('settings.startup_anim')}</span>
              <span class="setd">{$t('settings.startup_anim_desc')}</span>
            </div>
            <Switch checked={startupAnim} onchange={(v) => { startupAnim = v; setStartupAnim(v) }} />
          </div>
          <!-- 省 Token：每一句用户消息都强制先加载内置技能。 -->
          <div class="setrow">
            <div class="seti">
              <span class="setl">{$t('settings.force_token_skill')}</span>
              <span class="setd">{$t('settings.force_token_skill_desc')}</span>
            </div>
            <Switch
              checked={forceTokenSkill}
              onchange={(v) => {
                forceTokenSkill = v
                void api.putForceTokenSkill(v).catch(() => { forceTokenSkill = !v })
              }}
            />
          </div>
          <!-- 歌曲跳转：系统浏览器 / 应用内新窗口。 -->
          <div class="setrow">
            <div class="seti">
              <span class="setl">{$t('settings.music_open_system')}</span>
              <span class="setd">{$t('settings.music_open_system_desc')}</span>
            </div>
            <Switch
              checked={musicOpenSystem}
              onchange={(v) => {
                musicOpenSystem = v
                void api.putMusicOpenMode(v).catch(() => { musicOpenSystem = !v })
              }}
            />
          </div>
          {#if (bg && bg !== 'off') || bgVideo}
            <div class="setrow">
              <div class="seti">
                <span class="setl">{$t('settings.background.fit')}</span>
                <span class="setd"></span>
              </div>
              <select class="sinput" bind:value={bgFit} onchange={() => applyBgAppearance()}>
                <option value="cover">{$t('settings.background.fit_cover')}</option>
                <option value="contain">{$t('settings.background.fit_contain')}</option>
                <option value="fill">{$t('settings.background.fit_fill')}</option>
              </select>
            </div>
            <div class="setrow">
              <div class="seti">
                <span class="setl">{$t('settings.background.opacity')}</span>
                <span class="setd">{Math.round(bgOpacity * 100)}%</span>
              </div>
              <input class="bgrange" type="range" min="0.05" max="0.8" step="0.05" bind:value={bgOpacity} oninput={() => applyBgAppearance()} />
            </div>
            <div class="setrow">
              <div class="seti">
                <span class="setl">{$t('settings.background.blur')}</span>
                <span class="setd">{bgBlur}px</span>
              </div>
              <input class="bgrange" type="range" min="0" max="30" step="1" bind:value={bgBlur} oninput={() => applyBgAppearance()} />
            </div>
          {/if}
          {#if $nativeShell}
            <div class="setrow">
              <div class="seti">
                <span class="setl">{$t('settings.autostart')}</span>
                <span class="setd">{$t('settings.autostart_desc')}</span>
              </div>
              <Switch checked={autostart} onchange={(v) => toggleAutostart(v)} />
            </div>
          {/if}
          <div class="setrow">
            <div class="seti">
              <span class="setl">{$t('settings.notifications')}</span>
              <span class="setd">{$t('settings.notifications_desc')}</span>
            </div>
            <Switch checked={$notificationsEnabled} onchange={(v) => setNotificationsEnabled(v)} />
          </div>

        {:else if cat === 'endpoints'}
          <EndpointsSection />

        {:else if cat === 'agent'}
          <div class="setrow">
            <div class="seti">
              <span class="setl">{$t('settings.reasoning')}</span>
              <span class="setd">{$t('settings.reasoning_desc')}</span>
            </div>
            <Segment
              options={['low', 'medium', 'high', 'xhigh', 'max']}
              labels={{
                low: $t('models.reasoning.low'), medium: $t('models.reasoning.medium'), high: $t('models.reasoning.high'),
                xhigh: $t('models.reasoning.xhigh'), max: $t('models.reasoning.max'),
              }}
              value={reasoningEffort}
              onchange={(v) => saveReasoningEffort(v)}
            />
          </div>
          <div class="setrow">
            <div class="seti">
              <span class="setl">{$t('settings.perm_mode')}</span>
              <span class="setd">{$t('settings.perm_mode_desc')}</span>
            </div>
            <Segment
              options={['interactive', 'auto', 'strict']}
              labels={{
                interactive: $t('settings.perm_mode.interactive'), auto: $t('settings.perm_mode.auto'), strict: $t('settings.perm_mode.strict'),
              }}
              value={permissionMode}
              onchange={(v) => savePermissionMode(v)}
            />
          </div>
          <div class="setrow">
            <div class="seti">
              <span class="setl">{$t('settings.show_reasoning')}</span>
              <span class="setd">{$t('settings.show_reasoning_desc')}</span>
            </div>
            <Switch checked={showReasoningVal} onchange={(v) => saveShowReasoning(v)} />
          </div>
          <div class="setrow">
            <div class="seti">
              <span class="setl">{$t('settings.coauthor')}</span>
              <span class="setd">{$t('settings.coauthor_desc')}</span>
            </div>
            <Switch checked={coauthorVal} onchange={(v) => saveCoauthor(v)} />
          </div>
          <div class="setrow">
            <div class="seti">
              <span class="setl">{$t('settings.workspace_dir')}</span>
              <span class="setd">{$t('settings.workspace_dir_desc')}</span>
            </div>
            <input
              class="sinput mono"
              type="text"
              placeholder={workspaceDirDefault}
              value={workspaceDir}
              onchange={(e) => saveWorkspaceDir(e.currentTarget.value)}
            />
          </div>

        {:else if cat === 'mobile'}
          <MobileSection />

        {:else if cat === 'experimental'}
          <div class="setrow">
            <div class="seti">
              <span class="setl">{$t('settings.experimental.computer_use')}</span>
              <span class="setd">{$t('settings.experimental.computer_use_desc')}</span>
            </div>
            <Switch checked={computerUse} onchange={(v) => saveComputerUse(v)} />
          </div>
          <div class="setrow">
            <div class="seti">
              <span class="setd">{$t(computerHintKey)}</span>
            </div>
          </div>

        {:else if cat === 'data'}
          {#if dataSubView === 'none'}
            <!-- Memory leads: it's the row users come here to edit, while the
                 archive and the recycle bin are visited only when something
                 needs recovering. -->
            <div class="data-row">
              <div class="data-row-main">
                <iconify-icon icon="lucide:package-open" width="15"></iconify-icon>
                <span class="setl">数据备份</span>
              </div>
              <button class="link-btn" onclick={() => { dataSubView = 'backup'; void loadBackupCatalog() }}>{$t('settings.data.manage')}</button>
            </div>
            <div class="data-row">
              <div class="data-row-main">
                <iconify-icon icon="ant-design:user-outlined" width="15"></iconify-icon>
                <span class="setl">{$t('nav.memory')}</span>
              </div>
              <button class="link-btn" onclick={() => (dataSubView = 'memory')}>{$t('settings.data.manage')}</button>
            </div>
            <div class="data-row">
              <div class="data-row-main">
                <iconify-icon icon="lucide:archive" width="15"></iconify-icon>
                <span class="setl">{$t('settings.data.archived')}</span>
                <span class="data-count">{archivedSessions.length}</span>
              </div>
              <button class="link-btn" onclick={() => (dataSubView = 'archived')}>{$t('settings.data.manage')}</button>
            </div>
            <div class="data-row">
              <div class="data-row-main">
                <iconify-icon icon="ant-design:delete-outlined" width="15"></iconify-icon>
                <span class="setl">{$t('nav.file_recall')}</span>
              </div>
              <button class="link-btn" onclick={() => (dataSubView = 'trash')}>{$t('settings.data.manage')}</button>
            </div>
            <div class="data-row">
              <div class="data-row-main">
                <iconify-icon icon="lucide:database" width="15"></iconify-icon>
                <span class="setl">{$t('settings.profiles.title')}</span>
              </div>
              <button class="link-btn" onclick={() => (dataSubView = 'profiles')}>{$t('settings.data.manage')}</button>
            </div>
          {:else}
            <div class="data-subhead">
              <button class="back-btn" onclick={resetDataView}>
                <iconify-icon icon="ant-design:left-outlined" width="14"></iconify-icon>
              </button>
              <span class="data-subtitle">
                {dataSubView === 'backup' ? '数据备份' : dataSubView === 'archived' ? $t('settings.data.archived') : dataSubView === 'memory' ? $t('nav.memory') : dataSubView === 'profiles' ? $t('settings.profiles.title') : $t('nav.file_recall')}
              </span>
            </div>
            {#if dataSubView === 'backup'}
              <div class="backup-panel">
                <div class="backup-tabs">
                  <button class:on={backupMode === 'export'} onclick={() => { backupMode = 'export'; void loadBackupCatalog() }}>导出数据</button>
                  <button class:on={backupMode === 'import'} onclick={() => { backupMode = 'import'; void loadBackupCatalog() }}>导入数据</button>
                </div>

                {#if backupMode === 'export'}
                  <p class="backup-note">备份包使用你输入的密码加密（AES-256-GCM），密码不会被保存，忘记就无法恢复。</p>
                  <div class="backup-sections">
                    {#each backupCatalog as s (s.id)}
                      <label class="backup-check">
                        <input
                          type="checkbox"
                          checked={exportSel[s.id] ?? false}
                          onchange={(e) => (exportSel[s.id] = e.currentTarget.checked)}
                        />
                        <span><b>{s.label}</b><em>{s.hint}</em></span>
                      </label>
                    {/each}
                  </div>
                  <div class="backup-fields">
                    <input class="sinput" type="password" placeholder="设置密码（至少 6 位）" bind:value={exportPwd} />
                    <input class="sinput" type="password" placeholder="再输入一次密码" bind:value={exportPwd2} />
                  </div>
                  <div class="backup-actions">
                    <button class="link-btn" disabled={exportBusy} onclick={runExport}>
                      {exportBusy ? '正在打包…' : '加密导出'}
                    </button>
                  </div>
                  {#if exportDone}
                    <div class="backup-result">
                      <div>已导出：{exportDone.name}</div>
                      <div class="backup-dim">{exportDone.files} 个文件 · {formatBytes(exportDone.size)}</div>
                      <div class="backup-dim backup-path">{exportDone.path}</div>
                      <button class="link-btn" onclick={() => void openBackupFolder(exportDone?.path)}>打开文件夹</button>
                    </div>
                  {/if}
                {:else}
                  <input
                    bind:this={backupInput}
                    type="file"
                    accept=".atlrbak"
                    style="display:none"
                    onchange={(e) => { acceptPackage(e.currentTarget.files?.[0]); e.currentTarget.value = '' }}
                  />
                  <div
                    class="backup-drop"
                    role="button"
                    tabindex="0"
                    onclick={() => backupInput?.click()}
                    onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') backupInput?.click() }}
                  >
                    <iconify-icon icon="lucide:package-open" width="22"></iconify-icon>
                    <div class="backup-drop-title">{importFile ? importFile.name : '拖入 .atlrbak 备份包，或点击选择'}</div>
                    <div class="backup-dim">{importFile ? formatBytes(importFile.size) : '密码只用于解密，不会保存在任何地方'}</div>
                  </div>
                  {#if importFile}
                    <div class="backup-fields">
                      <input class="sinput" type="password" placeholder="备份密码" bind:value={importPwd} />
                    </div>
                    <div class="backup-actions">
                      <button class="link-btn" disabled={importBusy} onclick={runPreview}>
                        {importBusy ? '正在解析…' : '解析备份包'}
                      </button>
                    </div>
                    {#if importError}<div class="backup-error">{importError}</div>{/if}
                    {#if importSummary}
                      <div class="backup-summary">
                        <div>备份时间：{new Date(importSummary.created_at).toLocaleString()} · {importSummary.app || 'ATLR-Agent'}</div>
                        <div class="backup-dim">{importSummary.files} 个文件 · {formatBytes(importSummary.bytes)}</div>
                        <div class="backup-sections">
                          {#each importSummary.restorable as id (id)}
                            <label class="backup-check">
                              <input
                                type="checkbox"
                                checked={importSel[id] ?? true}
                                onchange={(e) => (importSel[id] = e.currentTarget.checked)}
                              />
                              <span><b>{sectionLabel(id)}</b></span>
                            </label>
                          {/each}
                        </div>
                        <p class="backup-note">导入会直接替换并删除所选数据，不生成备份，无法恢复。完成后应用会自动重启。</p>
                        <div class="backup-actions">
                          <button class="link-btn" disabled={importBusy} onclick={runImport}>
                            {importBusy ? '正在恢复…' : '开始恢复'}
                          </button>
                        </div>
                      </div>
                    {/if}
                    {#if importDone}
                      <div class="backup-result">
                        <div>已恢复：{importDone.sections.map(sectionLabel).join('、')}（{importDone.files} 个文件）</div>
                        {#if restartCountdown > 0}
                          <div class="backup-restart">
                            <span>{restartCountdown} 秒后自动重启…</span>
                            <button class="link-btn" onclick={cancelRestart}>取消</button>
                          </div>
                        {:else}
                          <div class="backup-dim">需要重启应用后才会生效。</div>
                          <button class="link-btn" onclick={() => void restartApp()}>立即重启</button>
                        {/if}
                      </div>
                    {/if}
                  {/if}
                {/if}
              </div>
            {:else if dataSubView === 'archived'}
              {#if archivedSessions.length === 0}
                <div class="data-empty">{$t('settings.data.archived_empty')}</div>
              {:else}
                {@const selCount = Object.keys(archiveSel).length}
                {@const visibleIds = archivedFiltered.map(s => s.id)}
                {@const allVisibleSelected = visibleIds.length > 0 && visibleIds.every(id => archiveSel[id])}
                <div class="archive-toolbar">
                  <div class="archive-search">
                    <iconify-icon icon="ant-design:search-outlined" width="14"></iconify-icon>
                    <input type="text" placeholder={$t('settings.data.search_placeholder')} bind:value={archiveSearch} />
                  </div>
                  <select class="sinput archive-project-select" bind:value={archiveProjectFilter}>
                    <option value="">{$t('settings.data.all_projects')}</option>
                    <option value="none">{$t('settings.data.no_project')}</option>
                    {#each archiveProjectOptions as g (g.id)}
                      <option value={g.id}>{g.name}</option>
                    {/each}
                  </select>
                </div>
                <div class="archive-selectbar">
                  <label class="archive-selectall">
                    <input type="checkbox" checked={allVisibleSelected} disabled={visibleIds.length === 0} onchange={toggleArchiveSelAll} />
                    <span>{$t('settings.data.select_all')}</span>
                  </label>
                  {#if selCount > 0}
                    <span class="archive-selcount">{$t('settings.data.n_selected').replace('{n}', String(selCount))}</span>
                    <button class="btns danger" onclick={deleteSelectedArchived}>{$t('settings.data.delete_selected')}</button>
                  {/if}
                </div>
                {#if archivedFiltered.length === 0}
                  <div class="data-empty">{$t('settings.data.archived_no_match')}</div>
                {:else}
                  {#each archivedGroups as bucket (bucket.group?.id ?? '')}
                    <div class="archive-bucket">
                      <div class="archive-bucket-head">
                        <iconify-icon icon="ant-design:folder-outlined" width="13"></iconify-icon>
                        <span>{bucket.group ? bucket.group.name : $t('settings.data.no_project')}</span>
                        <span class="archive-bucket-count">{bucket.items.length}</span>
                      </div>
                      <div class="archived-list">
                        {#each bucket.items as s (s.id)}
                          <div class="archived-row">
                            <input type="checkbox" checked={!!archiveSel[s.id]} onchange={() => toggleArchiveSel(s.id)} />
                            <div class="archived-info">
                              <span class="archived-name">{nameOf(s)}</span>
                              <span class="archived-meta mono">{s.id} · {ago((s as any).updated_at, $t, $clockTick)}</span>
                            </div>
                            <div class="archived-actions">
                              <button class="btns danger" onclick={() => deleteArchivedSession(s.id)}>{$t('common.delete')}</button>
                              <button class="btns" onclick={() => unarchiveSession(s.id)}>{$t('sidebar.uncollapse')}</button>
                            </div>
                          </div>
                        {/each}
                      </div>
                    </div>
                  {/each}
                {/if}
              {/if}
            {:else if dataSubView === 'memory'}
              <ProfileView embedded />
            {:else if dataSubView === 'profiles'}
              <ProfilesSection />
            {:else}
              <FileRecallView embedded />
            {/if}
          {/if}

        {:else if cat === 'about'}
          <div class="card2">
            <div class="setrow">
              <div class="seti">
                <span class="setl">{$t('common.version')}</span>
                <span class="setd">{$t('settings.about.version_desc')}</span>
              </div>
              <a class="setver mono" href={RELEASES_URL} target="_blank" rel="noopener noreferrer">v{versionStr}</a>
            </div>
            <div class="setrow">
              <div class="seti">
                <span class="setl">{$t('settings.about.firstrun')}</span>
                <span class="setd">{$t('settings.about.firstrun_desc')}</span>
              </div>
              <button class="btns" onclick={rerunFirstRun}>{$t('settings.about.firstrun_btn')}</button>
            </div>
            <div class="setrow">
              <div class="seti">
                <span class="setl">{$t('settings.about.license')}</span>
                <span class="setd">{$t('settings.about.license_desc')}</span>
              </div>
              <button class="link-btn" onclick={() => openUrl(LICENSE_URL)}>{$t('settings.about.license_view')}</button>
            </div>
          </div>
          <a class="about-footer" href={WEBSITE_URL} target="_blank" rel="noopener noreferrer">
            {$t('settings.about.footer').replace('{tagline}', $t('nav.workbench')).replace('{year}', String(new Date().getFullYear()))}
          </a>
        {/if}
      </div>
    </div>
  </div>
</div>
{/if}

<style>
.backdrop {
  position: fixed; inset: 0; z-index: 1000; background: var(--scrim);
  display: flex; align-items: center; justify-content: center; padding: 24px;
}
.modal {
  /* Fixed height (not max-height) so switching between categories with very
     different content lengths (关于 vs 端点) never resizes the modal itself
     — each pane scrolls internally instead. */
  width: 100%; max-width: 760px; height: calc(78vh / var(--font-zoom));
  position: relative;
  background: var(--bg-container); border: 1px solid var(--border); border-radius: var(--radius-card);
  box-shadow: 0 24px 48px rgba(15,23,42,0.18);
  display: flex; flex-direction: column; overflow: hidden;
  animation: octo-fadein 0.16s ease;
}
.modal:focus { outline: none; }

/* ── 数据备份 ─────────────────────────────────────────────────────────── */
.backup-panel { display: flex; flex-direction: column; gap: 12px; padding: 2px 2px 10px; }
.backup-tabs { display: flex; gap: 6px; }
.backup-tabs button {
  border: none; background: transparent; cursor: pointer; font-size: 12.5px;
  padding: 6px 12px; border-radius: 8px; color: var(--text-tertiary);
}
.backup-tabs button:hover { background: var(--hover-neutral); color: var(--text); }
.backup-tabs button.on { background: var(--hover-neutral); color: var(--text); font-weight: 600; }
.backup-note { margin: 0; font-size: 12px; line-height: 1.65; color: var(--text-tertiary); }
.backup-sections { display: flex; flex-direction: column; gap: 9px; }
.backup-check { display: flex; align-items: flex-start; gap: 9px; font-size: 13px; color: var(--text); cursor: pointer; }
.backup-check input { margin: 3px 0 0; accent-color: var(--blue-6); }
.backup-check b { font-weight: 600; }
.backup-check em { display: block; margin-top: 2px; font-style: normal; font-size: 11.5px; color: var(--text-quaternary); }
.backup-fields { display: flex; gap: 8px; flex-wrap: wrap; }
.backup-fields .sinput { flex: 1; min-width: 170px; }
.backup-actions { display: flex; gap: 10px; }
.backup-actions .link-btn[disabled] { opacity: 0.55; pointer-events: none; }
.backup-result, .backup-summary {
  display: flex; flex-direction: column; gap: 8px; padding: 12px;
  border: 1px solid var(--border-secondary); border-radius: 10px; font-size: 12.5px;
}
.backup-dim { font-size: 11.5px; color: var(--text-tertiary); }
.backup-restart { display: flex; align-items: center; gap: 10px; font-size: 12.5px; color: var(--text); }
.backup-path { word-break: break-all; }
.backup-error { font-size: 12px; color: #d4380d; }
.backup-drop {
  display: flex; flex-direction: column; align-items: center; gap: 7px;
  padding: 22px 14px; border: 1px dashed var(--border-secondary); border-radius: 12px;
  color: var(--text-tertiary); cursor: pointer;
  transition: border-color 120ms ease, background 120ms ease;
}
.backup-drop:hover { border-color: var(--blue-6); background: var(--hover-neutral); }
.backup-drop-title { font-size: 13px; color: var(--text); text-align: center; word-break: break-all; }
.backup-drop-overlay {
  position: absolute; inset: 0; z-index: 5; display: flex; flex-direction: column;
  align-items: center; justify-content: center; gap: 10px; border-radius: inherit;
  border: 2px dashed var(--blue-6); color: var(--blue-6); font-size: 14px;
  background: color-mix(in srgb, var(--bg-container) 84%, transparent);
  pointer-events: none;
}
.modal-header {
  display: flex; align-items: center; justify-content: space-between; flex: 0 0 auto;
  padding: 16px 20px; border-bottom: 1px solid var(--border);
}
.modal-title { font-size: 16px; font-weight: 700; letter-spacing: -0.01em; color: var(--text-heading); }
.modal-close {
  width: 28px; height: 28px; border: none; background: transparent; border-radius: 7px;
  display: flex; align-items: center; justify-content: center; cursor: pointer; color: var(--text-secondary);
}
.modal-close:hover { background: var(--hover-neutral); color: var(--text); }
.modal-body { flex: 1; min-height: 0; display: flex; }

/* ── category rail ─────────────────────────────────────────────────────────── */
.rail {
  width: 190px; flex: 0 0 190px; border-right: 1px solid var(--border);
  background: var(--bg-layout); padding: 10px; display: flex; flex-direction: column; gap: 2px;
  overflow-y: auto;
}
.scat {
  display: flex; align-items: center; gap: 9px; padding: 7px 10px;
  border-radius: 7px; cursor: pointer; color: var(--text-tertiary);
}
.scat span { font-size: 13px; color: var(--text-secondary); }
.scat:hover { background: var(--hover-neutral); }
.scat.on { background: var(--active-blue-bg); }
.scat.on span { color: var(--blue-6); font-weight: 600; }
.scat.on { color: var(--blue-6); }

/* ── content pane ──────────────────────────────────────────────────────────── */
.pane { flex: 1; min-width: 0; overflow-y: auto; padding: 20px 24px; display: flex; flex-direction: column; gap: 0; }
.loading-state { padding: 40px; text-align: center; color: var(--text-tertiary); font-size: 14px; }

.setrow {
  display: flex; align-items: center; justify-content: space-between;
  gap: 20px; padding: 14px 2px; border-bottom: 1px solid var(--border-secondary);
}
.setrow:last-child { border-bottom: none; }
/* Rows nested in a card (About) need the card's own inset — the flat rows
   above sit directly in .pane, which already provides that inset itself. */
.card2 .setrow { padding: 16px 20px; }
.seti { display: flex; flex-direction: column; gap: 2px; min-width: 0; }
.setl { font-size: 13px; color: var(--text); }
.setd { font-size: 12px; color: var(--text-secondary); margin-top: 2px; line-height: 1.45; max-width: 42ch; }
.setver { font-size: 13px; color: var(--text-tertiary); flex: 0 0 auto; text-decoration: none; }
.setver:hover { text-decoration: underline; }
.link-btn {
  border: none; background: transparent; color: var(--blue-6); font-size: 13px;
  font-weight: 500; cursor: pointer; font-family: inherit; padding: 0; flex: 0 0 auto;
}
.link-btn:hover { text-decoration: underline; }
.about-footer { display: block; padding: 28px 2px 4px; text-align: center; font-size: 12px; color: var(--text-tertiary); text-decoration: none; }
.about-footer:hover { text-decoration: underline; }
.sinput {
  width: 220px; flex: 0 0 auto; height: 32px; padding: 0 10px;
  border: 1px solid var(--border); border-radius: 8px; font-size: 13px;
  color: var(--text); font-family: inherit; background: var(--bg-container); outline: none;
}
select.sinput { cursor: pointer; }
.sinput:focus { border-color: var(--blue-6); box-shadow: 0 0 0 2px var(--focus-ring); }

/* ── animated wallpaper preview ───────────────────────────────────────────── */
.bg-video-thumb {
  width: 160px; aspect-ratio: 16 / 9; flex: 0 0 auto; border-radius: 8px;
  border: 1px solid var(--border); background: var(--bg-layout); object-fit: cover;
}

/* ── buttons ───────────────────────────────────────────────────────────────── */
.btns {
  height: 30px; padding: 0 12px; border: 1px solid var(--border); background: var(--bg-container);
  border-radius: 8px; font-size: 13px; font-weight: 500; color: var(--text); cursor: pointer;
  font-family: inherit; display: flex; align-items: center; gap: 6px;
}
.btns:hover:not(:disabled) { background: var(--hover-neutral); border-color: var(--text-quaternary); }
.btns:disabled { opacity: 0.5; cursor: not-allowed; }

/* ── about card ────────────────────────────────────────────────────────────── */
.card2 { background: var(--bg-container); border: 1px solid var(--border); border-radius: var(--radius-card); overflow: hidden; }

.mono { font-family: var(--font-mono); }

@media (max-width: 640px) {
  .backdrop { padding: 0; }
  .modal {
    max-width: none; height: 100%; border: none; border-radius: 0;
  }
  .modal-header { padding: 14px 16px; }
  .modal-body { flex-direction: column; }
  .rail {
    width: 100%; flex: 0 0 auto; flex-direction: row; gap: 2px;
    border-right: none; border-bottom: 1px solid var(--border); padding: 6px;
    overflow-x: auto; overflow-y: hidden;
  }
  .scat { flex: 0 0 auto; gap: 6px; padding: 8px 9px; white-space: nowrap; }
  .scat span { font-size: 12px; }
  .pane { padding: 16px 14px; }
  .setrow { gap: 12px; }
  .sinput { width: 150px; }
  .bg-video-thumb { width: 120px; }
  .archive-project-select { width: 120px; }
}

/* ── data management ─────────────────────────────────────────────────────── */
.data-row {
  display: flex; align-items: center; justify-content: space-between;
  gap: 16px; padding: 12px 10px; margin: 0 -10px; border-radius: 8px;
}
.data-row:hover { background: var(--hover-neutral); }
.data-row-main { display: flex; align-items: center; gap: 9px; min-width: 0; color: var(--text-tertiary); }
.data-count { font-size: 12px; color: var(--text-tertiary); }
.data-subhead { display: flex; align-items: center; gap: 8px; padding: 2px 0 14px; }
.back-btn {
  width: 26px; height: 26px; border: none; background: transparent; border-radius: 7px;
  display: flex; align-items: center; justify-content: center; cursor: pointer; color: var(--text-secondary);
}
.back-btn:hover { background: var(--hover-neutral); color: var(--text); }
.data-subtitle { font-size: 14px; font-weight: 600; color: var(--text-heading); }
.data-empty { padding: 32px 4px; text-align: center; font-size: 13px; color: var(--text-tertiary); }

/* ── archived tasks: search/filter + select-all bar + project buckets ────── */
.archive-toolbar { display: flex; gap: 10px; padding-bottom: 12px; }
.archive-search {
  flex: 1; min-width: 0; display: flex; align-items: center; gap: 7px;
  height: 32px; padding: 0 10px; border: 1px solid var(--border); border-radius: 8px;
  background: var(--bg-container); color: var(--text-tertiary);
}
.archive-search input {
  flex: 1; min-width: 0; border: none; outline: none; background: transparent;
  font-size: 13px; color: var(--text); font-family: inherit;
}
.archive-project-select { width: 160px; flex: 0 0 auto; }
.archive-selectbar {
  display: flex; align-items: center; gap: 10px; padding: 4px 2px 10px;
  border-bottom: 1px solid var(--border-secondary); margin-bottom: 4px;
}
.archive-selectall {
  display: flex; align-items: center; gap: 7px; font-size: 12px; color: var(--text-secondary);
  cursor: pointer; user-select: none;
}
.archive-selectall input { cursor: pointer; }
.archive-selcount { font-size: 12px; color: var(--text-tertiary); flex: 1; }
.archive-bucket { padding: 10px 0 4px; }
.archive-bucket-head {
  display: flex; align-items: center; gap: 7px; padding: 0 2px 6px;
  font-size: 12px; font-weight: 600; color: var(--text-tertiary);
}
.archive-bucket-count { color: var(--text-quaternary); font-weight: 400; }
.archived-list { display: flex; flex-direction: column; }
.archived-row {
  display: flex; align-items: center; gap: 12px; padding: 10px 2px;
  border-bottom: 1px solid var(--border-secondary);
}
.archived-row:last-child { border-bottom: none; }
.archived-row > input[type="checkbox"] { flex: 0 0 auto; cursor: pointer; }
.archived-info { flex: 1; display: flex; flex-direction: column; gap: 2px; min-width: 0; }
.archived-name { font-size: 13px; color: var(--text); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.archived-meta { font-size: 11px; color: var(--text-tertiary); }
.archived-actions { display: flex; align-items: center; gap: 8px; flex: 0 0 auto; }
.btns.danger { color: var(--error); }
.btns.danger:hover:not(:disabled) { background: var(--error-bg); border-color: var(--error-border); }
</style>
