package main

import (
	"os"
	"strings"
	"sync/atomic"

	"github.com/open-octo/octo-agent/internal/config"
)

// uiStrings holds every user-facing native string the desktop shell shows
// outside the web UI — dialogs, the tray menu, error messages. Selected once at
// startup by the system language. Format strings keep their verbs so call sites
// can fmt.Sprintf them.
type uiStrings struct {
	trayShow, trayQuit   string
	traySettings         string
	trayNewSession       string
	trayPet, trayPetHide string
	trayStarting         string
	trayBackendFmt       string // "Backend · %s"
	trayClientsFmt       string // "Connected clients: %d"
	trayChannelsFmt      string // "Configured channels: %d"

	takeoverTitle  string
	takeoverMsgFmt string // "...(pid %d)..."
	takeoverOK     string
	takeoverCancel string

	quitTitle  string
	quitMsg    string
	quitOK     string
	quitCancel string

	// Profile switching. The two switch messages differ because a turn in
	// flight and a turn waiting on the user are different reasons to stop.
	trayProfileFmt        string // "Profile · %s"
	profileDefault        string
	profileSwitchTitle    string
	profileSwitchBusyFmt  string
	profileSwitchAskFmt   string
	profileSwitchOK       string
	profileSwitchCancel   string
	profileSaveErrFmt     string
	profileRelaunchErrFmt string

	errTitle     string
	errBindFmt   string // "...%s...%v"
	errStopFmt   string // "...%v"
	errStartFmt  string // "...%v"
	dialogOKText string
}

var enStrings = uiStrings{
	trayShow:        "Show ATLR-Agent",
	trayQuit:        "Quit ATLR-Agent",
	trayNewSession:  "New Session",
	trayPet:         "Show Pet",
	trayPetHide:     "Hide Pet",
	traySettings:    "Settings…",
	trayStarting:    "Starting…",
	trayBackendFmt:  "Backend · %s",
	trayClientsFmt:  "Connected clients: %d",
	trayChannelsFmt: "Configured channels: %d",

	takeoverTitle:  "ATLR-Agent",
	takeoverMsgFmt: "A background ATLR-Agent backend is already running (pid %d).\n\nStop it and run ATLR-Agent as the hub for this machine?",
	takeoverOK:     "Stop and Continue",
	takeoverCancel: "Quit",

	trayProfileFmt:        "Profile · %s",
	profileDefault:        "Default",
	profileSwitchTitle:    "Switch profile",
	profileSwitchBusyFmt:  "ATLR-Agent is working on something right now. Switching to \u201c%s\u201d restarts the app, and that work is lost.\n\nSwitch anyway?",
	profileSwitchAskFmt:   "ATLR-Agent is waiting for an answer from you. Switching to \u201c%s\u201d restarts the app, and the question goes with it.\n\nSwitch anyway?",
	profileSwitchOK:       "Switch",
	profileSwitchCancel:   "Cancel",
	profileSaveErrFmt:     "Couldn't record the profile choice: %v",
	profileRelaunchErrFmt: "Couldn't restart ATLR-Agent: %v",

	quitTitle:  "Quit ATLR-Agent",
	quitMsg:    "Quitting stops the ATLR-Agent backend on this machine. Connected editors, browsers, and IM channels will disconnect.\n\nQuit anyway?",
	quitOK:     "Quit",
	quitCancel: "Cancel",

	errTitle:     "ATLR-Agent",
	errBindFmt:   "Couldn't bind %s — another program may be using it.\n\n%v",
	errStopFmt:   "Couldn't stop the running backend: %v",
	errStartFmt:  "Couldn't start the backend: %v",
	dialogOKText: "OK",
}

var zhStrings = uiStrings{
	trayShow:        "显示 ATLR-Agent 喵～",
	trayQuit:        "退出 ATLR-Agent",
	trayNewSession:  "新建会话喵～",
	trayPet:         "显示桌宠",
	trayPetHide:     "隐藏桌宠",
	traySettings:    "设置…",
	trayStarting:    "启动中…喵",
	trayBackendFmt:  "后端 · %s",
	trayClientsFmt:  "已连接客户端：%d",
	trayChannelsFmt: "已配置 channel：%d",

	takeoverTitle:  "ATLR-Agent",
	takeoverMsgFmt: "已有一个 ATLR-Agent 后端在后台运行了喵（pid %d）。\n\n要停掉它，让人家作为本机的后端中枢吗？",
	takeoverOK:     "停止并继续",
	takeoverCancel: "退出",

	trayProfileFmt:        "Profile · %s",
	profileDefault:        "默认",
	profileSwitchTitle:    "切换 Profile",
	profileSwitchBusyFmt:  "ATLR-Agent 正在处理一个任务喵！(´･ω･`) 切换到「%s」会重启应用，这个任务会丢失。\n\n仍要切换吗？",
	profileSwitchAskFmt:   "ATLR-Agent 正在等主人回答喵。切换到「%s」会重启应用，这个问题也会一并丢掉。\n\n仍要切换吗？",
	profileSwitchOK:       "切换",
	profileSwitchCancel:   "取消",
	profileSaveErrFmt:     "无法保存 Profile 选择：%v",
	profileRelaunchErrFmt: "无法重启 ATLR-Agent：%v",

	quitTitle:  "退出 ATLR-Agent",
	quitMsg:    "退出会停止本机的 ATLR-Agent 后端，已连接的编辑器、浏览器和 IM channel 都会断开喵。(´･ω･`) 仍要退出吗？",
	quitOK:     "退出",
	quitCancel: "取消",

	errTitle:     "ATLR-Agent",
	errBindFmt:   "无法绑定 %s —— 可能有其他程序正在占用喵。\n\n%v",
	errStopFmt:   "无法停止正在运行的后端：%v",
	errStartFmt:  "无法启动后端：%v",
	dialogOKText: "好",
}

// active holds the current string set. It's an atomic pointer because the tray
// refresh loop re-applies the language on its own goroutine while dialog
// callbacks read it on the UI thread.
var active atomic.Pointer[uiStrings]

// L returns the active string set (English until applyLang runs).
func L() *uiStrings {
	if s := active.Load(); s != nil {
		return s
	}
	return &enStrings
}

// applyLang re-resolves the UI language and swaps the active string set. Called
// at startup and on each tray tick, so switching language in onboarding or
// Settings (which writes profile-scoped config.yml) is reflected in the tray/dialogs
// within a few seconds — the desktop shell follows the in-app language choice,
// not the OS.
func applyLang() {
	if resolveLang() == "zh" {
		active.Store(&zhStrings)
	} else {
		active.Store(&enStrings)
	}
}

// resolveLang prefers the user's in-app language (config.yml `language`, set in
// onboarding / Settings). Only before that's chosen does it fall back to the
// OS's preferred UI language.
func resolveLang() string {
	if cfg, err := config.Load(); err == nil {
		switch cfg.Language {
		case "zh":
			return "zh"
		case "en":
			return "en"
		}
	}
	return preferredLang()
}

// preferredLang returns "zh" or "en" from the OS's preferred UI language. It
// asks the platform (osLang: AppleLanguages on macOS, GetUserDefaultLocaleName
// on Windows) first, since a Finder/Explorer-launched app usually has no LANG;
// only when that yields nothing does it fall back to the LC_*/LANG environment
// (which is how Linux exposes it).
func preferredLang() string {
	if lang := osLang(); lang != "" {
		if strings.HasPrefix(strings.ToLower(lang), "zh") {
			return "zh"
		}
		return "en"
	}
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(k); v != "" {
			if strings.HasPrefix(strings.ToLower(v), "zh") {
				return "zh"
			}
			return "en"
		}
	}
	return "en"
}
