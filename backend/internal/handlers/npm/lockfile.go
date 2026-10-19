package npm

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

// LookLikeLockfile 粗判粘贴内容是否为 npm / pnpm lockfile。
func LookLikeLockfile(text string) bool {
	s := strings.TrimSpace(text)
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "{") {
		low := strings.ToLower(s[:min(len(s), 800)])
		return strings.Contains(low, `"lockfileversion"`) ||
			(strings.Contains(low, `"packages"`) && strings.Contains(low, `"node_modules/`)) ||
			(strings.Contains(low, `"dependencies"`) && strings.Contains(low, `"resolved"`))
	}
	// pnpm-lock.yaml
	head := s
	if len(head) > 400 {
		head = head[:400]
	}
	low := strings.ToLower(head)
	return strings.Contains(low, "lockfileversion:") ||
		(strings.Contains(low, "packages:") && (strings.Contains(low, "resolution:") || strings.Contains(low, "specifier:")))
}

// ParseLockfile 从 package-lock.json / npm-shrinkwrap.json / pnpm-lock.yaml 提取 tarball URL。
// 返回 urls 与跳过说明（无法解析的条目简述）。
func ParseLockfile(text string) (urls []string, skipped []string) {
	s := strings.TrimSpace(text)
	if s == "" {
		return nil, nil
	}
	if strings.HasPrefix(s, "{") {
		return parsePackageLockJSON(s)
	}
	return parsePnpmLockYAML(s)
}

func parsePackageLockJSON(s string) ([]string, []string) {
	var root map[string]any
	if err := json.Unmarshal([]byte(s), &root); err != nil {
		return nil, []string{"package-lock.json 解析失败"}
	}
	seen := map[string]struct{}{}
	var urls []string
	var skipped []string
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u == "" {
			return
		}
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			skipped = append(skipped, u)
			return
		}
		if _, ok := seen[u]; ok {
			return
		}
		seen[u] = struct{}{}
		urls = append(urls, u)
	}

	// v2/v3: packages
	if pkgs, ok := root["packages"].(map[string]any); ok {
		for key, raw := range pkgs {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if key == "" {
				continue
			}
			if resolved, _ := m["resolved"].(string); resolved != "" {
				add(resolved)
				continue
			}
			// 无 resolved：用 version + 路径推断包名
			ver, _ := m["version"].(string)
			if ver == "" {
				continue
			}
			name := packageNameFromNodeModulesKey(key)
			if name == "" {
				continue
			}
			if u := ReconstructTarballURL(name, ver); u != "" {
				add(u)
			}
		}
	}

	// v1: 嵌套 dependencies
	if deps, ok := root["dependencies"].(map[string]any); ok {
		walkLockDeps(deps, add)
	}

	return urls, skipped
}

func walkLockDeps(deps map[string]any, add func(string)) {
	for _, raw := range deps {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if resolved, _ := m["resolved"].(string); resolved != "" {
			add(resolved)
		}
		if nested, ok := m["dependencies"].(map[string]any); ok {
			walkLockDeps(nested, add)
		}
	}
}

func packageNameFromNodeModulesKey(key string) string {
	key = strings.TrimSpace(key)
	if i := strings.LastIndex(key, "node_modules/"); i >= 0 {
		key = key[i+len("node_modules/"):]
	}
	key = strings.TrimPrefix(key, "node_modules/")
	if key == "" || strings.Contains(key, "/") && !strings.HasPrefix(key, "@") {
		// 嵌套且非 scoped 的复杂路径跳过
		if strings.Count(key, "/") > 1 {
			return ""
		}
	}
	return key
}

// ReconstructTarballURL 按官方 path 布局拼 tarball（仅改 host 由代理完成前的上游 URL）。
func ReconstructTarballURL(name, version string) string {
	name = strings.TrimSpace(name)
	version = strings.TrimSpace(version)
	if name == "" || version == "" {
		return ""
	}
	base := "https://registry.npmmirror.com"
	fileName := name
	if i := strings.LastIndex(name, "/"); i >= 0 {
		fileName = name[i+1:]
	}
	// scoped: /@scope/pkg/-/pkg-ver.tgz
	path := "/" + name + "/-/" + fileName + "-" + version + ".tgz"
	return base + path
}

var (
	pnpmPkgHeaderRe       = regexp.MustCompile(`(?m)^ {2}('([^']+)'|"([^"]+)"|([^:\n]+)): *$`)
	pnpmIntegrityRe       = regexp.MustCompile(`(?m)^\s+integrity:\s*(\S+)`)
	pnpmTarballRe         = regexp.MustCompile(`(?m)^\s+tarball:\s*(\S+)`)
	pnpmResolutionBlockRe = regexp.MustCompile(`(?ms)^ {2}(?:'([^']+)'|"([^"]+)"|([^:\n]+)):.*?(?:\n {4}resolution: \{([^}]*)\}|\n {4}resolution:\n((?: {6}.+\n)*))`)
)

func parsePnpmLockYAML(s string) ([]string, []string) {
	seen := map[string]struct{}{}
	var urls []string
	var skipped []string
	add := func(u string) {
		u = strings.TrimSpace(u)
		u = strings.Trim(u, `"'`)
		if u == "" {
			return
		}
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			skipped = append(skipped, u)
			return
		}
		if _, ok := seen[u]; ok {
			return
		}
		seen[u] = struct{}{}
		urls = append(urls, u)
	}

	// 优先用 resolution 块
	for _, m := range pnpmResolutionBlockRe.FindAllStringSubmatch(s, -1) {
		key := firstNonEmpty(m[1], m[2], m[3])
		inline := m[4]
		block := m[5]
		resText := inline
		if resText == "" {
			resText = block
		}
		if tb := findYAMLValue(resText, "tarball"); tb != "" {
			add(tb)
			continue
		}
		name, ver := splitPnpmPackageKey(key)
		if name != "" && ver != "" {
			if u := ReconstructTarballURL(name, ver); u != "" {
				add(u)
			} else {
				skipped = append(skipped, key)
			}
		}
	}

	if len(urls) > 0 {
		return urls, skipped
	}

	// 回退：扫 packages 段下的 key
	inPackages := false
	for _, line := range strings.Split(s, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "packages:" {
			inPackages = true
			continue
		}
		if inPackages && len(line) > 0 && line[0] != ' ' && line[0] != '\t' && strings.HasSuffix(trim, ":") {
			// 顶层下一节
			break
		}
		if !inPackages {
			continue
		}
		if m := pnpmPkgHeaderRe.FindStringSubmatch(line); m != nil {
			key := firstNonEmpty(m[2], m[3], m[4])
			name, ver := splitPnpmPackageKey(key)
			if name != "" && ver != "" {
				if u := ReconstructTarballURL(name, ver); u != "" {
					add(u)
				}
			}
		}
		if m := pnpmTarballRe.FindStringSubmatch(line); m != nil {
			add(m[1])
		}
	}
	_ = pnpmIntegrityRe
	return urls, skipped
}

func findYAMLValue(text, key string) string {
	re := regexp.MustCompile(`(?m)(?:^|,|\n)\s*` + regexp.QuoteMeta(key) + `:\s*(\S+)`)
	m := re.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return strings.Trim(m[1], `"'`)
}

func splitPnpmPackageKey(key string) (name, version string) {
	key = strings.TrimSpace(key)
	key = strings.Trim(key, `"'`)
	// 形式: /lodash@4.17.21 或 lodash@4.17.21 或 /@babel/core@7.0.0(@babel/...)
	key = strings.TrimPrefix(key, "/")
	// 去掉 peer 后缀 (@...)
	if i := strings.Index(key, "("); i >= 0 {
		key = key[:i]
	}
	if strings.HasPrefix(key, "@") {
		// @scope/name@version
		if i := strings.LastIndex(key, "@"); i > 0 {
			return key[:i], key[i+1:]
		}
		return "", ""
	}
	if i := strings.LastIndex(key, "@"); i > 0 {
		return key[:i], key[i+1:]
	}
	return "", ""
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// NormalizeResolvedURL 将 lockfile 里的 resolved 规范为可下载绝对 URL。
func NormalizeResolvedURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		if u, err := url.Parse(raw); err == nil {
			return u.String()
		}
		return raw
	}
	return ""
}
