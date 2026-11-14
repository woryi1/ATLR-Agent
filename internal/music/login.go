package music

// Bilibili QR login.
//
// The desktop app has no browser session, so the account is linked the way the
// phone app does it: show a QR, let the user scan it in the Bilibili app, and
// keep the cookies the passport service returns. Those cookies raise the audio
// quality ceiling and make the favourites call far less likely to be refused.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/open-octo/octo-agent/internal/datahome"
)

const passportBase = "https://passport.bilibili.com"

// LoginInfo is what the player UI shows in the corner of the panel.
type LoginInfo struct {
	LoggedIn bool   `json:"logged_in"`
	Name     string `json:"name"`
	Face     string `json:"face"`
}

func (s *Service) cookieFile() string {
	p, err := datahome.Path("bili-cookies.json")
	if err != nil {
		return ""
	}
	return p
}

func (s *Service) loadCookies() {
	cookiePath := s.cookieFile()
	if cookiePath == "" {
		return
	}
	data, err := os.ReadFile(cookiePath)
	if err != nil {
		return
	}
	var stored map[string]string
	if json.Unmarshal(data, &stored) != nil {
		return
	}
	s.mu.Lock()
	for k, v := range stored {
		s.cookies[k] = v
	}
	s.mu.Unlock()
}

func (s *Service) saveCookies() {
	path := s.cookieFile()
	if path == "" {
		return
	}
	s.mu.Lock()
	data, err := json.MarshalIndent(s.cookies, "", " ")
	s.mu.Unlock()
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

// QRGenerate asks passport for a login QR and its polling key.
func (s *Service) QRGenerate(ctx context.Context) (string, string, error) {
	var out struct {
		Code int `json:"code"`
		Data struct {
			URL       string `json:"url"`
			QrcodeKey string `json:"qrcode_key"`
		} `json:"data"`
	}
	if err := s.getJSON(ctx, passportBase+"/x/passport-login/web/qrcode/generate", &out); err != nil {
		return "", "", err
	}
	if out.Code != 0 || out.Data.URL == "" {
		return "", "", fmt.Errorf("bilibili: cannot create a login QR (code %d)", out.Code)
	}
	return out.Data.URL, out.Data.QrcodeKey, nil
}

// QRPoll checks one scan. status is one of waiting / scanned / expired / ok.
func (s *Service) QRPoll(ctx context.Context, key string) (LoginInfo, string, error) {
	resp, err := s.get(ctx, passportBase+"/x/passport-login/web/qrcode/poll?qrcode_key="+key)
	if err != nil {
		return LoginInfo{}, "error", err
	}
	defer resp.Body.Close()
	var out struct {
		Code int `json:"code"`
		Data struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return LoginInfo{}, "error", err
	}
	switch out.Data.Code {
	case 86101:
		return LoginInfo{}, "waiting", nil
	case 86090:
		return LoginInfo{}, "scanned", nil
	case 86038:
		return LoginInfo{}, "expired", nil
	case 0:
		s.absorbCookies(resp)
		info := s.Me(ctx)
		return info, "ok", nil
	default:
		return LoginInfo{}, "waiting", nil
	}
}

// absorbCookies stores SESSDATA and friends from a passport response.
func (s *Service) absorbCookies(resp *http.Response) {
	keep := map[string]bool{
		"SESSDATA": true, "bili_jct": true, "DedeUserID": true,
		"DedeUserID__ckMd5": true, "sid": true,
	}
	s.mu.Lock()
	for _, c := range resp.Cookies() {
		if keep[c.Name] {
			s.cookies[c.Name] = c.Value
		}
	}
	s.mu.Unlock()
	s.saveCookies()
}

// Me reports the signed-in account (or LoggedIn:false).
func (s *Service) Me(ctx context.Context) LoginInfo {
	var nav struct {
		Code int `json:"code"`
		Data struct {
			IsLogin bool   `json:"isLogin"`
			Uname   string `json:"uname"`
			Face    string `json:"face"`
		} `json:"data"`
	}
	if err := s.getJSON(ctx, apiBase+"/x/web-interface/nav", &nav); err != nil {
		return LoginInfo{}
	}
	if nav.Code != 0 || !nav.Data.IsLogin {
		return LoginInfo{}
	}
	return LoginInfo{LoggedIn: true, Name: nav.Data.Uname, Face: nav.Data.Face}
}

// Logout drops the account cookies but keeps the guest fingerprint.
func (s *Service) Logout() {
	s.mu.Lock()
	for _, k := range []string{"SESSDATA", "bili_jct", "DedeUserID", "DedeUserID__ckMd5", "sid"} {
		delete(s.cookies, k)
	}
	s.mu.Unlock()
	s.saveCookies()
}

// Folder is one of the account's favourites.
type Folder struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Count int    `json:"count"`
}

// Folders lists every favourite of the signed-in account, the way the Bilibili
// client does. It is the endpoint PiliPlus (BiliPlus) uses for the same screen;
// the web-facing list API only exposes one media_id at a time.
func (s *Service) Folders(ctx context.Context) ([]Folder, error) {
	var nav struct {
		Code int `json:"code"`
		Data struct {
			IsLogin bool  `json:"isLogin"`
			Mid     int64 `json:"mid"`
		} `json:"data"`
	}
	if err := s.getJSON(ctx, apiBase+"/x/web-interface/nav", &nav); err != nil {
		return nil, err
	}
	if nav.Code != 0 || !nav.Data.IsLogin || nav.Data.Mid == 0 {
		return nil, fmt.Errorf("music: not logged in")
	}
	var out struct {
		Code int `json:"code"`
		Data struct {
			List []struct {
				ID         int64  `json:"id"`
				Title      string `json:"title"`
				MediaCount int    `json:"media_count"`
			} `json:"list"`
		} `json:"data"`
	}
	u := fmt.Sprintf("%s/x/v3/fav/folder/created/list-all?up_mid=%d", apiBase, nav.Data.Mid)
	if err := s.getJSON(ctx, u, &out); err != nil {
		return nil, err
	}
	if out.Code != 0 {
		return nil, fmt.Errorf("music: folders refused (code %d)", out.Code)
	}
	folders := make([]Folder, 0, len(out.Data.List))
	for _, f := range out.Data.List {
		folders = append(folders, Folder{ID: f.ID, Title: f.Title, Count: f.MediaCount})
	}
	return folders, nil
}

// QRPNG renders a login URL as a PNG for the panel.
func QRPNG(text string, size int) ([]byte, error) {
	if size <= 0 {
		size = 240
	}
	return qrcode.Encode(text, qrcode.Medium, size)
}
