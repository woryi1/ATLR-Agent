package voice

// Anonymous (guest) bootstrap: the ASR gateway needs the web_id / aid /
// pc_version the Doubao homepage embeds in window._ROUTER_DATA, plus the guest
// cookies it hands out. Nothing here is an account credential.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const homepage = "https://www.doubao.com/chat/"

var homepageURL, _ = url.Parse(homepage)

var routerDataRe = regexp.MustCompile(`window\._ROUTER_DATA\s*=\s*`)

// BootstrapResult carries the guest session parameters.
type BootstrapResult struct {
	WebID     string
	AID       string
	PCVersion string
	Cookies   []*http.Cookie
}

// cachedBootstrap reuses the guest parameters across sessions. Fetching the
// homepage costs about a second and pulls ~340 KB; repeating it for every
// utterance was pure latency in front of the gateway handshake. The TTL keeps
// stale cookies from living forever.
func cachedBootstrap() (*BootstrapResult, error) {
	bootMu.Lock()
	defer bootMu.Unlock()
	if bootCache != nil && time.Since(bootAt) < bootTTL {
		return bootCache, nil
	}
	b, err := Bootstrap(userAgent)
	if err != nil {
		return nil, err
	}
	bootCache, bootAt = b, time.Now()
	return b, nil
}

var (
	bootMu    sync.Mutex
	bootCache *BootstrapResult
	bootAt    time.Time
)

const bootTTL = 10 * time.Minute

// Bootstrap fetches the homepage and extracts the guest session parameters.
func Bootstrap(userAgent string) (*BootstrapResult, error) {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 20 * time.Second}
	req, _ := http.NewRequest("GET", homepage, nil)
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch doubao homepage: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read homepage: %w", err)
	}
	html := string(body)

	loc := routerDataRe.FindStringIndex(html)
	if loc == nil {
		return nil, fmt.Errorf("homepage has no _ROUTER_DATA (page structure changed)")
	}
	// Stream-decode from just after the marker; the decoder stops at the end of
	// the object, so trailing markup is harmless. The shape nests differently
	// between releases (chat_layout.chat_layout now, launchCore before), so walk
	// the whole tree for the three values instead of pinning a path.
	dec := json.NewDecoder(strings.NewReader(html[loc[1]:]))
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("parse _ROUTER_DATA: %w", err)
	}
	result := &BootstrapResult{}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if result.WebID == "" {
				if s, ok := t["web_id"].(string); ok && s != "" {
					result.WebID = s
				}
			}
			if result.AID == "" {
				switch a := t["aid"].(type) {
				case float64:
					result.AID = fmt.Sprintf("%.0f", a)
				case string:
					result.AID = a
				}
			}
			if result.PCVersion == "" {
				if s, ok := t["pc_version"].(string); ok && s != "" {
					result.PCVersion = s
				}
			}
			for _, child := range t {
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(root)
	// Last resort: the values are also plainly visible in the raw markup.
	if result.WebID == "" {
		if m := regexp.MustCompile(`"web_id"\s*:\s*"([^"]+)"`).FindStringSubmatch(html); m != nil {
			result.WebID = m[1]
		}
	}
	if result.AID == "" {
		if m := regexp.MustCompile(`"aid"\s*:\s*(\d+)`).FindStringSubmatch(html); m != nil {
			result.AID = m[1]
		}
	}
	if result.PCVersion == "" {
		if m := regexp.MustCompile(`"pc_version"\s*:\s*"([^"]+)"`).FindStringSubmatch(html); m != nil {
			result.PCVersion = m[1]
		}
	}
	if result.WebID == "" || result.AID == "" {
		return nil, fmt.Errorf("_ROUTER_DATA is missing web_id/aid")
	}
	result.Cookies = jar.Cookies(homepageURL)
	return result, nil
}
