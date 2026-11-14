package music

// Favouriting a search result.
//
// The app plays videos from outside the two folders the player is built on, so
// the "save this one" action has to write to the account's own favourites. The
// target folder is chosen the way a person would: first the folders that look
// like a music shelf, then, if none of them accepts the video, the default one.
//
// This lives on the host because the call needs the session cookies plus a CSRF
// token, and Bilibili rejects the request without them.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxFavBody caps the batch-deal response; it is a tiny JSON envelope.
const maxFavBody = 1 << 20

// musicFolderHints are the words that make a favourite folder a music shelf.
var musicFolderHints = []string{"音乐", "歌", "曲", "music", "bgm", "ost", "playlist"}

func isMusicFolder(title string) bool {
	t := strings.ToLower(strings.TrimSpace(title))
	if t == "" {
		return false
	}
	for _, hint := range musicFolderHints {
		if strings.Contains(t, hint) {
			return true
		}
	}
	return false
}

// Favorite adds bvid to the account's favourites and reports the folder used.
func (s *Service) Favorite(ctx context.Context, bvid string) (string, error) {
	if !ValidBVID(bvid) {
		return "", fmt.Errorf("music: invalid bvid")
	}
	s.mu.Lock()
	csrf := s.cookies["bili_jct"]
	s.mu.Unlock()
	if csrf == "" {
		return "", fmt.Errorf("music: 需要先扫码登录")
	}

	aid, err := s.videoAID(ctx, bvid)
	if err != nil {
		return "", err
	}
	folders, err := s.Folders(ctx)
	if err != nil {
		return "", err
	}
	if len(folders) == 0 {
		return "", fmt.Errorf("music: 账号下没有收藏夹")
	}

	// Music-named folders first, in their listed order; everything else (the
	// default shelf is listed first by Bilibili) is the fallback chain.
	ordered := make([]Folder, 0, len(folders))
	var rest []Folder
	for _, f := range folders {
		if isMusicFolder(f.Title) {
			ordered = append(ordered, f)
		} else {
			rest = append(rest, f)
		}
	}
	ordered = append(ordered, rest...)

	var lastErr error
	for _, f := range ordered {
		ok, msg := s.favoriteInto(ctx, aid, f.ID, csrf)
		if ok {
			return f.Title, nil
		}
		lastErr = fmt.Errorf("%s：%s", f.Title, msg)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("music: 收藏失败")
	}
	return "", lastErr
}

// videoAID resolves the numeric aid the favourite API wants.
func (s *Service) videoAID(ctx context.Context, bvid string) (int64, error) {
	var view struct {
		Code int `json:"code"`
		Data struct {
			AID int64 `json:"aid"`
		} `json:"data"`
	}
	if err := s.getJSON(ctx, apiBase+"/x/web-interface/view?bvid="+url.QueryEscape(bvid), &view); err != nil {
		return 0, err
	}
	if view.Code != 0 || view.Data.AID == 0 {
		return 0, fmt.Errorf("music: 无法解析 %s", bvid)
	}
	return view.Data.AID, nil
}

// favoriteInto posts one batch-deal. ok is true when the folder already held
// the video too — from the user's point of view that is the same outcome.
func (s *Service) favoriteInto(ctx context.Context, aid, mediaID int64, csrf string) (bool, string) {
	form := url.Values{}
	form.Set("resources", fmt.Sprintf("%d:2", aid))
	form.Set("add_media_ids", fmt.Sprintf("%d", mediaID))
	form.Set("del_media_ids", "")
	form.Set("csrf", csrf)

	req, err := s.request(ctx, http.MethodPost, apiBase+"/x/v3/fav/resource/batch-deal", strings.NewReader(form.Encode()))
	if err != nil {
		return false, err.Error()
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, resp.Status
	}
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxFavBody)).Decode(&out); err != nil {
		return false, "返回内容无法解析"
	}
	switch out.Code {
	case 0:
		return true, ""
	case 11202: // already favourited in this folder
		return true, ""
	default:
		if out.Message == "" {
			return false, fmt.Sprintf("code %d", out.Code)
		}
		return false, out.Message
	}
}
