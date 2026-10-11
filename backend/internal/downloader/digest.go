package downloader

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// digests 记录索引里见到的 cleanURL / basename -> 摘要
// 值为 64 位 sha256 hex，或 SRI（sha256-… / sha512-…）。
var digests sync.Map

var (
	digestPersistPath string
	digestPersistMu   sync.Mutex
	digestDirty       bool
	digestSaveSched   bool
)

// InitDigestPersist 从 cache 目录加载并定期落盘 digests。
func InitDigestPersist(cacheDir string) {
	digestPersistPath = filepath.Join(cacheDir, "digests.json")
	loadDigestsFile()
}

func loadDigestsFile() {
	if digestPersistPath == "" {
		return
	}
	b, err := os.ReadFile(digestPersistPath)
	if err != nil {
		return
	}
	var m map[string]string
	if json.Unmarshal(b, &m) != nil {
		return
	}
	for k, v := range m {
		if k == "" || !validStoredDigest(v) {
			continue
		}
		digests.Store(k, normalizeStoredDigest(v))
	}
}

func validStoredDigest(v string) bool {
	v = strings.TrimSpace(v)
	if len(v) == 64 && isHex64(v) {
		return true
	}
	return strings.HasPrefix(v, "sha256-") || strings.HasPrefix(v, "sha512-")
}

func normalizeStoredDigest(v string) string {
	v = strings.TrimSpace(v)
	if len(v) == 64 && isHex64(v) {
		return strings.ToLower(v)
	}
	return v
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

func scheduleDigestSave() {
	digestPersistMu.Lock()
	digestDirty = true
	if digestSaveSched || digestPersistPath == "" {
		digestPersistMu.Unlock()
		return
	}
	digestSaveSched = true
	digestPersistMu.Unlock()
	go func() {
		time.Sleep(2 * time.Second)
		flushDigests()
	}()
}

func flushDigests() {
	digestPersistMu.Lock()
	defer digestPersistMu.Unlock()
	digestSaveSched = false
	if !digestDirty || digestPersistPath == "" {
		return
	}
	out := map[string]string{}
	digests.Range(func(k, v any) bool {
		ks, _ := k.(string)
		vs, _ := v.(string)
		if ks != "" && vs != "" {
			out[ks] = vs
		}
		return true
	})
	b, err := json.Marshal(out)
	if err != nil {
		return
	}
	tmp := digestPersistPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, digestPersistPath)
	digestDirty = false
}

func RememberDigest(rawURL, sha256hex string) {
	sha256hex = strings.ToLower(strings.TrimSpace(sha256hex))
	if sha256hex == "" || len(sha256hex) != 64 || !isHex64(sha256hex) {
		return
	}
	storeDigest(rawURL, sha256hex)
}

// RememberIntegrity 记录 npm SRI（sha512-… / sha256-…）或裸 sha256 hex。
func RememberIntegrity(rawURL, integrity string) {
	integrity = strings.TrimSpace(integrity)
	if integrity == "" {
		return
	}
	if len(integrity) == 64 && isHex64(integrity) {
		RememberDigest(rawURL, integrity)
		return
	}
	if strings.HasPrefix(integrity, "sha256-") || strings.HasPrefix(integrity, "sha512-") {
		storeDigest(rawURL, integrity)
	}
}

func storeDigest(rawURL, value string) {
	clean := stripFragment(rawURL)
	if clean == "" || value == "" {
		return
	}
	digests.Store(clean, value)
	if u, err := url.Parse(clean); err == nil {
		base := path.Base(u.Path)
		if base != "" && base != "." && base != "/" {
			digests.Store(base, value)
		}
		u.RawQuery = ""
		u.Fragment = ""
		digests.Store(u.String(), value)
	} else if base := path.Base(clean); base != "" {
		digests.Store(base, value)
	}
	scheduleDigestSave()
}

func LookupDigest(rawURL string) string {
	clean := stripFragment(rawURL)
	if v, ok := digests.Load(clean); ok {
		return v.(string)
	}
	if u, err := url.Parse(clean); err == nil {
		u.RawQuery = ""
		u.Fragment = ""
		if v, ok := digests.Load(u.String()); ok {
			return v.(string)
		}
		base := path.Base(u.Path)
		if v, ok := digests.Load(base); ok {
			return v.(string)
		}
	}
	if base := path.Base(clean); base != "" {
		if v, ok := digests.Load(base); ok {
			return v.(string)
		}
	}
	return ""
}

func stripFragment(raw string) string {
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		return raw[:i]
	}
	return raw
}

func (opt *Options) resolveExpectedDigest() {
	if opt.ExpectedSHA256 != "" {
		return
	}
	if d := LookupDigest(opt.metaSource()); d != "" {
		opt.ExpectedSHA256 = d
		return
	}
	if d := LookupDigest(opt.URL); d != "" {
		opt.ExpectedSHA256 = d
	}
}

func verifyAndRemember(path string, opt Options) error {
	exp := strings.TrimSpace(opt.ExpectedSHA256)
	if exp == "" {
		return nil
	}
	switch {
	case strings.HasPrefix(exp, "sha512-"):
		sum, err := fileDigestBase64(path, sha512.New())
		if err != nil {
			return err
		}
		want := strings.TrimPrefix(exp, "sha512-")
		if sum != want {
			_ = os.Remove(path)
			return fmt.Errorf("sha512 mismatch: want %s got %s", want, sum)
		}
		RememberIntegrity(opt.metaSource(), exp)
		RememberIntegrity(opt.URL, exp)
		return nil
	case strings.HasPrefix(exp, "sha256-"):
		sum, err := fileDigestBase64(path, sha256.New())
		if err != nil {
			return err
		}
		want := strings.TrimPrefix(exp, "sha256-")
		if sum != want {
			_ = os.Remove(path)
			return fmt.Errorf("sha256 mismatch: want %s got %s", want, sum)
		}
		RememberIntegrity(opt.metaSource(), exp)
		RememberIntegrity(opt.URL, exp)
		return nil
	default:
		sum, err := fileSHA256(path)
		if err != nil {
			return err
		}
		if !strings.EqualFold(sum, exp) {
			_ = os.Remove(path)
			return fmt.Errorf("sha256 mismatch: want %s got %s", exp, sum)
		}
		RememberDigest(opt.metaSource(), sum)
		RememberDigest(opt.URL, sum)
		return nil
	}
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileDigestBase64(path string, h hash.Hash) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}
