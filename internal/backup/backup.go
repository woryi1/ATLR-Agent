// Package backup builds and restores the encrypted data package offered in
// Settings → 数据管理 → 数据备份.
//
// A package is a ZIP of selected data regions, encrypted with a password that
// only ever lives in the user's head: the host derives a key with
// PBKDF2-HMAC-SHA256, then seals the archive in AES-256-GCM chunks. Nothing
// about the password is written to disk, logged, or returned by any endpoint.
//
// Container layout (all integers big-endian):
//
//	magic   [8]byte  "ATLRBK01"
//	version [1]byte
//	salt    [16]byte  PBKDF2 salt
//	nonce   [12]byte  base nonce; chunk N uses it with N in the last 4 bytes
//	chunks: uint32 length + AES-GCM ciphertext
package backup

import (
	"archive/zip"
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/open-octo/octo-agent/internal/datahome"
)

const (
	// Magic identifies the container. Import refuses anything else, which is
	// also how drag-and-drop tells our package apart from a stray zip.
	Magic   = "ATLRBK01"
	Version = 1
	// Ext is the file suffix the UI filters for.
	Ext = ".atlrbak"

	chunkSize  = 1 << 20 // 1 MiB plaintext per AEAD chunk
	saltSize   = 16
	nonceSize  = 12
	iterations = 210000
	keySize    = 32
	// maxPackageBytes caps what import will accept from a request.
	maxPackageBytes = 8 << 30
)

// Section is one selectable slice of the data directory.
type Section struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Hint  string `json:"hint"`
	// Region is the top-level folder under the data root (layout v2).
	Region string `json:"-"`
}

// Sections is the export/import menu, in display order. Cache is deliberately
// absent: it is rebuildable and would dwarf the real data.
var Sections = []Section{
	{ID: "user", Label: "设置与凭据", Hint: "API Key、模型配置、登录态、主题、上传附件", Region: datahome.RegionUser},
	{ID: "records", Label: "会话与记忆", Hint: "聊天记录、项目记忆、定时任务、工作流日志", Region: datahome.RegionRecords},
	{ID: "tools", Label: "技能与智能体", Hint: "技能、子智能体、MCP 配置与令牌", Region: datahome.RegionTools},
	{ID: "work", Label: "工作目录", Hint: "Work 下的项目文件，可能很大", Region: datahome.RegionWork},
}

// SectionByID returns the section with id, or false.
func SectionByID(id string) (Section, bool) {
	for _, s := range Sections {
		if s.ID == id {
			return s, true
		}
	}
	return Section{}, false
}

// manifest is stored as manifest.json inside the encrypted ZIP.
type manifest struct {
	Format    string    `json:"format"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	App       string    `json:"app"`
	Sections  []string  `json:"sections"`
	Files     int       `json:"files"`
	Bytes     int64     `json:"bytes"`
}

// ExportOptions selects what to pack.
type ExportOptions struct {
	Sections []string
	Password string
	App      string
}

// ExportResult describes the package that was written.
type ExportResult struct {
	Path     string    `json:"path"`
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Files    int       `json:"files"`
	Sections []string  `json:"sections"`
	Created  time.Time `json:"created_at"`
}

// Summary is what the import preview shows before anything is overwritten.
type Summary struct {
	Sections   []string  `json:"sections"`
	Created    time.Time `json:"created_at"`
	Files      int       `json:"files"`
	Bytes      int64     `json:"bytes"`
	App        string    `json:"app"`
	Version    int       `json:"version"`
	Restorable []string  `json:"restorable"`
}

// ExportsDir is where finished packages land.
func ExportsDir() (string, error) {
	return datahome.RegionPath(datahome.RegionCache, "exports")
}

// Export walks the selected regions, builds a ZIP and encrypts it.
func Export(opts ExportOptions) (ExportResult, error) {
	if strings.TrimSpace(opts.Password) == "" {
		return ExportResult{}, fmt.Errorf("backup: a password is required")
	}
	dataDir, err := datahome.Dir()
	if err != nil {
		return ExportResult{}, err
	}
	sections, err := normalizeSections(opts.Sections)
	if err != nil {
		return ExportResult{}, err
	}
	if len(sections) == 0 {
		return ExportResult{}, fmt.Errorf("backup: nothing selected")
	}

	outDir, err := ExportsDir()
	if err != nil {
		return ExportResult{}, err
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return ExportResult{}, err
	}
	now := time.Now()
	name := "ATLR-Backup-" + now.Format("20060102-1504") + Ext
	outPath := filepath.Join(outDir, name)

	tmp, err := os.CreateTemp("", "atlr-backup-*.zip")
	if err != nil {
		return ExportResult{}, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	files, bytes, err := collectFiles(dataDir, sections)
	if err != nil {
		return ExportResult{}, err
	}
	man := manifest{
		Format: Magic, Version: Version, CreatedAt: now, App: opts.App,
		Sections: sections, Files: len(files), Bytes: bytes,
	}
	if err := writeZip(tmpPath, files, man); err != nil {
		return ExportResult{}, err
	}
	if err := encryptFile(tmpPath, outPath, opts.Password); err != nil {
		_ = os.Remove(outPath)
		return ExportResult{}, err
	}
	stat, err := os.Stat(outPath)
	if err != nil {
		return ExportResult{}, err
	}
	return ExportResult{
		Path: outPath, Name: name, Size: stat.Size(), Files: len(files),
		Sections: sections, Created: now,
	}, nil
}

func normalizeSections(ids []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		if _, ok := SectionByID(id); !ok {
			return nil, fmt.Errorf("backup: unknown section %q", id)
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.SliceStable(out, func(i, j int) bool {
		// Keep the display order of Sections, not the request order.
		return sectionIndex(out[i]) < sectionIndex(out[j])
	})
	return out, nil
}

func sectionIndex(id string) int {
	for i, s := range Sections {
		if s.ID == id {
			return i
		}
	}
	return len(Sections)
}

// zipEntry is one file queued for packing.
type zipEntry struct {
	abs string
	rel string
	mod time.Time
}

// collectFiles lists what the selected regions contain. Skipping transient
// junk here keeps the package deterministic and small.
func collectFiles(dataDir string, sections []string) ([]zipEntry, int64, error) {
	var entries []zipEntry
	var total int64
	for _, id := range sections {
		sec, _ := SectionByID(id)
		root := filepath.Join(dataDir, sec.Region)
		if _, err := os.Stat(root); err != nil {
			continue
		}
		err := filepath.Walk(root, func(p string, info os.FileInfo, walkErr error) error {
			if walkErr != nil || info == nil || info.IsDir() {
				return nil
			}
			base := filepath.Base(p)
			if base == "migrate.log" || strings.HasSuffix(base, ".lock") || strings.HasSuffix(base, ".tmp") {
				return nil
			}
			rel, err := filepath.Rel(dataDir, p)
			if err != nil || strings.HasPrefix(rel, "..") {
				return nil
			}
			entries = append(entries, zipEntry{abs: p, rel: filepath.ToSlash(rel), mod: info.ModTime()})
			total += info.Size()
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
	}
	return entries, total, nil
}

// writeZip packs the manifest and every queued file into path.
func writeZip(path string, entries []zipEntry, man manifest) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)

	data, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return err
	}
	w, err := zw.Create("manifest.json")
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}

	for _, e := range entries {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: e.rel, Method: zip.Deflate, Modified: e.mod})
		if err != nil {
			zw.Close()
			return err
		}
		src, err := os.Open(e.abs)
		if err != nil {
			continue // unreadable file: skip, do not fail the whole export
		}
		_, copyErr := io.Copy(w, src)
		src.Close()
		if copyErr != nil {
			zw.Close()
			return copyErr
		}
	}
	return zw.Close()
}

func encryptFile(srcPath, dstPath, password string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer dst.Close()

	salt := make([]byte, saltSize)
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	if _, err := dst.Write([]byte(Magic)); err != nil {
		return err
	}
	if _, err := dst.Write([]byte{Version}); err != nil {
		return err
	}
	if _, err := dst.Write(salt); err != nil {
		return err
	}
	if _, err := dst.Write(nonce); err != nil {
		return err
	}

	aead, err := newAEAD(password, salt)
	if err != nil {
		return err
	}
	reader := bufio.NewReaderSize(src, chunkSize)
	buf := make([]byte, chunkSize)
	var counter uint32
	for {
		n, readErr := io.ReadFull(reader, buf)
		if readErr == io.EOF {
			break
		}
		if readErr != nil && readErr != io.ErrUnexpectedEOF {
			return readErr
		}
		sealed := aead.Seal(nil, chunkNonce(nonce, counter), buf[:n], nil)
		if err := binary.Write(dst, binary.BigEndian, uint32(len(sealed))); err != nil {
			return err
		}
		if _, err := dst.Write(sealed); err != nil {
			return err
		}
		counter++
		if readErr == io.ErrUnexpectedEOF {
			break
		}
	}
	return dst.Sync()
}

func chunkNonce(base []byte, n uint32) []byte {
	out := make([]byte, nonceSize)
	copy(out, base)
	binary.BigEndian.PutUint32(out[nonceSize-4:], n)
	return out
}

func newAEAD(password string, salt []byte) (cipher.AEAD, error) {
	key := pbkdf2SHA256([]byte(password), salt, iterations, keySize)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// ReadPreview decrypts the package to a temp file and describes it. The caller
// must call Cleanup when done (Apply does it itself).
func ReadPreview(pkgPath, password string) (Summary, string, error) {
	zipPath, err := decryptFile(pkgPath, password)
	if err != nil {
		return Summary{}, "", err
	}
	sum, err := readManifest(zipPath)
	if err != nil {
		_ = os.Remove(zipPath)
		return Summary{}, "", err
	}
	return sum, zipPath, nil
}

func readManifest(zipPath string) (Summary, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return Summary{}, fmt.Errorf("backup: package is not a valid archive")
	}
	defer zr.Close()
	var sum Summary
	for _, e := range zr.File {
		if e.Name != "manifest.json" {
			continue
		}
		rc, err := e.Open()
		if err != nil {
			return Summary{}, err
		}
		var man manifest
		err = json.NewDecoder(io.LimitReader(rc, 1<<20)).Decode(&man)
		rc.Close()
		if err != nil {
			return Summary{}, fmt.Errorf("backup: manifest is unreadable")
		}
		if man.Format != Magic || man.Version > Version {
			return Summary{}, fmt.Errorf("backup: unsupported package (format %q v%d)", man.Format, man.Version)
		}
		sum = Summary{
			Sections: man.Sections, Created: man.CreatedAt, Files: man.Files,
			Bytes: man.Bytes, App: man.App, Version: man.Version,
		}
		for _, id := range man.Sections {
			if _, ok := SectionByID(id); ok {
				sum.Restorable = append(sum.Restorable, id)
			}
		}
		return sum, nil
	}
	return Summary{}, fmt.Errorf("backup: package has no manifest")
}

func decryptFile(pkgPath, password string) (string, error) {
	src, err := os.Open(pkgPath)
	if err != nil {
		return "", err
	}
	defer src.Close()
	magic := make([]byte, len(Magic))
	if _, err := io.ReadFull(src, magic); err != nil || string(magic) != Magic {
		return "", fmt.Errorf("backup: 这不是 ATLR 备份包")
	}
	ver := make([]byte, 1)
	if _, err := io.ReadFull(src, ver); err != nil {
		return "", err
	}
	if int(ver[0]) > Version {
		return "", fmt.Errorf("backup: 备份包版本过新（v%d）", ver[0])
	}
	salt := make([]byte, saltSize)
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(src, salt); err != nil {
		return "", err
	}
	if _, err := io.ReadFull(src, nonce); err != nil {
		return "", err
	}
	aead, err := newAEAD(password, salt)
	if err != nil {
		return "", err
	}

	tmp, err := os.CreateTemp("", "atlr-restore-*.zip")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer tmp.Close()

	reader := bufio.NewReaderSize(src, chunkSize)
	var counter uint32
	for {
		var length uint32
		if err := binary.Read(reader, binary.BigEndian, &length); err != nil {
			if err == io.EOF {
				break
			}
			_ = os.Remove(tmpPath)
			return "", err
		}
		if length == 0 || length > chunkSize+(16*4) {
			_ = os.Remove(tmpPath)
			return "", fmt.Errorf("backup: 备份包已损坏")
		}
		sealed := make([]byte, length)
		if _, err := io.ReadFull(reader, sealed); err != nil {
			_ = os.Remove(tmpPath)
			return "", err
		}
		plain, err := aead.Open(nil, chunkNonce(nonce, counter), sealed, nil)
		if err != nil {
			_ = os.Remove(tmpPath)
			return "", fmt.Errorf("backup: 密码不正确或备份包已损坏")
		}
		if _, err := tmp.Write(plain); err != nil {
			_ = os.Remove(tmpPath)
			return "", err
		}
		counter++
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	return tmpPath, nil
}

// ApplyResult reports what was restored.
type ApplyResult struct {
	Sections []string `json:"sections"`
	Files    int      `json:"files"`
}

// Apply restores the selected sections. Import is destructive by design: the
// live folder is replaced with the package contents and the old one is deleted.
// The new tree is extracted beside the live one first, so a failed extraction
// still leaves the user's data untouched.
func Apply(pkgPath, password string, wanted []string) (ApplyResult, error) {
	sum, zipPath, err := ReadPreview(pkgPath, password)
	if err != nil {
		return ApplyResult{}, err
	}
	defer os.Remove(zipPath)

	available := map[string]bool{}
	for _, id := range sum.Restorable {
		available[id] = true
	}
	var sections []string
	for _, id := range wanted {
		if available[id] {
			sections = append(sections, id)
		}
	}
	if len(sections) == 0 {
		return ApplyResult{}, fmt.Errorf("backup: 备份包里没有所选的数据")
	}

	dataDir, err := datahome.Dir()
	if err != nil {
		return ApplyResult{}, err
	}

	files := 0
	for _, id := range sections {
		sec, _ := SectionByID(id)
		live := filepath.Join(dataDir, sec.Region)
		staged := live + ".restore-tmp"
		old := live + ".restore-old"
		if err := os.RemoveAll(staged); err != nil {
			return ApplyResult{}, err
		}
		if err := os.RemoveAll(old); err != nil {
			return ApplyResult{}, err
		}
		// extractRegion always creates the destination, even when the package
		// carries no files for this region (an empty Work folder is a valid
		// restore target).
		n, err := extractRegion(zipPath, sec.Region, staged)
		if err != nil {
			_ = os.RemoveAll(staged)
			return ApplyResult{}, fmt.Errorf("backup: 写入 %s 失败：%w", sec.Label, err)
		}
		// Move the live folder aside instead of deleting it, so a failed swap
		// can put it back without any data loss.
		hadLive := false
		if _, statErr := os.Stat(live); statErr == nil {
			if err := os.Rename(live, old); err != nil {
				_ = os.RemoveAll(staged)
				return ApplyResult{}, fmt.Errorf("backup: 暂存旧 %s 失败：%w", sec.Label, err)
			}
			hadLive = true
		}
		if err := os.Rename(staged, live); err != nil {
			if hadLive {
				_ = os.Rename(old, live)
			}
			_ = os.RemoveAll(staged)
			return ApplyResult{}, fmt.Errorf("backup: 替换 %s 失败：%w", sec.Label, err)
		}
		_ = os.RemoveAll(old)
		files += n
	}
	return ApplyResult{Sections: sections, Files: files}, nil
}

// extractRegion writes every entry under region/ into dst, refusing anything
// that would escape it.
func extractRegion(zipPath, region, dst string) (int, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return 0, err
	}
	defer zr.Close()
	// Create the destination up front: a region with zero entries in the package
	// still restores as an (empty) folder, which is what makes the swap work.
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return 0, err
	}
	prefix := region + "/"
	files := 0
	for _, e := range zr.File {
		if !strings.HasPrefix(e.Name, prefix) || e.FileInfo().IsDir() {
			continue
		}
		rel := strings.TrimPrefix(e.Name, prefix)
		clean := filepath.Clean(filepath.FromSlash(rel))
		if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			continue
		}
		target := filepath.Join(dst, clean)
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(dst)+string(filepath.Separator)) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return files, err
		}
		src, err := e.Open()
		if err != nil {
			return files, err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			src.Close()
			return files, err
		}
		_, copyErr := io.Copy(out, src)
		src.Close()
		if closeErr := out.Close(); copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			return files, copyErr
		}
		files++
	}
	return files, nil
}

// pbkdf2SHA256 is PBKDF2-HMAC-SHA256 (RFC 8018) in standard-library terms: the
// one dependency the module does not already carry.
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)
	var blockBuf [4]byte
	for block := 1; block <= blocks; block++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(blockBuf[:], uint32(block))
		prf.Write(blockBuf[:])
		u := prf.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for n := 1; n < iter; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for i := range t {
				t[i] ^= u[i]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}
