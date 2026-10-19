package pypi

import (
	"bufio"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/nihaoyanzu/cistern/internal/config"
)

var requiresDistRe = regexp.MustCompile(`(?i)^Requires-Dist:\s*(.+)$`)

// ParseRequiresDist 从 PEP 643/METADATA 文本提取 Requires-Dist 原始规格（含 marker）。
func ParseRequiresDist(meta []byte) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(string(meta)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		m := requiresDistRe.FindStringSubmatch(line)
		if len(m) < 2 {
			continue
		}
		spec := strings.TrimSpace(m[1])
		if spec == "" {
			continue
		}
		out = append(out, spec)
	}
	return out
}

// TargetEnv 组织目标环境（非网关本机），用于评估依赖 marker。
// Python / Platforms 均为列表；任一组合命中即纳入。
type TargetEnv struct {
	Python    []string // 如 ["3.10", "3.12"]
	Platforms []string // linux / linux-arm / win32 / win-arm / darwin / darwin-arm
	Platform  string   // 兼容旧单值
}

// SplitReqMarker 拆分 "name (>=1); marker" 为规格与 marker。
func SplitReqMarker(raw string) (reqPart, marker string) {
	raw = strings.TrimSpace(raw)
	if i := strings.IndexByte(raw, ';'); i >= 0 {
		return strings.TrimSpace(raw[:i]), strings.TrimSpace(raw[i+1:])
	}
	return raw, ""
}

// EvalMarker 评估精简 PEP 508 marker；不支持的复杂表达式偏保守返回 true（尽量预热）。
// 对 python_version：仅按配置的精确版本列表评估（任一命中即纳入）。
// 对平台：任一目标平台命中即纳入。
func EvalMarker(marker string, env TargetEnv) bool {
	marker = strings.TrimSpace(marker)
	if marker == "" {
		return true
	}
	low := strings.ToLower(marker)
	// 跳过 extras
	if strings.Contains(low, "extra ") || strings.Contains(low, "extra==") || strings.Contains(low, "extra ==") {
		if !strings.Contains(low, "extra == ''") && !strings.Contains(low, `extra == ""`) {
			return false
		}
	}
	plats := append([]string{}, env.Platforms...)
	if env.Platform != "" {
		plats = append(plats, env.Platform)
	}
	plats = config.NormalizeTargetPlatforms(plats)
	versions := env.Python
	if len(versions) == 0 {
		versions = []string{"3.10", "3.11", "3.12"}
	}
	for _, py := range versions {
		for _, plat := range plats {
			if evalMarkerForPython(marker, py, plat) {
				return true
			}
		}
	}
	return false
}

func evalMarkerForPython(marker, py, plat string) bool {
	parts := splitMarkerAnd(marker)
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !evalMarkerAtom(p, py, plat) {
			return false
		}
	}
	return true
}

// pythonVersionsFromFloor 返回 [floor, 3.13] 的 minor 序列，如 3.9 → 3.9…3.13。
func pythonVersionsFromFloor(floor string) []string {
	maj, min, ok := parsePyMajorMinor(normalizePy(floor))
	if !ok {
		return []string{normalizePy(floor)}
	}
	const maxMinor = 13
	if maj != 3 {
		return []string{fmt.Sprintf("%d.%d", maj, min)}
	}
	if min > maxMinor {
		min = maxMinor
	}
	out := make([]string, 0, maxMinor-min+1)
	for m := min; m <= maxMinor; m++ {
		out = append(out, fmt.Sprintf("3.%d", m))
	}
	return out
}

// CPTagsFromFloor 生成 cp39、cp310… 等标签（≥ 最低 Python）。
func CPTagsFromFloor(floor string) []string {
	versions := pythonVersionsFromFloor(floor)
	out := make([]string, 0, len(versions))
	for _, v := range versions {
		maj, min, ok := parsePyMajorMinor(v)
		if !ok {
			continue
		}
		out = append(out, fmt.Sprintf("cp%d%d", maj, min))
	}
	return out
}

// CPTagsFromVersions 为指定的 Python 版本列表生成 cp 标签。
// 如 ["3.10", "3.12"] → ["cp310", "cp312"]。
func CPTagsFromVersions(versions []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, v := range versions {
		maj, min, ok := parsePyMajorMinor(normalizePy(v))
		if !ok {
			continue
		}
		tag := fmt.Sprintf("cp%d%d", maj, min)
		if _, dup := seen[tag]; dup {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	return out
}

func parsePyMajorMinor(s string) (maj, min int, ok bool) {
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	maj, err1 := strconv.Atoi(parts[0])
	min, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return maj, min, true
}

func splitMarkerAnd(marker string) []string {
	// 仅顶层 and；忽略 or 复杂分支（整段当原子）
	low := strings.ToLower(marker)
	if strings.Contains(low, " or ") {
		return []string{marker}
	}
	var parts []string
	start := 0
	for {
		idx := strings.Index(strings.ToLower(marker[start:]), " and ")
		if idx < 0 {
			parts = append(parts, marker[start:])
			break
		}
		parts = append(parts, marker[start:start+idx])
		start = start + idx + 5
	}
	return parts
}

func evalMarkerAtom(atom, py, plat string) bool {
	atom = strings.TrimSpace(atom)
	low := strings.ToLower(atom)

	// python_version 比较
	if strings.Contains(low, "python_version") {
		return evalVersionCmp(atom, "python_version", py)
	}
	if strings.Contains(low, "python_full_version") {
		return evalVersionCmp(atom, "python_full_version", py+".0")
	}
	if strings.Contains(low, "sys_platform") {
		return evalStrCmp(atom, "sys_platform", markerSysPlatform(plat))
	}
	if strings.Contains(low, "platform_system") {
		return evalStrCmp(atom, "platform_system", markerPlatformSystem(plat))
	}
	// 未知原子：保守保留
	return true
}

var cmpRe = regexp.MustCompile(`(?i)(python_version|python_full_version|sys_platform|platform_system)\s*(==|!=|<=|>=|<|>)\s*['"]([^'"]+)['"]`)

func evalVersionCmp(atom, key, have string) bool {
	m := cmpRe.FindStringSubmatch(atom)
	if len(m) < 4 || !strings.EqualFold(m[1], key) {
		return true
	}
	op, want := m[2], m[3]
	return cmpVersionStrings(have, op, want)
}

func evalStrCmp(atom, key, have string) bool {
	m := cmpRe.FindStringSubmatch(atom)
	if len(m) < 4 || !strings.EqualFold(m[1], key) {
		return true
	}
	op, want := m[2], strings.ToLower(m[3])
	have = strings.ToLower(have)
	switch op {
	case "==":
		return have == want
	case "!=":
		return have != want
	default:
		return true
	}
}

func cmpVersionStrings(have, op, want string) bool {
	hv := parseShortVer(have)
	wv := parseShortVer(want)
	c := 0
	n := len(hv)
	if len(wv) > n {
		n = len(wv)
	}
	for i := 0; i < n; i++ {
		a, b := 0, 0
		if i < len(hv) {
			a = hv[i]
		}
		if i < len(wv) {
			b = wv[i]
		}
		if a < b {
			c = -1
			break
		}
		if a > b {
			c = 1
			break
		}
	}
	switch op {
	case "==":
		return c == 0
	case "!=":
		return c != 0
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	case ">=":
		return c >= 0
	default:
		return true
	}
}

func parseShortVer(s string) []int {
	parts := strings.Split(s, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, _ := strconv.Atoi(p)
		out = append(out, n)
	}
	return out
}

func normalizePy(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "3.9"
	}
	return s
}

func normalizePlat(s string) string {
	if p := config.NormalizeTargetPlatform(s); p != "" {
		return p
	}
	return "linux"
}

// markerSysPlatform 将组织目标平台映射为 PEP 508 sys_platform。
func markerSysPlatform(plat string) string {
	switch normalizePlat(plat) {
	case "win32", "win-arm":
		return "win32"
	case "darwin", "darwin-arm":
		return "darwin"
	default:
		return "linux"
	}
}

func markerPlatformSystem(plat string) string {
	switch normalizePlat(plat) {
	case "win32", "win-arm":
		return "Windows"
	case "darwin", "darwin-arm":
		return "Darwin"
	default:
		return "Linux"
	}
}
