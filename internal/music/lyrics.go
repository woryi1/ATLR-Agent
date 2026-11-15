package music

// Lyrics for a track.
//
// Bilibili exposes per-video subtitles through the player endpoint; some music
// uploads ship a dedicated lyric file there and some only have auto captions.
// Either way the shape is the same JSON body of timed lines. No subtitles means
// no lines, and the player hides the lyric control.

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
	"strconv"
	"strings"
	"sync"
	"time"
)

// LyricLine is one timed line (seconds).
type LyricLine struct {
	From float64 `json:"from"`
	To   float64 `json:"to"`
	Text string  `json:"text"`
}

type subtitleEntry struct {
	Lan    string `json:"lan"`
	LanDoc string `json:"lan_doc"`
	SubURL string `json:"subtitle_url"`
	IsLock bool   `json:"is_lock"`
	AIType int    `json:"ai_type"`
}

// Memory memo for the player answer. Without it the same video could answer
// with lines once and with nothing a minute later (the endpoint is rate
// limited and occasionally flaky), which is exactly the "switch away and back
// and the lyrics return" report. Lines are immutable once parsed, so sharing
// the slice is safe.
type lyricEntry struct {
	lines []LyricLine
	at    time.Time
}

var (
	lyricMu    sync.Mutex
	lyricCache = map[string]lyricEntry{}
)

const (
	lyricPositiveTTL = 30 * time.Minute
	lyricNegativeTTL = 60 * time.Second
	// A lyric answer can be hundreds of kilobytes; an unbounded map kept growing
	// for as long as the user kept picking new tracks. Capped well above the
	// playlist size, so the two favourites folders always stay warm.
	lyricCacheMax = 64
)

// pruneLyricsLocked drops expired entries first, then the oldest ones, so the
// memo cannot outgrow the cap no matter how many distinct tracks are played.
func pruneLyricsLocked(now time.Time) {
	if len(lyricCache) < lyricCacheMax {
		return
	}
	for bvid, e := range lyricCache {
		ttl := lyricPositiveTTL
		if len(e.lines) == 0 {
			ttl = lyricNegativeTTL
		}
		if now.Sub(e.at) > ttl {
			delete(lyricCache, bvid)
		}
	}
	for len(lyricCache) >= lyricCacheMax {
		oldest, oldestAt := "", now
		first := true
		for bvid, e := range lyricCache {
			if first || e.at.Before(oldestAt) {
				oldest, oldestAt, first = bvid, e.at, false
			}
		}
		if oldest == "" {
			return
		}
		delete(lyricCache, oldest)
	}
}

// Lyrics returns the timed lines for a video, preferring a Chinese track.
func (s *Service) Lyrics(ctx context.Context, bvid string) ([]LyricLine, error) {
	if lines, ok := lyricLookup(bvid); ok {
		return lines, nil
	}

	var view struct {
		Code int `json:"code"`
		Data struct {
			CID int64 `json:"cid"`
		} `json:"data"`
	}
	if err := s.getJSON(ctx, apiBase+"/x/web-interface/view?bvid="+url.QueryEscape(bvid), &view); err != nil {
		return nil, err
	}
	if view.Code != 0 || view.Data.CID == 0 {
		return nil, fmt.Errorf("music: cannot resolve %s", bvid)
	}

	lines, err := s.fetchLyrics(ctx, bvid, view.Data.CID)
	if err != nil {
		// Do not memoise hard failures: the next open retries.
		return nil, err
	}
	lyricMu.Lock()
	pruneLyricsLocked(time.Now())
	lyricCache[bvid] = lyricEntry{lines: lines, at: time.Now()}
	lyricMu.Unlock()
	return lines, nil
}

func lyricLookup(bvid string) ([]LyricLine, bool) {
	lyricMu.Lock()
	defer lyricMu.Unlock()
	e, ok := lyricCache[bvid]
	if !ok {
		return nil, false
	}
	ttl := lyricPositiveTTL
	if len(e.lines) == 0 {
		ttl = lyricNegativeTTL
	}
	if time.Since(e.at) > ttl {
		delete(lyricCache, bvid)
		return nil, false
	}
	return e.lines, true
}

// fetchLyrics asks the WBI-signed player endpoint. The signature is mandatory:
// the unsigned call answers anonymously (no subtitles at all) or, on a stale
// key, with a generic payload whose subtitles belong to another video. The cid
// echoed by the answer must therefore equal the requested one; anything else is
// discarded rather than shown as the current song's words.
func (s *Service) fetchLyrics(ctx context.Context, bvid string, cid int64) ([]LyricLine, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			s.resetWBI()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(250*attempt) * time.Millisecond):
			}
		}
		query, err := s.signWBI(ctx, map[string]string{"bvid": bvid, "cid": strconv.FormatInt(cid, 10)})
		if err != nil {
			lastErr = err
			continue
		}
		var player struct {
			Code int `json:"code"`
			Data struct {
				Cid      int64 `json:"cid"`
				Subtitle struct {
					Subtitles []subtitleEntry `json:"subtitles"`
				} `json:"subtitle"`
			} `json:"data"`
		}
		if err := s.getJSON(ctx, apiBase+"/x/player/wbi/v2?"+query, &player); err != nil {
			lastErr = err
			continue
		}
		if player.Code != 0 {
			lastErr = fmt.Errorf("music: player code %d", player.Code)
			continue
		}
		if player.Data.Cid != cid {
			lastErr = fmt.Errorf("music: player cid mismatch for %s", bvid)
			continue
		}
		if len(player.Data.Subtitle.Subtitles) == 0 {
			return nil, nil // no subtitles: the UI hides the control
		}
		return s.downloadSubtitle(ctx, player.Data.Subtitle.Subtitles)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("music: no lyrics for %s", bvid)
	}
	return nil, lastErr
}

// downloadSubtitle picks a usable track (Chinese, not locked, manual before AI)
// and turns it into timed lines.
func (s *Service) downloadSubtitle(ctx context.Context, subs []subtitleEntry) ([]LyricLine, error) {
	var pick *subtitleEntry
	var ai *subtitleEntry
	for i := range subs {
		sub := &subs[i]
		if sub.SubURL == "" || sub.IsLock {
			continue
		}
		// "ai-zh" is how automatic Chinese captions are labelled: it is not a
		// "zh*" prefix, but it is still the song's words and must not be skipped.
		isAI := strings.HasPrefix(sub.Lan, "ai")
		isZH := strings.HasPrefix(sub.Lan, "zh") || (isAI && strings.Contains(sub.Lan, "zh"))
		if !isZH {
			continue
		}
		if !isAI {
			pick = sub
			break
		}
		if ai == nil {
			ai = sub
		}
	}
	if pick == nil {
		pick = ai
	}
	if pick == nil {
		for i := range subs {
			if subs[i].SubURL != "" && !subs[i].IsLock {
				pick = &subs[i]
				break
			}
		}
	}
	if pick == nil {
		return nil, nil
	}

	subURL := pick.SubURL
	if strings.HasPrefix(subURL, "//") {
		subURL = "https:" + subURL
	}

	resp, err := s.get(ctx, subURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("music: subtitle %s: %s", subURL, resp.Status)
	}
	var body struct {
		Body []struct {
			From    float64 `json:"from"`
			To      float64 `json:"to"`
			Content string  `json:"content"`
		} `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body); err != nil {
		return nil, err
	}
	lines := make([]LyricLine, 0, len(body.Body))
	for _, l := range body.Body {
		text := strings.TrimSpace(l.Content)
		if text == "" {
			continue
		}
		lines = append(lines, LyricLine{From: l.From, To: l.To, Text: text})
	}
	return lines, nil
}

// FetchImage pulls a Bilibili image (cover art, avatar) with the Referer the
// CDN expects. The webview cannot set it, and hotlink protection blanks them.
// Successful fetches are cached on disk under a bounded budget.
func (s *Service) FetchImage(ctx context.Context, rawURL string) ([]byte, string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", err
	}
	host := parsed.Hostname()
	if !(strings.HasSuffix(host, "hdslb.com") || strings.HasSuffix(host, "biliimg.com") ||
		strings.HasSuffix(host, "bilibili.com")) {
		return nil, "", fmt.Errorf("music: image host not allowed")
	}
	if parsed.Scheme == "http" {
		parsed.Scheme = "https"
	}
	key := sha1Hex(parsed.String())
	dir := filepath.Join(s.cacheDir, "covers")
	if data, err := os.ReadFile(filepath.Join(dir, key)); err == nil {
		return data, "image/jpeg", nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", referer)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("music: image %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, "", err
	}
	ctype := resp.Header.Get("Content-Type")
	if ctype == "" {
		ctype = "image/jpeg"
	}
	if len(data) > 0 && os.MkdirAll(dir, 0o755) == nil {
		_ = os.WriteFile(filepath.Join(dir, key), data, 0o644)
	}
	return data, ctype, nil
}

// evictCovers keeps the cover cache under 64 MB, oldest first.
func (s *Service) evictCovers() {
	dir := filepath.Join(s.cacheDir, "covers")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type info struct {
		name string
		size int64
		at   time.Time
	}
	var files []info
	var total int64
	for _, e := range entries {
		if st, err := e.Info(); err == nil {
			files = append(files, info{e.Name(), st.Size(), st.ModTime()})
			total += st.Size()
		}
	}
	const budget = 64 << 20
	if total <= budget {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].at.Before(files[j].at) })
	for _, f := range files {
		if total <= budget {
			break
		}
		if os.Remove(filepath.Join(dir, f.name)) == nil {
			total -= f.size
		}
	}
}

func sha1Hex(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
