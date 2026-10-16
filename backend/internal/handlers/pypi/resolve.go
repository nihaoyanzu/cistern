package pypi

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/livehl/mirrorhub/internal/config"
)

var (
	hrefRe       = regexp.MustCompile(`(?i)href=["']([^"']+)["']`)
	nameNorm     = regexp.MustCompile(`[-_.]+`)
	wheelTag     = regexp.MustCompile(`(?i)^(.+)-([0-9][^-]*)-(.+)\.(whl|egg)$`)
	sdistTag     = regexp.MustCompile(`(?i)^(.+)-([0-9].+)\.(tar\.gz|tar\.bz2|zip)$`)
	// 注意：不能写成 (单字符|多字符)，Go RE2 会优先匹配左侧单字符
	reqNameRe = regexp.MustCompile(`(?i)^([A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?)`)
	constraintRe = regexp.MustCompile(`(?i)^(===|==|!=|~=|>=|<=|>|<)\s*([^\s,;]+)`)
	verSplitRe   = regexp.MustCompile(`\s*,\s*`)
)

// Requirement 解析后的包需求（PEP 508 子集，不含 marker）
type Requirement struct {
	Name        string
	Constraints []Constraint
	Raw         string
}

// Constraint 单条版本约束
type Constraint struct {
	Op      string
	Version string
}

// ParseRequirement 支持常见 pip 规格：
//
//	requests
//	requests==2.31.0 / requests===2.31.0
//	requests>=2.0 / requests~=2.31.0
//	requests>=2.0,<3
//	requests[security]>=2.0  （extras 忽略）
func ParseRequirement(raw string) (*Requirement, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("空的包规格")
	}
	if i := strings.IndexByte(raw, ';'); i >= 0 {
		raw = strings.TrimSpace(raw[:i])
	}
	m := reqNameRe.FindStringSubmatch(raw)
	if m == nil {
		return nil, fmt.Errorf("无效包名: %q", raw)
	}
	name := m[1]
	rest := strings.TrimSpace(raw[len(m[0]):])
	if strings.HasPrefix(rest, "[") {
		end := strings.IndexByte(rest, ']')
		if end < 0 {
			return nil, fmt.Errorf("extras 未闭合: %q", raw)
		}
		rest = strings.TrimSpace(rest[end+1:])
	}
	req := &Requirement{Name: name, Raw: raw}
	if rest == "" {
		return req, nil
	}
	for _, p := range verSplitRe.Split(rest, -1) {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		cm := constraintRe.FindStringSubmatch(p)
		if cm == nil {
			return nil, fmt.Errorf("无法解析版本约束 %q，示例: requests>=2.0,<3 或 requests==2.31.0", p)
		}
		req.Constraints = append(req.Constraints, Constraint{
			Op:      cm[1],
			Version: strings.TrimSpace(cm[2]),
		})
	}
	if len(req.Constraints) == 0 {
		return nil, fmt.Errorf("无效规格 %q", raw)
	}
	return req, nil
}

// NormalizeName PEP 503 包名规范化
func NormalizeName(name string) string {
	return nameNorm.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
}

// SimpleIndexURL 拼出 simple 索引地址
func SimpleIndexURL(upstream, name string) string {
	base := strings.TrimRight(strings.TrimSpace(upstream), "/")
	return base + "/simple/" + NormalizeName(name) + "/"
}

// ArtifactRef 索引中的发行文件链接（含可选摘要）
type ArtifactRef struct {
	URL    string
	SHA256 string
	Yanked bool
}

// ParseLinkDigest 从 URL fragment 解析 digests，返回 cleanURL。
// 例：.../pkg.whl#sha256=abcdef
func ParseLinkDigest(raw string) (algo, hexDigest, cleanURL string) {
	cleanURL = raw
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		cleanURL = raw[:i]
		frag := raw[i+1:]
		for _, part := range strings.Split(frag, ",") {
			part = strings.TrimSpace(part)
			if j := strings.IndexByte(part, '='); j > 0 {
				algo = strings.ToLower(strings.TrimSpace(part[:j]))
				hexDigest = strings.ToLower(strings.TrimSpace(part[j+1:]))
				if algo == "sha256" && len(hexDigest) == 64 {
					return algo, hexDigest, cleanURL
				}
			}
		}
		algo, hexDigest = "", ""
	}
	return "", "", cleanURL
}

// ExtractArtifactRefs 提取发行文件链接并保留 sha256 / yanked
func ExtractArtifactRefs(body []byte, pageURL string) []ArtifactRef {
	base, err := url.Parse(pageURL)
	if err != nil {
		base = nil
	}
	var out []ArtifactRef
	// 匹配 <a ... href="..." ...> 粗粒度取整标签以便读 data-yanked
	re := regexp.MustCompile(`(?is)<a\s+([^>]*?)>`)
	for _, m := range re.FindAllSubmatch(body, -1) {
		attrs := string(m[1])
		hm := hrefRe.FindStringSubmatch(attrs)
		if len(hm) < 2 {
			continue
		}
		raw := hm[1]
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		abs := raw
		if base != nil {
			if u, err := base.Parse(raw); err == nil {
				abs = u.String()
			}
		}
		_, sha, clean := ParseLinkDigest(abs)
		yanked := strings.Contains(strings.ToLower(attrs), "data-yanked")
		out = append(out, ArtifactRef{URL: clean, SHA256: sha, Yanked: yanked})
	}
	return out
}

// ExtractPackageNames 从根 simple 索引（包名列表页）解析包名
func ExtractPackageNames(body []byte) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, m := range hrefRe.FindAllSubmatch(body, -1) {
		raw := strings.TrimSpace(string(m[1]))
		if raw == "" || strings.HasPrefix(raw, "#") || strings.HasPrefix(raw, "?") {
			continue
		}
		if i := strings.IndexByte(raw, '#'); i >= 0 {
			raw = raw[:i]
		}
		raw = strings.Trim(raw, "/")
		if raw == "" || strings.Contains(raw, "/") {
			// 根索引项形如 "torch/"，去掉斜杠后不应再含路径
			base := path.Base(raw)
			if base == "" || base == "." || base == ".." {
				continue
			}
			raw = base
		}
		name := NormalizeName(raw)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// SelectFiles 取满足约束的最高版本的全部文件（贴近 pip 选版）
func SelectFiles(fileURLs []string, req *Requirement) (version string, urls []string, err error) {
	if req == nil {
		return "", nil, fmt.Errorf("空需求")
	}
	byVer := map[string][]string{}
	var versions []Version
	seenVer := map[string]struct{}{}
	for _, u := range fileURLs {
		vs := fileVersion(filenameFromURL(u))
		if vs == "" {
			continue
		}
		v, err := ParseVersion(vs)
		if err != nil {
			continue
		}
		byVer[vs] = append(byVer[vs], u)
		if _, ok := seenVer[vs]; !ok {
			seenVer[vs] = struct{}{}
			versions = append(versions, v)
		}
	}
	if len(versions) == 0 {
		return "", nil, fmt.Errorf("索引中没有可解析的发行文件")
	}

	var matched []Version
	for _, v := range versions {
		ok, err := Satisfy(v, req.Constraints)
		if err != nil {
			return "", nil, err
		}
		if ok {
			matched = append(matched, v)
		}
	}
	if len(matched) == 0 {
		return "", nil, fmt.Errorf("没有版本满足 %s", req.Raw)
	}

	best := matched[0]
	for _, v := range matched[1:] {
		if Compare(v, best) > 0 {
			best = v
		}
	}
	urls = byVer[best.Original]
	if len(urls) == 0 {
		return "", nil, fmt.Errorf("版本 %s 无对应文件", best.Original)
	}
	return best.Original, urls, nil
}

// FilterArtifacts 按预取策略裁剪发行文件（仅预取用；交互不过滤）。
// 平台过滤始终生效（不受 mode 影响）；mode 控制品类过滤：
//   - all：保留 sdist + none-any + 目标平台 wheel（不限 cp 标签）
//   - portable（默认）：保留 sdist + none-any + 「目标平台匹配 + cp 标签命中」的平台 wheel
func FilterArtifacts(urls []string, mode string, extraTags []string, targetPlatforms []string) []string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	tags := make([]string, 0, len(extraTags))
	for _, t := range extraTags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t != "" {
			tags = append(tags, t)
		}
	}
	plats := normalizePlatformList(targetPlatforms)
	needTag := mode != "all" // portable 模式要求 cp 标签匹配
	out := make([]string, 0, len(urls))
	seen := map[string]struct{}{}
	for _, u := range urls {
		fn := strings.ToLower(filenameFromURL(u))
		keep := false
		if strings.HasSuffix(fn, ".tar.gz") || strings.HasSuffix(fn, ".tar.bz2") || strings.HasSuffix(fn, ".zip") {
			keep = true // sdist 始终保留
		} else if strings.HasSuffix(fn, ".whl") {
			if wheelIsNoneAny(fn) {
				keep = true // none-any 始终保留
			} else if wheelMatchesAnyPlatform(fn, plats) {
				// 平台 wheel：all 模式直接保留；portable 模式还需匹配 cp 标签
				keep = !needTag || wheelMatchesAnyTag(fn, tags)
			}
		}
		if !keep {
			continue
		}
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	return out
}

func wheelIsNoneAny(fn string) bool {
	return strings.Contains(fn, "-none-any.whl") || strings.Contains(fn, ".py2.py3-none-any")
}

func normalizePlatformList(in []string) []string {
	return config.NormalizeTargetPlatforms(in)
}

func wheelMatchesAnyPlatform(fn string, plats []string) bool {
	for _, p := range plats {
		if wheelMatchesPlatform(fn, p) {
			return true
		}
	}
	return false
}

func wheelIsARM(fn string) bool {
	return strings.Contains(fn, "aarch64") ||
		strings.Contains(fn, "arm64") ||
		strings.Contains(fn, "win_arm") ||
		strings.Contains(fn, "_armv7") ||
		strings.Contains(fn, "_armv6")
}

func wheelIsLinuxFamily(fn string) bool {
	if strings.Contains(fn, "win") || strings.Contains(fn, "macosx") || strings.Contains(fn, "darwin") {
		return false
	}
	return strings.Contains(fn, "manylinux") ||
		strings.Contains(fn, "musllinux") ||
		strings.Contains(fn, "linux_")
}

// wheelMatchesPlatform 按组织目标平台过滤；linux / linux-arm / win32 / win-arm / darwin / darwin-arm。
func wheelMatchesPlatform(fn, plat string) bool {
	arm := wheelIsARM(fn)
	switch plat {
	case "win32":
		if !strings.Contains(fn, "win") {
			return false
		}
		return !strings.Contains(fn, "win_arm")
	case "win-arm":
		return strings.Contains(fn, "win_arm")
	case "darwin":
		if !(strings.Contains(fn, "macosx") || strings.Contains(fn, "darwin")) {
			return false
		}
		if strings.Contains(fn, "universal2") {
			return true
		}
		return !arm
	case "darwin-arm":
		if !(strings.Contains(fn, "macosx") || strings.Contains(fn, "darwin")) {
			return false
		}
		return arm || strings.Contains(fn, "universal2")
	case "linux-arm":
		return wheelIsLinuxFamily(fn) && arm
	default: // linux (x86_64 / i686 等)
		return wheelIsLinuxFamily(fn) && !arm
	}
}

// wheelMatchesAnyTag 要求 tag 紧跟在 '-' 后（wheel 标签段），避免无界 Contains。
// 无 tags 时平台 wheel 一律不收（仅靠 sdist/none-any）。
func wheelMatchesAnyTag(fn string, tags []string) bool {
	if len(tags) == 0 {
		return false
	}
	for _, t := range tags {
		if t == "" {
			continue
		}
		if strings.Contains(fn, "-"+t) {
			return true
		}
	}
	return false
}

// IsPlausiblePackageName 拒绝平台词、纯版本串等伪包名，避免预取误入队。
func IsPlausiblePackageName(name string) bool {
	raw := strings.TrimSpace(name)
	if raw == "" {
		return false
	}
	// 规范化前先拦纯版本（否则 3.11 → 3-11）
	if pureVersionName.MatchString(strings.ToLower(raw)) {
		return false
	}
	n := NormalizeName(raw)
	if n == "" {
		return false
	}
	switch n {
	case "win32", "win", "windows", "linux", "darwin", "macos", "mac", "osx",
		"amd64", "x86-64", "aarch64", "arm64", "armv7l", "i686",
		"ppc64le", "s390x", "riscv64", "musl", "gnu":
		return false
	}
	if pureVersionName.MatchString(n) {
		return false
	}
	// 常见包名以字母开头
	if n[0] < 'a' || n[0] > 'z' {
		return false
	}
	return true
}

var pureVersionName = regexp.MustCompile(`^\d+([.\-]\d+)*$`)

func filenameFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return path.Base(raw)
	}
	return path.Base(u.Path)
}

func fileVersion(filename string) string {
	info := ParseArtifactFilename(filename)
	return info.Version
}

// Version PEP 440 精简结构
type Version struct {
	Original string
	Epoch    int
	Release  []int
	PreKind  int // 0=无 1=a 2=b 3=rc
	PreNum   int
	Post     int // -1=无
	Dev      int // -1=无
}

func ParseVersion(s string) (Version, error) {
	orig := strings.TrimSpace(s)
	s = strings.ToLower(orig)
	if s == "" {
		return Version{}, fmt.Errorf("空版本")
	}
	v := Version{Original: orig, Post: -1, Dev: -1}
	if i := strings.IndexByte(s, '!'); i >= 0 {
		ep, err := strconv.Atoi(s[:i])
		if err != nil {
			return Version{}, fmt.Errorf("无效 epoch: %s", s)
		}
		v.Epoch = ep
		s = s[i+1:]
	}
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}

	i := 0
	for i < len(s) && ((s[i] >= '0' && s[i] <= '9') || s[i] == '.') {
		i++
	}
	rel := strings.Trim(s[:i], ".")
	if rel == "" {
		return Version{}, fmt.Errorf("无效版本: %s", orig)
	}
	for _, p := range strings.Split(rel, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return Version{}, fmt.Errorf("无效版本段: %s", orig)
		}
		v.Release = append(v.Release, n)
	}
	rest := s[i:]
	rest = strings.ReplaceAll(rest, "-", ".")
	rest = strings.ReplaceAll(rest, "_", ".")

	for rest != "" {
		if strings.HasPrefix(rest, ".") {
			rest = rest[1:]
			continue
		}
		switch {
		case consumeLabel(&rest, &v.PreKind, &v.PreNum, 1, "alpha", "a"):
		case consumeLabel(&rest, &v.PreKind, &v.PreNum, 2, "beta", "b"):
		case consumeLabel(&rest, &v.PreKind, &v.PreNum, 3, "preview", "pre", "rc", "c"):
		case consumePostOrDev(&rest, &v.Post, "post", "rev", "r"):
		case consumePostOrDev(&rest, &v.Dev, "dev"):
		default:
			return v, nil
		}
	}
	return v, nil
}

func consumeLabel(rest *string, kind, num *int, k int, labels ...string) bool {
	s := *rest
	for _, lb := range labels {
		if !strings.HasPrefix(s, lb) {
			continue
		}
		s = s[len(lb):]
		n, left := readDigits(s)
		*kind = k
		*num = n
		*rest = left
		return true
	}
	return false
}

func consumePostOrDev(rest *string, dest *int, labels ...string) bool {
	s := *rest
	for _, lb := range labels {
		if !strings.HasPrefix(s, lb) {
			continue
		}
		s = s[len(lb):]
		n, left := readDigits(s)
		*dest = n
		*rest = left
		return true
	}
	return false
}

func readDigits(s string) (int, string) {
	j := 0
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	if j == 0 {
		return 0, s
	}
	n, _ := strconv.Atoi(s[:j])
	return n, s[j:]
}

// Compare a>b => 1; a==b => 0; a<b => -1
func Compare(a, b Version) int {
	if a.Epoch != b.Epoch {
		return cmpInt(a.Epoch, b.Epoch)
	}
	n := len(a.Release)
	if len(b.Release) > n {
		n = len(b.Release)
	}
	for i := 0; i < n; i++ {
		ai, bi := 0, 0
		if i < len(a.Release) {
			ai = a.Release[i]
		}
		if i < len(b.Release) {
			bi = b.Release[i]
		}
		if ai != bi {
			return cmpInt(ai, bi)
		}
	}
	aDevOnly := a.Dev >= 0 && a.PreKind == 0
	bDevOnly := b.Dev >= 0 && b.PreKind == 0
	if aDevOnly != bDevOnly {
		if aDevOnly {
			return -1
		}
		return 1
	}
	if a.PreKind == 0 && b.PreKind != 0 {
		return 1
	}
	if a.PreKind != 0 && b.PreKind == 0 {
		return -1
	}
	if a.PreKind != b.PreKind {
		return cmpInt(a.PreKind, b.PreKind)
	}
	if a.PreKind != 0 && a.PreNum != b.PreNum {
		return cmpInt(a.PreNum, b.PreNum)
	}
	if a.Post != b.Post {
		return cmpInt(a.Post, b.Post)
	}
	if a.Dev != b.Dev {
		if a.Dev < 0 && b.Dev >= 0 {
			return 1
		}
		if a.Dev >= 0 && b.Dev < 0 {
			return -1
		}
		return cmpInt(a.Dev, b.Dev)
	}
	return 0
}

func cmpInt(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

// Satisfy 检查版本是否满足全部约束（AND）
func Satisfy(v Version, cs []Constraint) (bool, error) {
	if len(cs) == 0 {
		return true, nil
	}
	for _, c := range cs {
		ok, err := checkOne(v, c)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func checkOne(v Version, c Constraint) (bool, error) {
	tv, err := ParseVersion(c.Version)
	if err != nil {
		return false, fmt.Errorf("约束版本无效 %s: %w", c.Version, err)
	}
	cmp := Compare(v, tv)
	switch c.Op {
	case "==":
		return equalPublic(v, tv), nil
	case "===":
		return strings.EqualFold(v.Original, tv.Original), nil
	case "!=":
		return !equalPublic(v, tv), nil
	case ">":
		return cmp > 0, nil
	case ">=":
		return cmp >= 0, nil
	case "<":
		return cmp < 0, nil
	case "<=":
		return cmp <= 0, nil
	case "~=":
		if cmp < 0 {
			return false, nil
		}
		return compatibleRelease(v, tv), nil
	default:
		return false, fmt.Errorf("未知运算符 %s", c.Op)
	}
}

func equalPublic(a, b Version) bool {
	if a.Epoch != b.Epoch || a.PreKind != b.PreKind || a.PreNum != b.PreNum || a.Post != b.Post || a.Dev != b.Dev {
		return false
	}
	n := len(a.Release)
	if len(b.Release) > n {
		n = len(b.Release)
	}
	for i := 0; i < n; i++ {
		ai, bi := 0, 0
		if i < len(a.Release) {
			ai = a.Release[i]
		}
		if i < len(b.Release) {
			bi = b.Release[i]
		}
		if ai != bi {
			return false
		}
	}
	return true
}

func compatibleRelease(v, base Version) bool {
	if len(base.Release) == 0 {
		return equalPublic(v, base)
	}
	prefix := base.Release[:len(base.Release)-1]
	if len(v.Release) < len(prefix) {
		return false
	}
	for i := range prefix {
		if v.Release[i] != prefix[i] {
			return false
		}
	}
	return true
}
