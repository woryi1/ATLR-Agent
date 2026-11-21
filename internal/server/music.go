package server

// Music endpoints: the playlist (two public Bilibili favourites), the audio
// stream proxy, and the resume state. Playback itself lives in the webview; the
// Go side exists because the CDN requires a Referer header the page cannot set.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/open-octo/octo-agent/internal/config"
	"github.com/open-octo/octo-agent/internal/datahome"
	"github.com/open-octo/octo-agent/internal/music"
)

var musicService = music.NewService()

// musicState is what the player restores on launch.
type musicState struct {
	Index    int     `json:"index"`
	Paused   bool    `json:"paused"`
	Position float64 `json:"position"`
}

func musicStatePath() string {
	p, err := datahome.Path("music-state.json")
	if err != nil {
		return ""
	}
	return p
}

func (s *Server) handleMusicPlaylist(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("refresh") == "1" {
		musicService.Invalidate()
	} else {
		// Background refresh is throttled inside the service: a fresh list is
		// served as-is, and a rebuild runs at most once per refreshMinInterval,
		// so opening the player repeatedly cannot walk both favourites every
		// time (Bilibili answers that with 412).
		musicService.RefreshAsync()
	}
	tracks, err := musicService.Playlist(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	// An empty list with loading=true means the background walk is still
	// running; the page polls and fills in instead of blocking on this call.
	if tracks == nil {
		tracks = []music.Track{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tracks": tracks, "loading": musicService.Loading()})
}

// POST /api/music/nearby — record the playing index. The service keeps that
// track and its neighbours in Data/User/music-nearby.json, which is what the
// next launch starts from instead of re-paging ~1,200 favourites.
func (s *Server) handleMusicNearby(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Index int `json:"index"`
	}
	if err := readBodyJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	musicService.RecordPlayback(body.Index)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMusicStream(w http.ResponseWriter, r *http.Request) {
	bvid := r.PathValue("bvid")
	if bvid == "" {
		writeError(w, http.StatusBadRequest, "missing track id")
		return
	}
	musicService.ServeStream(w, r, bvid)
}

// GET /api/music/video/{bvid} — proxy the DASH video stream (best decodable
// rendition). Nothing is fetched until the page asks for it.
func (s *Server) handleMusicVideo(w http.ResponseWriter, r *http.Request) {
	bvid := r.PathValue("bvid")
	if bvid == "" {
		writeError(w, http.StatusBadRequest, "missing track id")
		return
	}
	musicService.ServeVideo(w, r, bvid)
}

func (s *Server) handleBiliQR(w http.ResponseWriter, r *http.Request) {
	loginURL, key, err := musicService.QRGenerate(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": loginURL, "key": key})
}

func (s *Server) handleBiliQRPoll(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "missing key")
		return
	}
	info, status, err := musicService.QRPoll(r.Context(), key)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": status, "logged_in": info.LoggedIn, "name": info.Name, "face": info.Face,
	})
}

func (s *Server) handleBiliMe(w http.ResponseWriter, r *http.Request) {
	info := musicService.Me(r.Context())
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleBiliLogout(w http.ResponseWriter, r *http.Request) {
	musicService.Logout()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleBiliQRImage(w http.ResponseWriter, r *http.Request) {
	text := r.URL.Query().Get("text")
	if text == "" {
		writeError(w, http.StatusBadRequest, "missing text")
		return
	}
	png, err := music.QRPNG(text, 520)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to render QR")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

func (s *Server) handleMusicSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	tracks, err := musicService.SearchRemote(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if tracks == nil {
		tracks = []music.Track{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tracks": tracks})
}

func (s *Server) handleMusicFolders(w http.ResponseWriter, r *http.Request) {
	folders, err := musicService.Folders(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"folders": folders})
}

// handleMusicFavorite saves a search result into the account's favourites,
// picking a music-named folder first and falling back to the default shelf.
func (s *Server) handleMusicFavorite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BVID string `json:"bvid"`
	}
	if err := readBodyJSON(r, &body); err != nil || body.BVID == "" {
		writeError(w, http.StatusBadRequest, "missing bvid")
		return
	}
	folder, err := musicService.Favorite(r.Context(), body.BVID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "folder": folder})
}

// Lyrics are static once published, so the answer is cached on disk and a
// restart does not pay for the WBI-signed lookup again. A "no lyrics" answer is
// cached briefly: a track can gain subtitles, and a permanent negative entry
// would hide them forever.
const (
	lyricDiskTTL         = 30 * 24 * time.Hour
	lyricDiskNegativeTTL = time.Hour
)

type lyricCacheFile struct {
	Lines []music.LyricLine `json:"lines"`
	At    int64             `json:"at"`
}

func lyricCachePath(bvid string) string {
	p, err := datahome.SharedPath(datahome.RegionCache, "music", "lyrics", bvid+".json")
	if err != nil {
		return ""
	}
	return p
}

func readLyricDiskCache(bvid string) ([]music.LyricLine, bool) {
	path := lyricCachePath(bvid)
	if path == "" {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var f lyricCacheFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, false
	}
	ttl := lyricDiskTTL
	if len(f.Lines) == 0 {
		ttl = lyricDiskNegativeTTL
	}
	if time.Since(time.Unix(f.At, 0)) > ttl {
		return nil, false
	}
	return f.Lines, true
}

func writeLyricDiskCache(bvid string, lines []music.LyricLine) {
	path := lyricCachePath(bvid)
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	if lines == nil {
		lines = []music.LyricLine{}
	}
	data, err := json.Marshal(lyricCacheFile{Lines: lines, At: time.Now().Unix()})
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}

func (s *Server) handleMusicLyrics(w http.ResponseWriter, r *http.Request) {
	bvid := r.URL.Query().Get("bvid")
	if bvid == "" {
		writeError(w, http.StatusBadRequest, "missing bvid")
		return
	}
	if lines, ok := readLyricDiskCache(bvid); ok {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"bvid": bvid, "lines": lines, "cached": true})
		return
	}
	lines, err := musicService.Lyrics(r.Context(), bvid)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if lines == nil {
		lines = []music.LyricLine{}
	}
	writeLyricDiskCache(bvid, lines)
	// Never let the webview serve a stale answer for the same bvid: the host
	// already memoises, and a cached "no lyrics" must not outlive a fix.
	w.Header().Set("Cache-Control", "no-store")
	// Echo the id so the page can drop an answer that raced a track change.
	writeJSON(w, http.StatusOK, map[string]any{"bvid": bvid, "lines": lines})
}

func (s *Server) handleBiliImage(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.Query().Get("url")
	if rawURL == "" {
		writeError(w, http.StatusBadRequest, "missing url")
		return
	}
	data, ctype, err := musicService.FetchImage(r.Context(), rawURL)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(data)
}

func (s *Server) handleMusicOpen(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BVID string `json:"bvid"`
	}
	if err := readBodyJSON(r, &body); err != nil || body.BVID == "" {
		writeError(w, http.StatusBadRequest, "missing bvid")
		return
	}
	cfg, err := config.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load config")
		return
	}
	pageURL := "https://www.bilibili.com/video/" + body.BVID
	// Unset means the in-app window: opening the system browser is opt-in.
	useSystem := cfg.MusicOpenInSystem != nil && *cfg.MusicOpenInSystem
	if !useSystem {
		writeJSON(w, http.StatusOK, map[string]any{"url": pageURL})
		return
	}
	if err := openInSystemBrowser(pageURL); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"opened": true})
}

// openInSystemBrowser hands the URL to the OS default handler.
func openInSystemBrowser(pageURL string) error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("music: system browser open is Windows-only")
	}
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", pageURL).Start()
}

func (s *Server) handleGetMusicOpenMode(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"system": cfg.MusicOpenInSystem != nil && *cfg.MusicOpenInSystem,
	})
}

func (s *Server) handlePutMusicOpenMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		System bool `json:"system"`
	}
	if err := readBodyJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	cfg, err := config.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load config")
		return
	}
	cfg.MusicOpenInSystem = &body.System
	if err := cfg.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"system": body.System})
}

func (s *Server) handleGetMusicState(w http.ResponseWriter, r *http.Request) {
	path := musicStatePath()
	if path == "" {
		writeJSON(w, http.StatusOK, musicState{})
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, http.StatusOK, musicState{})
		return
	}
	var st musicState
	if err := json.Unmarshal(data, &st); err != nil {
		writeJSON(w, http.StatusOK, musicState{})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handlePutMusicState(w http.ResponseWriter, r *http.Request) {
	var st musicState
	if err := readBodyJSON(r, &st); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	path := musicStatePath()
	if path == "" {
		writeError(w, http.StatusInternalServerError, "no data directory")
		return
	}
	data, err := json.Marshal(st)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encode state")
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save state")
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save state")
		return
	}
	writeJSON(w, http.StatusOK, st)
}
