// Package music plays Bilibili audio from two public favourites.
//
// The page cannot do this itself: Bilibili's audio CDN rejects requests without
// a `Referer: https://www.bilibili.com/` header, which a browser <audio> element
// is not allowed to set. So the Go host resolves the stream, proxies it, and
// keeps a bounded on-disk cache; the webview only points <audio> at a local URL.
package music

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-octo/octo-agent/internal/datahome"
)

// FavLists are the two public favourites, played in this order, then looped.
var FavLists = []int64{4045382497, 3589159597}

const (
	apiBase     = "https://api.bilibili.com"
	userAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	referer     = "https://www.bilibili.com/"
	playlistTTL = 10 * time.Minute
	// playlistDiskTTL lets a launch reuse yesterday's snapshot instead of
	// paging through ~1,200 favourites again.
	playlistDiskTTL = 6 * time.Hour
	// refreshMinInterval rate-limits the background full re-page. Opening the
	// player repeatedly must not re-walk both favourites each time: Bilibili
	// answers that pattern with 412 and a temporary block.
	refreshMinInterval = 30 * time.Minute
	// nearbyRadius is how many neighbours on either side of the playing track
	// are kept in the personal nearby file and fetched first on a rebuild.
	nearbyRadius = 12
	// Paging is shaped like a client's: a short gap between 20-track pages,
	// then a 3s pause after every 100 tracks.
	pageGap       = 250 * time.Millisecond
	pagesPerPause = 5
	batchPause    = 3 * time.Second
	urlTTL        = 3 * time.Minute
	cacheBudget   = 128 << 20 // 128 MB of audio on disk
	// videoCacheBudget is separate from the audio budget: one 1080p clip can be
	// tens of megabytes, and sharing the budget would evict the songs that play
	// all day whenever the video is used.
	videoCacheBudget = 512 << 20
)

// Track is one playable entry.
type Track struct {
	BVID     string `json:"bvid"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Cover    string `json:"cover"`
	Duration int    `json:"duration"`
}

// favMedia is one entry of a favourite folder, as both the listing endpoint and
// the id-fallback produce it.
type favMedia struct {
	BVID     string `json:"bvid"`
	Title    string `json:"title"`
	Cover    string `json:"cover"`
	Duration int    `json:"duration"`
	Type     int    `json:"type"`
	Upper    struct {
		Name string `json:"name"`
	} `json:"upper"`
}

type favPage struct {
	Code int `json:"code"`
	Data struct {
		Medias  []favMedia `json:"medias"`
		HasMore bool       `json:"has_more"`
	} `json:"data"`
}

type cachedURL struct {
	url string
	at  time.Time
}

// Service owns the playlist cache, the resolved-URL cache and the audio cache.
type Service struct {
	mu      sync.Mutex
	tracks  []Track
	fetched time.Time
	urls    map[string]cachedURL
	// videoURLs memoises resolved DASH video URLs separately from audio.
	videoURLs map[string]cachedURL
	warming   map[string]bool
	client    *http.Client
	cacheDir  string
	// videoCacheDir holds on-demand video downloads (separate budget).
	videoCacheDir string
	// refreshing guards the background re-page so overlapping panel opens do not
	// multiply requests.
	refreshing bool
	// lastRefresh is when the background re-page last started; it enforces
	// refreshMinInterval between full walks of the favourites.
	lastRefresh time.Time
	// Guest cookies (buvid3/buvid4) plus, after a QR login, the account's
	// SESSDATA/bili_jct/DedeUserID. Bilibili answers 412 without any of them.
	cookies map[string]string
}

// NewService builds the player. The audio cache and the playlist snapshot are
// disposable (Cache), while the login cookies belong with the user's data — the
// service resolves each through datahome instead of taking one directory.
func NewService() *Service {
	cacheDir, _ := datahome.RegionPath(datahome.RegionCache, "music-cache")
	videoCacheDir, _ := datahome.RegionPath(datahome.RegionCache, "music-video-cache")
	s := &Service{
		urls:          make(map[string]cachedURL),
		videoURLs:     make(map[string]cachedURL),
		cookies:       make(map[string]string),
		client:        &http.Client{Timeout: 20 * time.Second},
		cacheDir:      cacheDir,
		videoCacheDir: videoCacheDir,
	}
	s.loadCookies()
	return s
}

// Video debug log.
//
// Both the resolve/proxy path here and the frontend (via /api/debug-log) write
// to the same file, so one playback session reads as one chronological trace.
// It is deliberately verbose: this exists to diagnose "the player shows but the
// picture never arrives" without another rebuild.
var (
	debugMu  sync.Mutex
	debugSeq atomic.Int64
)

func videoDebugPath() string {
	p, err := datahome.RegionPath(datahome.RegionCache, "video-debug.log")
	if err != nil {
		return ""
	}
	return p
}

// NextDebugRequestID hands out an id that ties one HTTP request's lines
// together.
func NextDebugRequestID() int64 { return debugSeq.Add(1) }

// LogDebug appends one timestamped JSON line to video-debug.log. side is "be"
// for the Go side and "fe" for events forwarded from the page.
func LogDebug(side, event string, detail any) {
	path := videoDebugPath()
	if path == "" {
		return
	}
	debugMu.Lock()
	defer debugMu.Unlock()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	// Rotate once past 20 MB so a long soak cannot fill the disk.
	if st, err := os.Stat(path); err == nil && st.Size() > 20<<20 {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	payload := ""
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			payload = " " + string(b)
		}
	}
	_, _ = f.WriteString(time.Now().Format("2006-01-02T15:04:05.000-07:00") + " [" + side + "] " + event + payload + "\n")
}

// cookieHeader renders the stored cookies as a Cookie header.
func (s *Service) cookieHeader() string {
	parts := make([]string, 0, len(s.cookies))
	for k, v := range s.cookies {
		if k != "" && v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, "; ")
}

func (s *Service) playlistFile() string {
	p, err := datahome.Path("music-playlist.json")
	if err != nil {
		return ""
	}
	return p
}

// ensureCookie fetches a guest fingerprint once per process.
func (s *Service) ensureCookie(ctx context.Context) {
	s.mu.Lock()
	have := s.cookies["buvid3"] != ""
	s.mu.Unlock()
	if have {
		return
	}
	var spi struct {
		Code int `json:"code"`
		Data struct {
			B3 string `json:"b_3"`
			B4 string `json:"b_4"`
		} `json:"data"`
	}
	if err := s.getJSON(ctx, apiBase+"/x/frontend/finger/spi", &spi); err != nil || spi.Data.B3 == "" {
		return
	}
	s.mu.Lock()
	s.cookies["buvid3"] = spi.Data.B3
	s.cookies["buvid4"] = spi.Data.B4
	s.mu.Unlock()
	s.saveCookies()
}

// request builds one Bilibili API request with the browser-shaped headers the
// WAF expects. Shared by GET reads and the form POSTs that change favourites.
func (s *Service) request(ctx context.Context, method, rawURL string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", referer)
	req.Header.Set("Origin", "https://www.bilibili.com")
	// Browser-shaped headers: the WAF treats a bare Go request as a bot even
	// with the right UA. The app-key pair mirrors what PiliPlus (BiliPlus)
	// sends, which is what keeps favlist readable while logged in.
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("env", "prod")
	req.Header.Set("app-key", "android64")
	req.Header.Set("x-bili-aurora-zone", "sh001")
	req.Header.Set("sec-fetch-site", "same-site")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("sec-fetch-dest", "empty")
	s.mu.Lock()
	cookie := s.cookieHeader()
	s.mu.Unlock()
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	return req, nil
}

func (s *Service) get(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := s.request(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	return s.client.Do(req)
}

func (s *Service) getJSON(ctx context.Context, rawURL string, dst any) error {
	resp, err := s.get(ctx, rawURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bilibili %s: %s", rawURL, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(dst)
}

// Playlist returns both favourites in order, de-duplicated, cached for a while.
func (s *Service) Playlist(ctx context.Context) ([]Track, error) {
	s.mu.Lock()
	if len(s.tracks) > 0 && time.Since(s.fetched) < playlistTTL {
		out := append([]Track(nil), s.tracks...)
		s.mu.Unlock()
		return out, nil
	}
	s.mu.Unlock()

	// A persisted snapshot spares a fresh launch ~1,200 items' worth of paged
	// requests, which is also what gets anonymous clients rate-limited (412).
	if tracks, ok := s.loadPlaylistFile(); ok {
		s.mu.Lock()
		s.tracks, s.fetched = tracks, time.Now()
		s.mu.Unlock()
		return tracks, nil
	}

	// No usable snapshot: start from the personal nearby file (the playing
	// track and its neighbours) so the player comes up immediately. The
	// throttled background refresh fills in the rest without blocking this call.
	if snap, ok := s.loadNearby(); ok && len(snap.Tracks) > 0 {
		s.mu.Lock()
		s.tracks, s.fetched = snap.Tracks, time.Now()
		s.mu.Unlock()
		s.refreshNow()
		return append([]Track(nil), snap.Tracks...), nil
	}

	s.ensureCookie(ctx)
	tracks, complete, err := s.fetchRemote(ctx)
	if err != nil || len(tracks) == 0 {
		// The WAF answered 412 (IP-level rate limit) — an older snapshot is far
		// better than an empty player, so reuse one regardless of age.
		if stale, _, ok := s.loadPlaylistFileAny(); ok {
			s.mu.Lock()
			s.tracks, s.fetched = stale, time.Now()
			s.mu.Unlock()
			return stale, nil
		}
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("music: no playable items in the favourites")
	}

	s.mu.Lock()
	s.tracks = tracks
	// An incomplete run keeps the old timestamp, so the next attempt refetches
	// instead of serving the partial list for the in-memory TTL.
	if complete {
		s.fetched = time.Now()
	}
	s.mu.Unlock()
	// A partial list (one favourite refused) must not poison the snapshot: it
	// would otherwise stick for six hours and hide the rest of the library.
	if complete {
		s.savePlaylistFile(tracks)
	} else if stale, _, ok := s.loadPlaylistFileAny(); ok && len(stale) > len(tracks) {
		s.mu.Lock()
		s.tracks = stale
		s.mu.Unlock()
		return stale, nil
	}
	return append([]Track(nil), tracks...), nil
}

// fetchRemote pages through both favourites. It reports complete=false when a
// page was refused mid-way, so callers can avoid persisting a partial list.
func (s *Service) fetchRemote(ctx context.Context) ([]Track, bool, error) {
	var tracks []Track
	seen := map[string]bool{}
	seenTitle := map[string]bool{}
	complete := true
	addMedia := func(m favMedia) {
		if m.Type != 2 || m.BVID == "" || seen[m.BVID] {
			return
		}
		// The two favourites overlap in practice (same upload re-faved):
		// collapse identical titles as well, normalised for case/space.
		key := strings.ToLower(strings.Join(strings.Fields(m.Title), " "))
		if key != "" && seenTitle[key] {
			return
		}
		seen[m.BVID] = true
		if key != "" {
			seenTitle[key] = true
		}
		tracks = append(tracks, Track{
			BVID:     m.BVID,
			Title:    m.Title,
			Artist:   m.Upper.Name,
			Cover:    m.Cover,
			Duration: m.Duration,
		})
	}

	// One page with the WAF retry already in it. The full parameter set matters:
	// PiliPlus sends keyword/type/tid as well, and the WAF rejects the shortened
	// form it sees as scraped.
	fetchPage := func(fid int64, pn int) (favPage, error) {
		var page favPage
		u := fmt.Sprintf("%s/x/v3/fav/resource/list?media_id=%d&pn=%d&ps=20&keyword=&order=mtime&type=0&tid=0&platform=web", apiBase, fid, pn)
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			err = s.getJSON(ctx, u, &page)
			if err == nil {
				break
			}
			// 412 is the WAF: refresh the guest fingerprint, then back off.
			s.refreshCookie(ctx)
			time.Sleep(time.Duration(2*(attempt+1)*(attempt+1)) * time.Second)
		}
		if err == nil && page.Code != 0 {
			err = fmt.Errorf("favlist %d: code %d", fid, page.Code)
		}
		return page, err
	}

	for fi, fid := range FavLists {
		fetched := map[int]bool{}
		// Current-shelf first: the page that should hold the track the personal
		// nearby file says is playing. Only the first shelf lines up with that
		// index; the second keeps the plain order.
		if fi == 0 {
			if anchor := s.nearbyPage(); anchor > 1 {
				if page, err := fetchPage(fid, anchor); err == nil {
					for _, m := range page.Data.Medias {
						addMedia(m)
					}
					fetched[anchor] = true
					time.Sleep(pageGap)
				}
			}
		}
		pagesSincePause := 0
		for pn := 1; pn <= 100; pn++ {
			if fetched[pn] {
				continue
			}
			page, err := fetchPage(fid, pn)
			if err != nil {
				// The ordered listing endpoint is the one the WAF refuses first
				// (412), while the id list still answers. Rebuild the whole
				// folder from ids + per-video info instead of showing 0 songs.
				if medias, ferr := s.favMediasByIDs(ctx, fid); ferr == nil && len(medias) > 0 {
					for _, m := range medias {
						addMedia(m)
					}
					break
				}
				if len(tracks) > 0 {
					return tracks, false, nil
				}
				return nil, false, err
			}
			for _, m := range page.Data.Medias {
				addMedia(m)
			}
			fetched[pn] = true
			// Client-shaped pacing: a small gap between pages, then a longer
			// pause after each 100-track batch. Dozens of requests back to back
			// are what trips the WAF on anonymous clients.
			pagesSincePause++
			if pagesSincePause >= pagesPerPause {
				pagesSincePause = 0
				time.Sleep(batchPause)
			} else {
				time.Sleep(pageGap)
			}
			if !page.Data.HasMore {
				break
			}
		}
	}
	return tracks, complete, nil
}

// favMediasByIDs is the fallback for a blocked listing endpoint:
// fav/resource/ids returns every aid (it keeps working while the ordered list
// answers 412), then one web-interface/view per video fills in bvid, title,
// cover and duration. Four workers with a 200 ms pace keep us well under the
// rate that triggers the WAF; ~1200 songs take about a minute.
func (s *Service) favMediasByIDs(ctx context.Context, fid int64) ([]favMedia, error) {
	var ids struct {
		Code int `json:"code"`
		Data []struct {
			ID   int64 `json:"id"`
			Type int   `json:"type"`
		} `json:"data"`
	}
	u := fmt.Sprintf("%s/x/v3/fav/resource/ids?media_id=%d", apiBase, fid)
	if err := s.getJSON(ctx, u, &ids); err != nil {
		return nil, err
	}
	if ids.Code != 0 {
		return nil, fmt.Errorf("favlist ids %d: code %d", fid, ids.Code)
	}
	out := make([]favMedia, len(ids.Data))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, item := range ids.Data {
		if item.Type != 2 || item.ID == 0 {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, aid int64) {
			defer wg.Done()
			defer func() { <-sem }()
			select {
			case <-ctx.Done():
				return
			case <-time.After(300 * time.Millisecond):
			}
			var v struct {
				Code int `json:"code"`
				Data struct {
					BVID     string `json:"bvid"`
					Title    string `json:"title"`
					Pic      string `json:"pic"`
					Duration int    `json:"duration"`
					Owner    struct {
						Name string `json:"name"`
					} `json:"owner"`
				} `json:"data"`
			}
			if err := s.getJSON(ctx, fmt.Sprintf("%s/x/web-interface/view?aid=%d", apiBase, aid), &v); err != nil || v.Code != 0 {
				return
			}
			out[i] = favMedia{
				BVID:     v.Data.BVID,
				Title:    v.Data.Title,
				Cover:    v.Data.Pic,
				Duration: v.Data.Duration,
				Type:     2,
				Upper: struct {
					Name string `json:"name"`
				}{Name: v.Data.Owner.Name},
			}
		}(i, item.ID)
	}
	wg.Wait()
	medias := out[:0]
	for _, m := range out {
		if m.BVID != "" {
			medias = append(medias, m)
		}
	}
	return medias, nil
}

// RefreshAsync re-pages the favourites in the background and swaps the result in
// only after it is complete. The player keeps serving the old list meanwhile;
// the next call to Playlist picks up the new one seamlessly.
func (s *Service) RefreshAsync() {
	s.mu.Lock()
	if s.refreshing {
		s.mu.Unlock()
		return
	}
	// A list fetched within the disk TTL needs no re-page, and a recent failed
	// attempt must not be retried immediately: opening the player repeatedly is
	// exactly the traffic pattern Bilibili answers with 412.
	if len(s.tracks) > 0 && time.Since(s.fetched) < playlistDiskTTL {
		s.mu.Unlock()
		return
	}
	if !s.lastRefresh.IsZero() && time.Since(s.lastRefresh) < refreshMinInterval {
		s.mu.Unlock()
		return
	}
	s.refreshing = true
	s.lastRefresh = time.Now()
	s.mu.Unlock()
	s.runRefresh()
}

// refreshNow starts the background re-page regardless of the freshness
// throttle. Playlist uses it when the visible list is only the nearby subset:
// that list is fresh enough to serve but still owes a full library.
func (s *Service) refreshNow() {
	s.mu.Lock()
	if s.refreshing {
		s.mu.Unlock()
		return
	}
	s.refreshing = true
	s.lastRefresh = time.Now()
	s.mu.Unlock()
	s.runRefresh()
}

// runRefresh is the worker behind both entry points above.
func (s *Service) runRefresh() {
	go func() {
		defer func() {
			s.mu.Lock()
			s.refreshing = false
			s.mu.Unlock()
		}()
		// The id fallback fetches per-video info (~1,200 songs at 4 workers),
		// so a cold rebuild needs minutes rather than seconds.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		s.ensureCookie(ctx)
		tracks, complete, err := s.fetchRemote(ctx)
		if err != nil || !complete || len(tracks) == 0 {
			return
		}
		s.mu.Lock()
		s.tracks, s.fetched = tracks, time.Now()
		s.mu.Unlock()
		s.savePlaylistFile(tracks)
		s.cleanupAudioCache(tracks)
		s.evictCovers()
	}()
}

// cleanupAudioCache drops cached audio for tracks that are no longer in the
// list, then enforces the size budget.
func (s *Service) cleanupAudioCache(tracks []Track) {
	if s.cacheDir == "" {
		return
	}
	keep := make(map[string]bool, len(tracks))
	for _, t := range tracks {
		keep[filepath.Base(s.cachePath(t.BVID))] = true
	}
	entries, err := os.ReadDir(s.cacheDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".part") || keep[e.Name()] {
			continue
		}
		_ = os.Remove(filepath.Join(s.cacheDir, e.Name()))
	}
	s.evict()
}

// Invalidate drops the in-memory list and the disk snapshot, so the next
// Playlist call refetches from Bilibili (the panel's "refresh" path).
func (s *Service) Invalidate() {
	s.mu.Lock()
	s.tracks = nil
	s.fetched = time.Time{}
	// An explicit refresh (the panel's retry) must not sit behind the throttle.
	s.lastRefresh = time.Time{}
	s.mu.Unlock()
	_ = os.Remove(s.playlistFile())
}

func (s *Service) refreshCookie(ctx context.Context) {
	s.mu.Lock()
	delete(s.cookies, "buvid3")
	delete(s.cookies, "buvid4")
	s.mu.Unlock()
	s.ensureCookie(ctx)
}

func (s *Service) loadPlaylistFile() ([]Track, bool) {
	tracks, at, ok := s.loadPlaylistFileAny()
	if !ok || time.Since(at) > playlistDiskTTL {
		return nil, false
	}
	return tracks, true
}

// loadPlaylistFileAny ignores the snapshot's age (used when the API refuses).
func (s *Service) loadPlaylistFileAny() ([]Track, time.Time, bool) {
	data, err := os.ReadFile(s.playlistFile())
	if err != nil {
		return nil, time.Time{}, false
	}
	var snap struct {
		At     time.Time `json:"at"`
		Tracks []Track   `json:"tracks"`
	}
	if json.Unmarshal(data, &snap) != nil || len(snap.Tracks) == 0 {
		return nil, time.Time{}, false
	}
	return snap.Tracks, snap.At, true
}

func (s *Service) savePlaylistFile(tracks []Track) {
	path := s.playlistFile()
	if path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	data, err := json.MarshalIndent(struct {
		At     time.Time `json:"at"`
		Tracks []Track   `json:"tracks"`
	}{time.Now(), tracks}, "", " ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}

// nearbySnapshot is the personal record of playback position plus the handful
// of tracks around it. It lives in the User region, not Cache: the playlist
// snapshot is disposable, this is what lets a fresh launch restore the current
// neighbourhood without touching Bilibili.
type nearbySnapshot struct {
	At     time.Time `json:"at"`
	Index  int       `json:"index"`
	BVID   string    `json:"bvid"`
	Tracks []Track   `json:"tracks"`
}

func (s *Service) nearbyPath() string {
	p, err := datahome.RegionPath(datahome.RegionUser, "music-nearby.json")
	if err != nil {
		return ""
	}
	return p
}

// RecordPlayback persists the playing index and its neighbours. Called by the
// player whenever the track changes; a stale index or empty list is ignored.
func (s *Service) RecordPlayback(index int) {
	s.mu.Lock()
	if index < 0 || index >= len(s.tracks) {
		s.mu.Unlock()
		return
	}
	all := s.tracks
	from := index - nearbyRadius
	if from < 0 {
		from = 0
	}
	to := index + nearbyRadius + 1
	if to > len(all) {
		to = len(all)
	}
	snap := nearbySnapshot{
		At:     time.Now(),
		Index:  index,
		BVID:   all[index].BVID,
		Tracks: append([]Track(nil), all[from:to]...),
	}
	s.mu.Unlock()

	path := s.nearbyPath()
	if path == "" {
		return
	}
	data, err := json.MarshalIndent(snap, "", " ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}

// loadNearby reads the personal nearby record. No TTL: its whole point is to
// survive restarts, and a refresh replaces it with fresher neighbours.
func (s *Service) loadNearby() (nearbySnapshot, bool) {
	path := s.nearbyPath()
	if path == "" {
		return nearbySnapshot{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nearbySnapshot{}, false
	}
	var snap nearbySnapshot
	if json.Unmarshal(data, &snap) != nil || len(snap.Tracks) == 0 {
		return nearbySnapshot{}, false
	}
	return snap, true
}

// nearbyPage estimates which 20-track listing page holds the playing track, so
// a rebuild can fetch that neighbourhood before walking the shelf from page 1.
// The estimate can be off (the shelf is ordered by favourite time), which is
// harmless: the page is fetched once and the ordered walk still covers it.
func (s *Service) nearbyPage() int {
	snap, ok := s.loadNearby()
	if !ok || snap.Index <= 0 {
		return 0
	}
	pn := snap.Index/20 + 1
	if pn < 1 {
		pn = 1
	}
	if pn > 100 {
		pn = 100
	}
	return pn
}

// Resolve maps a bvid to a directly fetchable audio URL (DASH, best bandwidth).
func (s *Service) Resolve(ctx context.Context, bvid string) (string, error) {
	s.mu.Lock()
	if c, ok := s.urls[bvid]; ok && time.Since(c.at) < urlTTL {
		s.mu.Unlock()
		return c.url, nil
	}
	s.mu.Unlock()

	var view struct {
		Code int `json:"code"`
		Data struct {
			CID int64 `json:"cid"`
		} `json:"data"`
	}
	if err := s.getJSON(ctx, apiBase+"/x/web-interface/view?bvid="+url.QueryEscape(bvid), &view); err != nil {
		return "", err
	}
	if view.Code != 0 || view.Data.CID == 0 {
		return "", fmt.Errorf("music: cannot resolve %s", bvid)
	}

	var play struct {
		Code int `json:"code"`
		Data struct {
			Dash struct {
				Audio []struct {
					BaseURL   string   `json:"baseUrl"`
					BackupURL []string `json:"backupUrl"`
					Bandwidth int      `json:"bandwidth"`
				} `json:"audio"`
			} `json:"dash"`
		} `json:"data"`
	}
	u := fmt.Sprintf("%s/x/player/playurl?bvid=%s&cid=%d&fnval=4048&qn=127&fourk=1&try_look=1", apiBase, url.QueryEscape(bvid), view.Data.CID)
	if err := s.getJSON(ctx, u, &play); err != nil {
		return "", err
	}
	if play.Code != 0 || len(play.Data.Dash.Audio) == 0 {
		return "", fmt.Errorf("music: no audio stream for %s", bvid)
	}
	best := play.Data.Dash.Audio[0]
	for _, a := range play.Data.Dash.Audio {
		if a.Bandwidth > best.Bandwidth {
			best = a
		}
	}
	picked := best.BaseURL
	if picked == "" && len(best.BackupURL) > 0 {
		picked = best.BackupURL[0]
	}
	if picked == "" {
		return "", fmt.Errorf("music: empty stream URL for %s", bvid)
	}

	s.mu.Lock()
	// A memo, not a record: with a 3-minute TTL nothing old is ever useful, but
	// deleting only on lookup let the map grow with every bvid ever played.
	if len(s.urls) >= 256 {
		for key, c := range s.urls {
			if time.Since(c.at) >= urlTTL {
				delete(s.urls, key)
			}
		}
		for len(s.urls) >= 256 {
			for key := range s.urls {
				delete(s.urls, key)
				break
			}
		}
	}
	s.urls[bvid] = cachedURL{url: picked, at: time.Now()}
	s.mu.Unlock()
	return picked, nil
}

func (s *Service) cachePath(bvid string) string {
	sum := sha1.Sum([]byte(bvid))
	return filepath.Join(s.cacheDir, hex.EncodeToString(sum[:])+".m4s")
}

// dashVideoStream is one DASH video rendition. Codecid 7 = AVC, 12 = HEVC,
// 13 = AV1.
type dashVideoStream struct {
	ID        int      `json:"id"`
	BaseURL   string   `json:"baseUrl"`
	BackupURL []string `json:"backupUrl"`
	Bandwidth int      `json:"bandwidth"`
	Codecid   int      `json:"codecid"`
}

// pickVideoStream prefers AVC, then AV1, then anything except HEVC. WebView2
// has no HEVC soft decoder on most machines, so an HEVC-only pick would show
// audio with a black picture. Within the chosen codec the highest bandwidth
// wins — that is the highest resolution the account is allowed to play.
func pickVideoStream(list []dashVideoStream) dashVideoStream {
	rank := func(codecid int) int {
		switch codecid {
		case 7:
			return 0 // AVC/H.264 — decodable everywhere
		case 13:
			return 1 // AV1
		case 12:
			return 9 // HEVC — no soft decoder in WebView2
		default:
			return 5
		}
	}
	best := list[0]
	bestRank := rank(best.Codecid)
	for _, v := range list[1:] {
		r := rank(v.Codecid)
		if r < bestRank || (r == bestRank && v.Bandwidth > best.Bandwidth) {
			best, bestRank = v, r
		}
	}
	return best
}

// ResolveVideo maps a bvid to the best playable DASH video URL, using the same
// playurl call PiliPlus makes (fnval=4048 = every DASH format, try_look=1 lets
// a logged-out client reach 1080p). URLs are memoised separately from audio.
func (s *Service) ResolveVideo(ctx context.Context, bvid string) (string, error) {
	s.mu.Lock()
	if c, ok := s.videoURLs[bvid]; ok && time.Since(c.at) < urlTTL {
		s.mu.Unlock()
		return c.url, nil
	}
	s.mu.Unlock()

	var view struct {
		Code int `json:"code"`
		Data struct {
			CID int64 `json:"cid"`
		} `json:"data"`
	}
	if err := s.getJSON(ctx, apiBase+"/x/web-interface/view?bvid="+url.QueryEscape(bvid), &view); err != nil {
		return "", err
	}
	if view.Code != 0 || view.Data.CID == 0 {
		return "", fmt.Errorf("music: cannot resolve %s", bvid)
	}

	var play struct {
		Code int `json:"code"`
		Data struct {
			Dash struct {
				Video []dashVideoStream `json:"video"`
			} `json:"dash"`
		} `json:"data"`
	}
	u := fmt.Sprintf("%s/x/player/playurl?bvid=%s&cid=%d&fnval=4048&qn=127&fourk=1&try_look=1", apiBase, url.QueryEscape(bvid), view.Data.CID)
	if err := s.getJSON(ctx, u, &play); err != nil {
		return "", err
	}
	if play.Code != 0 || len(play.Data.Dash.Video) == 0 {
		return "", fmt.Errorf("music: no video stream for %s", bvid)
	}
	best := pickVideoStream(play.Data.Dash.Video)
	picked := best.BaseURL
	if picked == "" && len(best.BackupURL) > 0 {
		picked = best.BackupURL[0]
	}
	if picked == "" {
		return "", fmt.Errorf("music: empty video stream URL for %s", bvid)
	}

	s.mu.Lock()
	if len(s.videoURLs) >= 64 {
		for k, c := range s.videoURLs {
			if time.Since(c.at) >= urlTTL {
				delete(s.videoURLs, k)
			}
		}
		for len(s.videoURLs) >= 64 {
			for k := range s.videoURLs {
				delete(s.videoURLs, k)
				break
			}
		}
	}
	s.videoURLs[bvid] = cachedURL{url: picked, at: time.Now()}
	s.mu.Unlock()
	return picked, nil
}

func (s *Service) videoCachePath(bvid string) string {
	sum := sha1.Sum([]byte(bvid))
	return filepath.Join(s.videoCacheDir, hex.EncodeToString(sum[:])+".m4s")
}

// resolveProgressive resolves Bilibili's plain MP4 (durl). A WebView's <video>
// can play this directly; DASH renditions are fragmented MP4 and need MSE,
// which is why a DASH source only ever showed the poster frame here.
func (s *Service) resolveProgressive(ctx context.Context, bvid string) (string, error) {
	key := "p:" + bvid
	s.mu.Lock()
	if c, ok := s.videoURLs[key]; ok && time.Since(c.at) < urlTTL {
		s.mu.Unlock()
		LogDebug("be", "video.resolve.progressive.cached", map[string]any{"bvid": bvid})
		return c.url, nil
	}
	s.mu.Unlock()
	LogDebug("be", "video.resolve.progressive.start", map[string]any{"bvid": bvid})
	start := time.Now()

	var view struct {
		Code int `json:"code"`
		Data struct {
			CID int64 `json:"cid"`
		} `json:"data"`
	}
	if err := s.getJSON(ctx, apiBase+"/x/web-interface/view?bvid="+url.QueryEscape(bvid), &view); err != nil {
		LogDebug("be", "video.resolve.progressive.view_error", map[string]any{"bvid": bvid, "error": err.Error()})
		return "", err
	}
	if view.Code != 0 || view.Data.CID == 0 {
		LogDebug("be", "video.resolve.progressive.no_cid", map[string]any{"bvid": bvid, "code": view.Code, "cid": view.Data.CID})
		return "", fmt.Errorf("music: cannot resolve %s", bvid)
	}

	var play struct {
		Code int `json:"code"`
		Data struct {
			Quality int `json:"quality"`
			Durl    []struct {
				URL       string   `json:"url"`
				BackupURL []string `json:"backup_url"`
				Size      int64    `json:"size"`
			} `json:"durl"`
		} `json:"data"`
	}
	// qn=80 asks for 1080p; Bilibili downgrades a client that may not play it,
	// and the response's quality field reports what was actually granted.
	u := fmt.Sprintf("%s/x/player/playurl?bvid=%s&cid=%d&fnval=1&qn=80&fourk=1&try_look=1", apiBase, url.QueryEscape(bvid), view.Data.CID)
	if err := s.getJSON(ctx, u, &play); err != nil {
		LogDebug("be", "video.resolve.progressive.playurl_error", map[string]any{"bvid": bvid, "error": err.Error()})
		return "", err
	}
	if play.Code != 0 || len(play.Data.Durl) == 0 {
		LogDebug("be", "video.resolve.progressive.empty", map[string]any{
			"bvid": bvid, "code": play.Code, "quality": play.Data.Quality, "segments": len(play.Data.Durl),
		})
		return "", fmt.Errorf("music: no progressive stream for %s", bvid)
	}
	picked := play.Data.Durl[0].URL
	if picked == "" && len(play.Data.Durl[0].BackupURL) > 0 {
		picked = play.Data.Durl[0].BackupURL[0]
	}
	if picked == "" {
		LogDebug("be", "video.resolve.progressive.no_url", map[string]any{"bvid": bvid, "segments": len(play.Data.Durl)})
		return "", fmt.Errorf("music: empty progressive URL for %s", bvid)
	}
	LogDebug("be", "video.resolve.progressive.ok", map[string]any{
		"bvid": bvid, "cid": view.Data.CID, "quality": play.Data.Quality,
		"segments": len(play.Data.Durl), "size": play.Data.Durl[0].Size,
		"host": urlHost(picked), "ms": time.Since(start).Milliseconds(),
	})
	s.mu.Lock()
	if len(s.videoURLs) >= 128 {
		for k, c := range s.videoURLs {
			if time.Since(c.at) >= urlTTL {
				delete(s.videoURLs, k)
			}
		}
		for len(s.videoURLs) >= 128 {
			for k := range s.videoURLs {
				delete(s.videoURLs, k)
				break
			}
		}
	}
	s.videoURLs[key] = cachedURL{url: picked, at: time.Now()}
	s.mu.Unlock()
	return picked, nil
}

func (s *Service) progressiveCachePath(bvid string) string {
	sum := sha1.Sum([]byte(bvid))
	return filepath.Join(s.videoCacheDir, hex.EncodeToString(sum[:])+".mp4")
}

// ServeVideo proxies one video. Progressive MP4 is tried first so the WebView
// can play it as a plain <video src>; DASH stays as a fallback for videos that
// have no progressive rendition.
func (s *Service) ServeVideo(w http.ResponseWriter, r *http.Request, bvid string) {
	rid := NextDebugRequestID()
	LogDebug("be", "video.request", map[string]any{
		"rid":    rid,
		"bvid":   bvid,
		"method": r.Method,
		"range":  r.Header.Get("Range"),
		"accept": r.Header.Get("Accept"),
		"ua":     r.Header.Get("User-Agent"),
	})
	if !ValidBVID(bvid) {
		LogDebug("be", "video.reject", map[string]any{"rid": rid, "bvid": bvid, "reason": "invalid bvid"})
		http.Error(w, "unknown track", http.StatusNotFound)
		return
	}
	if videoURL, err := s.resolveProgressive(r.Context(), bvid); err == nil {
		LogDebug("be", "video.source", map[string]any{
			"rid": rid, "mode": "progressive", "host": urlHost(videoURL),
		})
		s.serveVideoStream(w, r, bvid, videoURL, s.progressiveCachePath(bvid), rid)
		return
	} else {
		LogDebug("be", "video.progressive.failed", map[string]any{
			"rid": rid, "bvid": bvid, "error": err.Error(),
		})
	}
	videoURL, err := s.ResolveVideo(r.Context(), bvid)
	if err != nil {
		LogDebug("be", "video.dash.failed", map[string]any{"rid": rid, "bvid": bvid, "error": err.Error()})
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	LogDebug("be", "video.source", map[string]any{"rid": rid, "mode": "dash", "host": urlHost(videoURL)})
	s.serveVideoStream(w, r, bvid, videoURL, s.videoCachePath(bvid), rid)
}

// urlHost is the log-safe part of a signed CDN URL (the query is a token).
func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "unparsed"
	}
	return u.Host + u.Path
}

// serveVideoStream proxies and caches one resolved stream URL. Nothing is
// fetched until this endpoint is hit; a full playback is written through to the
// video cache, and a Range playback warms it in the background.
func (s *Service) serveVideoStream(w http.ResponseWriter, r *http.Request, bvid, videoURL, path string, rid int64) {
	if f, err := os.Open(path); err == nil {
		if st, err := f.Stat(); err == nil {
			LogDebug("be", "video.cache.hit", map[string]any{"rid": rid, "bvid": bvid, "size": st.Size(), "range": r.Header.Get("Range")})
			defer f.Close()
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Accept-Ranges", "bytes")
			http.ServeContent(w, r, "", st.ModTime(), f)
			return
		}
		f.Close()
	}
	LogDebug("be", "video.cache.miss", map[string]any{"rid": rid, "bvid": bvid, "range": r.Header.Get("Range")})

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, videoURL, nil)
	if err != nil {
		LogDebug("be", "video.upstream.request_error", map[string]any{"rid": rid, "error": err.Error()})
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", referer)
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}
	upStart := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		LogDebug("be", "video.upstream.error", map[string]any{"rid": rid, "error": err.Error(), "ms": time.Since(upStart).Milliseconds()})
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	LogDebug("be", "video.upstream.response", map[string]any{
		"rid":           rid,
		"status":        resp.StatusCode,
		"contentLength": resp.ContentLength,
		"contentRange":  resp.Header.Get("Content-Range"),
		"contentType":   resp.Header.Get("Content-Type"),
		"ms":            time.Since(upStart).Milliseconds(),
	})

	for _, h := range []string{"Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.WriteHeader(resp.StatusCode)

	rangeReq := r.Header.Get("Range") != ""
	if rangeReq || resp.StatusCode != http.StatusOK {
		copyStart := time.Now()
		n, copyErr := io.Copy(w, resp.Body)
		LogDebug("be", "video.copy.range", map[string]any{
			"rid": rid, "bytes": n, "status": resp.StatusCode, "error": errString(copyErr),
			"ms": time.Since(copyStart).Milliseconds(),
		})
		if rangeReq {
			LogDebug("be", "video.warm.trigger", map[string]any{"rid": rid, "bvid": bvid})
			s.warmVideo(videoURL, path)
		}
		return
	}
	if err := os.MkdirAll(s.videoCacheDir, 0o755); err != nil {
		LogDebug("be", "video.cache.mkdir_error", map[string]any{"rid": rid, "error": err.Error()})
		_, _ = io.Copy(w, resp.Body)
		return
	}
	tmp, err := os.CreateTemp(s.videoCacheDir, "dl-*.part")
	if err != nil {
		LogDebug("be", "video.cache.temp_error", map[string]any{"rid": rid, "error": err.Error()})
		_, _ = io.Copy(w, resp.Body)
		return
	}
	tmpName := tmp.Name()
	copyStart := time.Now()
	n, copyErr := io.Copy(io.MultiWriter(w, tmp), resp.Body)
	closeErr := tmp.Close()
	ok := copyErr == nil && closeErr == nil
	if ok {
		_ = os.Rename(tmpName, path)
		s.evictVideo()
	} else {
		_ = os.Remove(tmpName)
	}
	LogDebug("be", "video.copy.full", map[string]any{
		"rid": rid, "bytes": n, "cached": ok,
		"copyError": errString(copyErr), "closeError": errString(closeErr),
		"ms": time.Since(copyStart).Milliseconds(),
	})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// warmVideo downloads a clip in the background after a Range response so the
// next play comes from disk. Keyed by cache path, which separates the
// progressive and DASH variants of the same bvid.
func (s *Service) warmVideo(videoURL, path string) {
	key := "v:" + path
	warmStart := time.Now()
	LogDebug("be", "video.warm.start", map[string]any{"file": filepath.Base(path), "host": urlHost(videoURL)})
	s.mu.Lock()
	if s.warming == nil {
		s.warming = make(map[string]bool)
	}
	if s.warming[key] {
		s.mu.Unlock()
		LogDebug("be", "video.warm.skip", map[string]any{"file": filepath.Base(path), "reason": "already warming"})
		return
	}
	s.warming[key] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.warming, key)
		s.mu.Unlock()
	}()

	if _, err := os.Stat(path); err == nil {
		LogDebug("be", "video.warm.skip", map[string]any{"file": filepath.Base(path), "reason": "already cached"})
		return
	}
	req, err := http.NewRequest(http.MethodGet, videoURL, nil)
	if err != nil {
		LogDebug("be", "video.warm.fail", map[string]any{"file": filepath.Base(path), "error": err.Error()})
		return
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", referer)
	resp, err := s.client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		msg := "status " + http.StatusText(resp.StatusCode)
		if err != nil {
			msg = err.Error()
		}
		if resp != nil {
			resp.Body.Close()
		}
		LogDebug("be", "video.warm.fail", map[string]any{"file": filepath.Base(path), "error": msg})
		return
	}
	defer resp.Body.Close()
	if err := os.MkdirAll(s.videoCacheDir, 0o755); err != nil {
		LogDebug("be", "video.warm.fail", map[string]any{"file": filepath.Base(path), "error": err.Error()})
		return
	}
	tmp, err := os.CreateTemp(s.videoCacheDir, "dl-*.part")
	if err != nil {
		LogDebug("be", "video.warm.fail", map[string]any{"file": filepath.Base(path), "error": err.Error()})
		return
	}
	name := tmp.Name()
	n, copyErr := io.Copy(tmp, resp.Body)
	closeErr := tmp.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(name)
		LogDebug("be", "video.warm.fail", map[string]any{
			"file": filepath.Base(path), "bytes": n,
			"copyError": errString(copyErr), "closeError": errString(closeErr),
		})
		return
	}
	if err := os.Rename(name, path); err == nil {
		s.evictVideo()
	}
	LogDebug("be", "video.warm.done", map[string]any{
		"file": filepath.Base(path), "bytes": n, "ms": time.Since(warmStart).Milliseconds(),
	})
}

// ServeStream proxies one track. Full requests are written through to the disk
// cache; a cached track is served straight from disk (with Range support).
func (s *Service) ServeStream(w http.ResponseWriter, r *http.Request, bvid string) {
	// Any well-formed bvid may play (search results come from outside the two
	// favourites); the shape check is what keeps this from being an open proxy.
	// The favourites list is deliberately not consulted here: a playlist refresh
	// failing must never block a track the user picked from search.
	if !ValidBVID(bvid) {
		http.Error(w, "unknown track", http.StatusNotFound)
		return
	}

	path := s.cachePath(bvid)
	if f, err := os.Open(path); err == nil {
		defer f.Close()
		if st, err := f.Stat(); err == nil {
			w.Header().Set("Content-Type", "audio/mp4")
			w.Header().Set("Accept-Ranges", "bytes")
			http.ServeContent(w, r, "", st.ModTime(), f)
			return
		}
	}

	audioURL, err := s.Resolve(r.Context(), bvid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, audioURL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", referer)
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for _, h := range []string{"Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	// The CDN answers with application/octet-stream; <audio> needs a real audio
	// type or it refuses the source.
	w.Header().Set("Content-Type", "audio/mp4")
	w.WriteHeader(resp.StatusCode)

	// Only a full, successful body is cacheable; a Range slice is passed
	// through and a background fetch fills the cache for the next play.
	rangeReq := r.Header.Get("Range") != ""
	if rangeReq || resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(w, resp.Body)
		if rangeReq {
			s.warm(bvid)
		}
		return
	}
	if err := os.MkdirAll(s.cacheDir, 0o755); err != nil {
		_, _ = io.Copy(w, resp.Body)
		return
	}
	tmp, err := os.CreateTemp(s.cacheDir, "dl-*.part")
	if err != nil {
		_, _ = io.Copy(w, resp.Body)
		return
	}
	tmpName := tmp.Name()
	_, copyErr := io.Copy(io.MultiWriter(w, tmp), resp.Body)
	closeErr := tmp.Close()
	if copyErr == nil && closeErr == nil {
		_ = os.Rename(tmpName, path)
		s.evict()
	} else {
		_ = os.Remove(tmpName)
	}
}

// warm downloads a track into the cache in the background, so the next play of
// the same track comes from disk. Used after serving a Range request, which the
// browser makes before it has the whole file.
func (s *Service) warm(bvid string) {
	s.mu.Lock()
	if s.warming == nil {
		s.warming = make(map[string]bool)
	}
	if s.warming[bvid] {
		s.mu.Unlock()
		return
	}
	s.warming[bvid] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.warming, bvid)
		s.mu.Unlock()
	}()

	path := s.cachePath(bvid)
	if _, err := os.Stat(path); err == nil {
		return // already there
	}
	audioURL, err := s.Resolve(context.Background(), bvid)
	if err != nil {
		return
	}
	req, err := http.NewRequest(http.MethodGet, audioURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", referer)
	resp, err := s.client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return
	}
	defer resp.Body.Close()
	if err := os.MkdirAll(s.cacheDir, 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(s.cacheDir, "dl-*.part")
	if err != nil {
		return
	}
	name := tmp.Name()
	_, copyErr := io.Copy(tmp, resp.Body)
	closeErr := tmp.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(name)
		return
	}
	if err := os.Rename(name, path); err == nil {
		s.evict()
	}
}

// evict keeps the audio cache under cacheBudget, oldest first.
func (s *Service) evict() {
	evictDir(s.cacheDir, cacheBudget)
}

// evictVideo keeps the on-demand video cache under its own budget.
func (s *Service) evictVideo() {
	evictDir(s.videoCacheDir, videoCacheBudget)
}

// evictDir removes the oldest files in dir until the total fits the budget.
func evictDir(dir string, budget int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type fileInfo struct {
		name string
		size int64
		at   time.Time
	}
	var files []fileInfo
	var total int64
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".part") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{e.Name(), info.Size(), info.ModTime()})
		total += info.Size()
	}
	if total <= budget {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].at.Before(files[j].at) })
	for _, f := range files {
		if total <= budget {
			break
		}
		if err := os.Remove(filepath.Join(dir, f.name)); err == nil {
			total -= f.size
		}
	}
}

// CacheStats reports the cache size and file count (settings/UI can surface it).
func (s *Service) CacheStats() (bytes int64, files int) {
	entries, err := os.ReadDir(s.cacheDir)
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if info, err := e.Info(); err == nil {
			bytes += info.Size()
			files++
		}
	}
	return bytes, files
}
