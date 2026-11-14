package music

// Search: the two favourites first (already in memory), then Bilibili's own
// video search appended behind them. The remote call is WBI-signed for the same
// reason player/v2 is — unsigned answers are generic and unreliable.

import (
	"context"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var tagRe = regexp.MustCompile(`<[^>]*>`)

// SearchRemote asks Bilibili for videos matching q and returns playable tracks.
func (s *Service) SearchRemote(ctx context.Context, q string) ([]Track, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, nil
	}
	query, err := s.signWBI(ctx, map[string]string{
		"search_type": "video",
		"keyword":     q,
		"page":        "1",
		"page_size":   "20",
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		Code int `json:"code"`
		Data struct {
			Result []struct {
				BVID     string `json:"bvid"`
				Title    string `json:"title"`
				Author   string `json:"author"`
				Pic      string `json:"pic"`
				Duration string `json:"duration"` // "mm:ss"
				Type     string `json:"type"`
			} `json:"result"`
		} `json:"data"`
	}
	u := apiBase + "/x/web-interface/wbi/search/type?" + query
	if err := s.getJSON(ctx, u, &out); err != nil {
		return nil, err
	}
	if out.Code != 0 {
		return nil, fmt.Errorf("music: search refused (code %d)", out.Code)
	}
	tracks := make([]Track, 0, len(out.Data.Result))
	for _, r := range out.Data.Result {
		if r.BVID == "" || r.Type != "video" {
			continue
		}
		title := strings.TrimSpace(html.UnescapeString(tagRe.ReplaceAllString(r.Title, "")))
		pic := r.Pic
		if strings.HasPrefix(pic, "//") {
			pic = "https:" + pic
		}
		tracks = append(tracks, Track{
			BVID:     r.BVID,
			Title:    title,
			Artist:   r.Author,
			Cover:    pic,
			Duration: parseClock(r.Duration),
		})
	}
	return tracks, nil
}

func parseClock(v string) int {
	parts := strings.Split(v, ":")
	total := 0
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return 0
		}
		total = total*60 + n
	}
	return total
}

// ValidBVID keeps the stream proxy from being asked to resolve arbitrary URLs.
func ValidBVID(bvid string) bool {
	if len(bvid) != 12 || !strings.HasPrefix(bvid, "BV") {
		return false
	}
	for _, r := range bvid[2:] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

var _ = url.QueryEscape
