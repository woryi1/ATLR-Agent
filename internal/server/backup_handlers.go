package server

// Encrypted backup endpoints: Settings → 数据管理 → 数据备份.
//
// The password travels in a request header (multipart) or the JSON body
// (export) and is never logged or persisted — the only place it survives is
// the encrypted container itself.

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-octo/octo-agent/internal/backup"
	"github.com/open-octo/octo-agent/internal/datahome"
	"github.com/open-octo/octo-agent/internal/version"
)

const backupPasswordHeader = "X-Backup-Password"

func (s *Server) handleBackupSections(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sections": backup.Sections})
}

func (s *Server) handleBackupExport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Sections []string `json:"sections"`
		Password string   `json:"password"`
	}
	if err := readBodyJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(body.Password) == "" {
		writeError(w, http.StatusBadRequest, "backup password is required")
		return
	}
	res, err := backup.Export(backup.ExportOptions{
		Sections: body.Sections,
		Password: body.Password,
		App:      "ATLR-Agent " + version.Version,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleBackupPreview decrypts an uploaded package and describes it without
// touching any data.
func (s *Server) handleBackupPreview(w http.ResponseWriter, r *http.Request) {
	password := r.Header.Get(backupPasswordHeader)
	if password == "" {
		writeError(w, http.StatusBadRequest, "backup password is required")
		return
	}
	pkg, cleanup, err := receiveBackupPackage(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	defer cleanup()

	sum, zipPath, err := backup.ReadPreview(pkg, password)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = os.Remove(zipPath)

	size := int64(0)
	if st, statErr := os.Stat(pkg); statErr == nil {
		size = st.Size()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sections":   sum.Sections,
		"restorable": sum.Restorable,
		"created_at": sum.Created,
		"files":      sum.Files,
		"bytes":      sum.Bytes,
		"app":        sum.App,
		"version":    sum.Version,
		"size":       size,
	})
}

// handleBackupApply restores the selected sections, parking the current data
// under Cache\pre-restore-<time> first.
func (s *Server) handleBackupApply(w http.ResponseWriter, r *http.Request) {
	password := r.Header.Get(backupPasswordHeader)
	if password == "" {
		writeError(w, http.StatusBadRequest, "backup password is required")
		return
	}
	var wanted []string
	for _, id := range strings.Split(r.URL.Query().Get("sections"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			wanted = append(wanted, id)
		}
	}
	pkg, cleanup, err := receiveBackupPackage(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	defer cleanup()

	res, err := backup.Apply(pkg, password, wanted)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleBackupOpen opens the export folder (or the pre-restore folder) in the
// system file manager. Only paths under Cache are accepted.
func (s *Server) handleBackupOpen(w http.ResponseWriter, r *http.Request) {
	base, err := datahome.RegionPath(datahome.RegionCache)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	dir := strings.TrimSpace(r.URL.Query().Get("dir"))
	if dir == "" {
		if dir, err = backup.ExportsDir(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	cleanDir := filepath.Clean(dir)
	if cleanDir != filepath.Clean(base) && !strings.HasPrefix(cleanDir, filepath.Clean(base)+string(filepath.Separator)) {
		writeError(w, http.StatusBadRequest, "path is outside the cache folder")
		return
	}
	// The UI usually hands over the package itself; open its folder instead.
	if st, statErr := os.Stat(cleanDir); statErr == nil && !st.IsDir() {
		cleanDir = filepath.Dir(cleanDir)
	}
	if err := os.MkdirAll(cleanDir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := openInSystemBrowser(cleanDir); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// receiveBackupPackage streams the "file" part to a temp file. Streaming keeps
// a multi-hundred-megabyte package out of memory.
func receiveBackupPackage(r *http.Request) (string, func(), error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return "", nil, fmt.Errorf("expected multipart/form-data: %w", err)
	}
	const maxBytes = int64(8) << 30
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nil, fmt.Errorf("read upload: %w", err)
		}
		if part.FormName() != "file" {
			_ = part.Close()
			continue
		}
		tmp, err := os.CreateTemp("", "atlr-upload-*.atlrbak")
		if err != nil {
			return "", nil, err
		}
		path := tmp.Name()
		n, copyErr := io.Copy(tmp, io.LimitReader(part, maxBytes+1))
		closeErr := tmp.Close()
		_ = part.Close()
		if copyErr != nil {
			_ = os.Remove(path)
			return "", nil, copyErr
		}
		if closeErr != nil {
			_ = os.Remove(path)
			return "", nil, closeErr
		}
		if n > maxBytes {
			_ = os.Remove(path)
			return "", nil, fmt.Errorf("package is too large")
		}
		if n == 0 {
			_ = os.Remove(path)
			return "", nil, fmt.Errorf("uploaded file is empty")
		}
		return path, func() { _ = os.Remove(path) }, nil
	}
	return "", nil, fmt.Errorf("missing file field")
}
