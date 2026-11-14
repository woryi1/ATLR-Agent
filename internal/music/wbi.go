package music

// WBI signing, ported from PiliPlus (lib/utils/wbi_sign.dart).
//
// Bilibili's newer endpoints (/x/player/wbi/v2 among them) reject unsigned
// calls; an unsigned player/v2 answers with a generic payload whose subtitle
// list belongs to a different video — which is exactly why the lyric panel
// showed another song's words.

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

var mixinKeyEncTab = []int{
	46, 47, 18, 2, 53, 8, 23, 32, 15, 50, 10, 31, 58, 3, 45, 35, 27, 43, 5, 49,
	33, 9, 42, 19, 29, 28, 14, 39, 12, 38, 41, 13,
}

var (
	wbiMu      sync.Mutex
	wbiMixin   string
	wbiFetched time.Time
)

func mixinKeyFrom(imgKey, subKey string) string {
	orig := imgKey + subKey
	var b strings.Builder
	for _, i := range mixinKeyEncTab {
		if i < len(orig) {
			b.WriteByte(orig[i])
		}
	}
	return b.String()
}

func fileStem(u string) string {
	if i := strings.LastIndex(u, "/"); i >= 0 {
		u = u[i+1:]
	}
	if i := strings.Index(u, "."); i >= 0 {
		u = u[:i]
	}
	return u
}

// wbiKey returns the current mixin key, cached for the day.
func (s *Service) wbiKey(ctx context.Context) (string, error) {
	wbiMu.Lock()
	if wbiMixin != "" && time.Since(wbiFetched) < 12*time.Hour {
		k := wbiMixin
		wbiMu.Unlock()
		return k, nil
	}
	wbiMu.Unlock()

	var nav struct {
		Data struct {
			WbiImg struct {
				ImgURL string `json:"img_url"`
				SubURL string `json:"sub_url"`
			} `json:"wbi_img"`
		} `json:"data"`
	}
	if err := s.getJSON(ctx, apiBase+"/x/web-interface/nav", &nav); err != nil {
		return "", err
	}
	img, sub := fileStem(nav.Data.WbiImg.ImgURL), fileStem(nav.Data.WbiImg.SubURL)
	if img == "" || sub == "" {
		return "", fmt.Errorf("music: nav returned no wbi keys")
	}
	key := mixinKeyFrom(img, sub)
	wbiMu.Lock()
	wbiMixin, wbiFetched = key, time.Now()
	wbiMu.Unlock()
	return key, nil
}

// resetWBI drops the cached key so the next signature refetches it.
func (s *Service) resetWBI() {
	wbiMu.Lock()
	wbiMixin = ""
	wbiFetched = time.Time{}
	wbiMu.Unlock()
}

// signWBI adds wts/w_rid to params and returns the encoded query string.
func (s *Service) signWBI(ctx context.Context, params map[string]string) (string, error) {
	key, err := s.wbiKey(ctx)
	if err != nil {
		return "", err
	}
	params["wts"] = fmt.Sprintf("%d", time.Now().Unix())
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	strip := func(v string) string {
		return strings.NewReplacer("!", "", "'", "", "(", "", ")", "", "*", "").Replace(v)
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(strip(params[k])))
	}
	query := strings.Join(parts, "&")
	sum := md5.Sum([]byte(query + key))
	return query + "&w_rid=" + hex.EncodeToString(sum[:]), nil
}
