package npm

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// PackageRef package.json / 预取规格中的包名与版本约束。
type PackageRef struct {
	Name    string
	Version string // 精确版本、dist-tag 或简易 semver 范围
}

// Spec 返回 name@version（scoped 亦然）。
func (r PackageRef) Spec() string {
	return r.Name + "@" + r.Version
}

// LookLikePackageJSON 粗判是否为 package.json（排除 package-lock）。
func LookLikePackageJSON(text string) bool {
	s := strings.TrimSpace(text)
	if s == "" || !strings.HasPrefix(s, "{") {
		return false
	}
	if LookLikeLockfile(s) {
		return false
	}
	low := strings.ToLower(s[:min(len(s), 1200)])
	if strings.Contains(low, `"lockfileversion"`) {
		return false
	}
	// 典型 package.json：有 name/dependencies，且无 node_modules/ 包图
	hasDeps := strings.Contains(low, `"dependencies"`) ||
		strings.Contains(low, `"devdependencies"`) ||
		strings.Contains(low, `"optionaldependencies"`)
	if !hasDeps {
		return false
	}
	if strings.Contains(low, `"node_modules/"`) {
		return false
	}
	return true
}

// ParsePackageJSON 从 package.json 提取顶层依赖规格（dependencies / dev / optional）。
// 跳过 workspace/file/link/git/portal 等非 registry 来源；范围版本原样保留供预取解析。
func ParsePackageJSON(text string) (refs []PackageRef, skipped []string) {
	s := strings.TrimSpace(text)
	if s == "" {
		return nil, nil
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(s), &root); err != nil {
		return nil, []string{"package.json 解析失败"}
	}
	seen := map[string]struct{}{}
	addMap := func(raw any) {
		m, ok := raw.(map[string]any)
		if !ok {
			return
		}
		for name, verRaw := range m {
			name = strings.TrimSpace(name)
			ver, _ := verRaw.(string)
			ver = strings.TrimSpace(ver)
			if name == "" || ver == "" {
				continue
			}
			if !isRegistryVersionSpec(ver) {
				skipped = append(skipped, name+"@"+ver)
				continue
			}
			key := name + "@" + ver
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			refs = append(refs, PackageRef{Name: name, Version: ver})
		}
	}
	addMap(root["dependencies"])
	addMap(root["devDependencies"])
	addMap(root["optionalDependencies"])
	return refs, skipped
}

// ParsePackageSpec 解析 name@version / @scope/name@version。
func ParsePackageSpec(raw string) (PackageRef, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "#") || strings.HasPrefix(raw, "//") {
		return PackageRef{}, false
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return PackageRef{}, false
	}
	name, ver, ok := splitNPMAt(raw)
	if !ok || name == "" || ver == "" {
		return PackageRef{}, false
	}
	if !looksLikePackageName(name) || !isRegistryVersionSpec(ver) {
		return PackageRef{}, false
	}
	return PackageRef{Name: name, Version: ver}, true
}

// PackumentURL 构造 registry packument URL。
func PackumentURL(upstream, name string) string {
	base := strings.TrimRight(strings.TrimSpace(upstream), "/")
	name = strings.TrimSpace(name)
	if base == "" || name == "" {
		return ""
	}
	// scoped：/@scope%2Fpkg；普通包：/name
	escaped := strings.ReplaceAll(name, "/", "%2F")
	return base + "/" + escaped
}

// ResolvePackumentTarball 根据 packument JSON 与版本规格选出 tarball，并返回该版本的 dependencies。
func ResolvePackumentTarball(packument []byte, spec string) (tarball string, version string, deps map[string]string, err error) {
	var root map[string]any
	if err := json.Unmarshal(packument, &root); err != nil {
		return "", "", nil, fmt.Errorf("packument 解析失败")
	}
	versions, _ := root["versions"].(map[string]any)
	if len(versions) == 0 {
		return "", "", nil, fmt.Errorf("packument 无 versions")
	}
	tags, _ := root["dist-tags"].(map[string]any)
	ver, err := pickNPMVersion(spec, versions, tags)
	if err != nil {
		return "", "", nil, err
	}
	meta, ok := versions[ver].(map[string]any)
	if !ok {
		return "", "", nil, fmt.Errorf("无版本元数据: %s", ver)
	}
	dist, _ := meta["dist"].(map[string]any)
	tb, _ := dist["tarball"].(string)
	tb = strings.TrimSpace(tb)
	if tb == "" {
		return "", "", nil, fmt.Errorf("无 dist.tarball: %s", ver)
	}
	outDeps := map[string]string{}
	if m, ok := meta["dependencies"].(map[string]any); ok {
		for k, v := range m {
			if vs, ok := v.(string); ok {
				outDeps[k] = vs
			}
		}
	}
	if m, ok := meta["optionalDependencies"].(map[string]any); ok {
		for k, v := range m {
			if vs, ok := v.(string); ok {
				outDeps[k] = vs
			}
		}
	}
	return tb, ver, outDeps, nil
}

// IsRegistryVersionSpec 是否为可走 registry 解析的版本约束（非 file/git/workspace 等）。
func IsRegistryVersionSpec(ver string) bool {
	return isRegistryVersionSpec(ver)
}

func isRegistryVersionSpec(ver string) bool {
	ver = strings.TrimSpace(ver)
	if ver == "" {
		return false
	}
	low := strings.ToLower(ver)
	for _, p := range []string{
		"workspace:", "file:", "link:", "portal:", "git+", "git:",
		"github:", "gist:", "bitbucket:", "gitlab:", "http://", "https://",
	} {
		if strings.HasPrefix(low, p) {
			return false
		}
	}
	// npm:alias@range → 取右侧范围仍可能可解析，整段跳过以免歧义
	if strings.HasPrefix(low, "npm:") {
		return false
	}
	return true
}

func looksLikePackageName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, " \t") {
		return false
	}
	if strings.HasPrefix(name, "@") {
		parts := strings.Split(name, "/")
		return len(parts) == 2 && parts[0] != "@" && parts[1] != ""
	}
	return !strings.Contains(name, "/")
}

func splitNPMAt(raw string) (name, version string, ok bool) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "@") {
		// @scope/name@version：版本分隔 @ 必须在 / 之后
		slash := strings.Index(raw, "/")
		if slash <= 1 {
			return "", "", false
		}
		at := strings.LastIndex(raw, "@")
		if at <= slash || at == len(raw)-1 {
			return "", "", false
		}
		return raw[:at], raw[at+1:], true
	}
	i := strings.LastIndex(raw, "@")
	if i <= 0 || i == len(raw)-1 {
		return "", "", false
	}
	return raw[:i], raw[i+1:], true
}

func pickNPMVersion(spec string, versions map[string]any, tags map[string]any) (string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		spec = "latest"
	}
	// dist-tag
	if tags != nil {
		if v, ok := tags[spec].(string); ok && strings.TrimSpace(v) != "" {
			if _, exists := versions[v]; exists {
				return v, nil
			}
		}
	}
	if _, ok := versions[spec]; ok {
		return spec, nil
	}
	low := strings.ToLower(spec)
	if low == "*" || low == "x" || low == "latest" {
		if tags != nil {
			if v, ok := tags["latest"].(string); ok && v != "" {
				if _, exists := versions[v]; exists {
					return v, nil
				}
			}
		}
	}

	keys := make([]string, 0, len(versions))
	for k := range versions {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return npmVersionLess(keys[j], keys[i]) }) // 高→低

	for _, v := range keys {
		if npmVersionSatisfies(v, spec) {
			return v, nil
		}
	}
	return "", fmt.Errorf("无法满足版本: %s", spec)
}

type npmVer struct {
	Major, Minor, Patch int
	Pre                 string
	Valid               bool
}

func parseNPMVer(s string) npmVer {
	s = strings.TrimSpace(strings.TrimPrefix(s, "v"))
	if s == "" {
		return npmVer{}
	}
	pre := ""
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		if s[i] == '-' {
			pre = s[i+1:]
			if j := strings.IndexByte(pre, '+'); j >= 0 {
				pre = pre[:j]
			}
		}
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 1 {
		return npmVer{}
	}
	num := func(p string) (int, bool) {
		n, err := strconv.Atoi(p)
		return n, err == nil && n >= 0
	}
	maj, ok1 := num(parts[0])
	min, ok2 := 0, true
	pat, ok3 := 0, true
	if len(parts) > 1 {
		min, ok2 = num(parts[1])
	}
	if len(parts) > 2 {
		pat, ok3 = num(parts[2])
	}
	if !ok1 || !ok2 || !ok3 {
		return npmVer{}
	}
	return npmVer{Major: maj, Minor: min, Patch: pat, Pre: pre, Valid: true}
}

func npmVersionLess(a, b string) bool {
	va, vb := parseNPMVer(a), parseNPMVer(b)
	if !va.Valid || !vb.Valid {
		return a < b
	}
	if va.Major != vb.Major {
		return va.Major < vb.Major
	}
	if va.Minor != vb.Minor {
		return va.Minor < vb.Minor
	}
	if va.Patch != vb.Patch {
		return va.Patch < vb.Patch
	}
	// 无 pre > 有 pre；同有 pre 则字典序
	if va.Pre == vb.Pre {
		return false
	}
	if va.Pre == "" {
		return false
	}
	if vb.Pre == "" {
		return true
	}
	return va.Pre < vb.Pre
}

func npmVersionSatisfies(version, spec string) bool {
	spec = strings.TrimSpace(spec)
	v := parseNPMVer(version)
	if !v.Valid {
		return false
	}
	// 跳过预发布，除非约束本身带 pre
	if v.Pre != "" && !strings.Contains(spec, "-") {
		return false
	}

	orParts := strings.Split(spec, "||")
	for _, part := range orParts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if npmVersionSatisfiesAnd(version, part) {
			return true
		}
	}
	return false
}

func npmVersionSatisfiesAnd(version, spec string) bool {
	spec = strings.TrimSpace(spec)
	v := parseNPMVer(version)
	if !v.Valid {
		return false
	}
	if spec == "*" || spec == "x" || spec == "X" {
		return v.Pre == ""
	}
	// 空格分隔的复合约束：>=1.2.0 <2.0.0
	if strings.ContainsAny(spec, " \t") && (strings.Contains(spec, ">=") || strings.Contains(spec, "<") || strings.Contains(spec, ">")) {
		for _, tok := range strings.Fields(spec) {
			if !npmVersionSatisfiesOne(version, tok) {
				return false
			}
		}
		return true
	}
	return npmVersionSatisfiesOne(version, spec)
}

func npmVersionSatisfiesOne(version, spec string) bool {
	spec = strings.TrimSpace(spec)
	v := parseNPMVer(version)
	if !v.Valid || spec == "" {
		return false
	}
	switch {
	case strings.HasPrefix(spec, "^"):
		base := parseNPMVer(strings.TrimSpace(spec[1:]))
		if !base.Valid {
			return false
		}
		if v.Major != base.Major {
			return false
		}
		return !npmVersionLess(version, strings.TrimSpace(spec[1:]))
	case strings.HasPrefix(spec, "~"):
		base := parseNPMVer(strings.TrimSpace(spec[1:]))
		if !base.Valid {
			return false
		}
		if v.Major != base.Major || v.Minor != base.Minor {
			return false
		}
		return !npmVersionLess(version, strings.TrimSpace(spec[1:]))
	case strings.HasPrefix(spec, ">="):
		base := strings.TrimSpace(spec[2:])
		if !parseNPMVer(base).Valid {
			return false
		}
		return !npmVersionLess(version, base)
	case strings.HasPrefix(spec, "<="):
		base := strings.TrimSpace(spec[2:])
		if !parseNPMVer(base).Valid {
			return false
		}
		return !npmVersionLess(base, version)
	case strings.HasPrefix(spec, ">"):
		base := strings.TrimSpace(spec[1:])
		if !parseNPMVer(base).Valid {
			return false
		}
		return npmVersionLess(base, version)
	case strings.HasPrefix(spec, "<"):
		base := strings.TrimSpace(spec[1:])
		if !parseNPMVer(base).Valid {
			return false
		}
		return npmVersionLess(version, base)
	case strings.HasPrefix(spec, "="):
		return version == strings.TrimSpace(spec[1:])
	default:
		return version == spec
	}
}
