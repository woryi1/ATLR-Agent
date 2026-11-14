package server

import (
	"embed"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-octo/octo-agent/internal/datahome"
)

// ─── Built-in backgrounds ───────────────────────────────────────────────────
//
// The ATLR-Agent rebuild ships two wallpaper images baked into the binary and
// seeds them nowhere: they are served straight from the embed, so the portable
// Data directory carries only the *choice* (selection.json's background field)
// and, when the user picks their own, the uploaded custom image under
// Data/backgrounds/.

//go:embed backgrounds/bloom.jpeg backgrounds/mystery.jpeg backgrounds/meadow.jpeg
var backgroundFS embed.FS

// backgroundIDs maps the selection ids a client may send to the embedded
// asset. "off" is not here — it means "no background image".
var backgroundAssets = map[string]string{
	"bloom":   "backgrounds/bloom.jpeg",
	"mystery": "backgrounds/mystery.jpeg",
	"meadow":  "backgrounds/meadow.jpeg",
}

// maxCustomBackgroundBytes caps an uploaded wallpaper at 15 MB — plenty for a
// photo, small enough that a mistimed upload can't bloat the portable Data
// directory.
const maxCustomBackgroundBytes = 15 << 20

// backgroundsDir returns the directory holding the user's custom wallpaper.
func backgroundsDir() string {
	dir, err := datahome.Path("backgrounds")
	if err != nil {
		return filepath.Join(".", "Data", "backgrounds")
	}
	return dir
}

// customBackgroundPath returns the file backing the "custom" background id.
func customBackgroundPath() string {
	return filepath.Join(backgroundsDir(), "custom.jpeg")
}

// handleGetBackground serves one background image. ids: bloom, mystery, meadow
// (embedded), custom (uploaded). Unknown ids answer 404.
func (s *Server) handleGetBackground(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	switch id {
	case "custom":
		data, err := os.ReadFile(customBackgroundPath())
		if err != nil {
			writeError(w, http.StatusNotFound, "no custom background uploaded")
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(data)
		return
	default:
		asset, ok := backgroundAssets[id]
		if !ok {
			writeError(w, http.StatusNotFound, "unknown background")
			return
		}
		data, err := backgroundFS.ReadFile(asset)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		// Embedded assets are immutable per build; let the webview cache them.
		w.Header().Set("Cache-Control", "max-age=86400")
		_, _ = w.Write(data)
	}
}

// handlePutBackgroundCustom accepts a multipart upload (field "file") and
// stores it as the user's custom wallpaper. The image is stored as-is; the
// browser scales it with CSS object-fit.
func (s *Server) handlePutBackgroundCustom(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCustomBackgroundBytes+1<<20)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid upload: "+err.Error())
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing file field")
		return
	}
	defer file.Close()

	// Sniff the leading bytes rather than trusting the filename: anything that
	// is not a JPEG/PNG/WebP is refused, so only ever images land in Data.
	head := make([]byte, 512)
	n, err := io.ReadFull(file, head)
	if err != nil && err != io.ErrUnexpectedEOF {
		writeError(w, http.StatusBadRequest, "read upload: "+err.Error())
		return
	}
	head = head[:n]
	if !isImageHead(head) {
		writeError(w, http.StatusBadRequest, "only JPEG/PNG/WebP images are accepted")
		return
	}

	if err := os.MkdirAll(backgroundsDir(), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out, err := os.Create(customBackgroundPath())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer out.Close()
	if _, err := out.Write(head); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := io.Copy(out, file); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "background": "custom"})
}

// isImageHead reports whether the magic bytes look like a JPEG, PNG or WebP.
func isImageHead(head []byte) bool {
	switch {
	case len(head) >= 3 && head[0] == 0xFF && head[1] == 0xD8 && head[2] == 0xFF: // JPEG
		return true
	case len(head) >= 8 && string(head[:8]) == "\x89PNG\r\n\x1a\n":
		return true
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		return true
	}
	return false
}

// validBackgroundID reports whether id is a background the selection may name.
// "" and "off" mean no wallpaper; anything else must be a known asset.
func validBackgroundID(id string) error {
	switch id {
	case "", "off":
		return nil
	default:
		if _, ok := backgroundAssets[id]; ok {
			return nil
		}
		if id == "custom" {
			if _, err := os.Stat(customBackgroundPath()); err != nil {
				return fmt.Errorf("no custom background has been uploaded")
			}
			return nil
		}
		return fmt.Errorf("unknown background %q", strings.TrimSpace(id))
	}
}
