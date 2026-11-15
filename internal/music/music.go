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
	urlTTL          = 3 * time.Minute
	cacheBudget     = 128 << 20 // 128 MB of audio on disk
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
	mu       sync.Mutex
	tracks   []Track
	fetched  time.Time
	urls     map[string]cachedURL
	warming  map[string]bool
	client   *http.Client
	cacheDir string
	// refreshing guards the background re-page so overlapping panel opens do not
	// multiply requests.
	refreshing bool
	// Guest cookies (buvid3/buvid4) plus, after a QR login, the account's
	// SESSDATA/bili_jct/DedeUserID. Bilibili answers 412 without any of them.
	cookies map[string]string
}

// NewService builds the player. The audio cache and the playlist snapshot are
// disposable (Cache), while the login cookies belong with the user's data — the
// service resolves each through datahome instead of taking one directory.
func NewService() *Service {
	cacheDir, _ := datahome.RegionPath(datahome.RegionCache, "music-cache")
	s := &Service{
		urls:     make(map[string]cachedURL),
		cookies:  make(map[string]string),
		client:   &http.Client{Timeout: 20 * time.Second},
		cacheDir: cacheDir,
	}
	s.loadCookies()
	return s
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
	for _, fid := range FavLists {
		for pn := 1; pn <= 100; pn++ {
			var page favPage
			// The full parameter set matters: PiliPlus sends keyword/type/tid as
			// well, and the WAF rejects the shortened form it sees as scraped.
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
			if !page.Data.HasMore {
				break
			}
			// Pace the paging: dozens of requests back to back trip the WAF.
			time.Sleep(350 * time.Millisecond)
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
	sem := make(chan struct{}, 4)
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
			case <-time.After(200 * time.Millisecond):
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
	s.refreshing = true
	s.mu.Unlock()
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
	u := fmt.Sprintf("%s/x/player/playurl?bvid=%s&cid=%d&fnval=16&qn=64&fourk=0", apiBase, url.QueryEscape(bvid), view.Data.CID)
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

// evict keeps the cache under cacheBudget, oldest first.
func (s *Service) evict() {
	entries, err := os.ReadDir(s.cacheDir)
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
	if total <= cacheBudget {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].at.Before(files[j].at) })
	for _, f := range files {
		if total <= cacheBudget {
			break
		}
		if err := os.Remove(filepath.Join(s.cacheDir, f.name)); err == nil {
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
