package pypi

import (
	"regexp"
	"strings"
)

var (
	tomlPackageNameRe    = regexp.MustCompile(`(?im)^\s*name\s*=\s*["']([^"']+)["']\s*$`)
	tomlPackageVersionRe = regexp.MustCompile(`(?im)^\s*version\s*=\s*["']([^"']+)["']\s*$`)
	pipfileAssignRe      = regexp.MustCompile(`^\s*([A-Za-z0-9][A-Za-z0-9_.-]*)\s*=\s*(.+)$`)
)

// LookLikePoetryLock 粗判 poetry.lock。
func LookLikePoetryLock(text string) bool {
	s := strings.TrimSpace(text)
	if s == "" {
		return false
	}
	low := strings.ToLower(s[:min(len(s), 600)])
	return strings.Contains(low, "[[package]]") &&
		(strings.Contains(low, "category =") || strings.Contains(low, "python-versions") ||
			strings.Contains(low, "[metadata]") || strings.Contains(low, "description ="))
}

// LookLikeUVLock 粗判 uv.lock（与 poetry.lock 互斥）。
func LookLikeUVLock(text string) bool {
	s := strings.TrimSpace(text)
	if s == "" || LookLikePoetryLock(s) {
		return false
	}
	low := strings.ToLower(s[:min(len(s), 800)])
	if !strings.Contains(low, "[[package]]") {
		return false
	}
	return strings.Contains(low, "requires-python") ||
		strings.Contains(low, "source = {") ||
		strings.Contains(low, "resolution-markers") ||
		strings.Contains(low, "sdist = {") ||
		strings.Contains(low, "wheels = [")
}

// LookLikePyProject 粗判 PEP 621 / Poetry 的 pyproject.toml。
func LookLikePyProject(text string) bool {
	s := strings.TrimSpace(text)
	if s == "" || LookLikePoetryLock(s) || LookLikeUVLock(s) || LookLikePipfile(s) {
		return false
	}
	return looksLikeTOMLDeps(s)
}

// LookLikePipfile 粗判 Pipfile（[packages] / [dev-packages]）。
func LookLikePipfile(text string) bool {
	s := strings.TrimSpace(text)
	if s == "" {
		return false
	}
	low := strings.ToLower(s)
	if LookLikePoetryLock(s) || LookLikeUVLock(s) {
		return false
	}
	hasPkg := strings.Contains(low, "[packages]") || strings.Contains(low, "[dev-packages]")
	if !hasPkg {
		return false
	}
	// 排除 poetry.lock / pyproject 误伤：Pipfile 通常有 [[source]] 或 verify_ssl
	return strings.Contains(low, "[[source]]") || strings.Contains(low, "verify_ssl") ||
		strings.Contains(low, "[requires]") || (hasPkg && !strings.Contains(low, "[project]"))
}

// ParsePoetryOrUVLock 从 poetry.lock / uv.lock 的 [[package]] 提取 name==version。
func ParsePoetryOrUVLock(text string) (items []string, skipped []string) {
	seen := map[string]struct{}{}
	blocks := splitTOMLTables(text, "[[package]]")
	for _, block := range blocks {
		name, ver := "", ""
		if m := tomlPackageNameRe.FindStringSubmatch(block); m != nil {
			name = strings.TrimSpace(m[1])
		}
		if m := tomlPackageVersionRe.FindStringSubmatch(block); m != nil {
			ver = strings.TrimSpace(m[1])
		}
		if name == "" || ver == "" {
			if name != "" || ver != "" {
				skipped = append(skipped, name+"=="+ver)
			}
			continue
		}
		name = NormalizeName(name)
		if !IsPlausiblePackageName(name) {
			skipped = append(skipped, name+"=="+ver)
			continue
		}
		spec := name + "==" + ver
		if _, dup := seen[spec]; dup {
			continue
		}
		seen[spec] = struct{}{}
		items = append(items, spec)
	}
	return items, skipped
}

// ParsePipfile 解析 [packages] / [dev-packages]。
func ParsePipfile(text string) (items []string, skipped []string) {
	seen := map[string]struct{}{}
	section := ""
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			low := strings.ToLower(line)
			switch low {
			case "[packages]", "[dev-packages]":
				section = low
			default:
				section = ""
			}
			continue
		}
		if section == "" {
			continue
		}
		m := pipfileAssignRe.FindStringSubmatch(line)
		if m == nil {
			skipped = append(skipped, line)
			continue
		}
		name := NormalizeName(m[1])
		if name == "" || name == "python" || !IsPlausiblePackageName(name) {
			skipped = append(skipped, line)
			continue
		}
		rhs := strings.TrimSpace(m[2])
		spec := pipfileRHSToSpec(name, rhs)
		if spec == "" {
			skipped = append(skipped, line)
			continue
		}
		if _, dup := seen[spec]; dup {
			continue
		}
		seen[spec] = struct{}{}
		items = append(items, spec)
	}
	return items, skipped
}

func pipfileRHSToSpec(name, rhs string) string {
	rhs = strings.TrimSpace(rhs)
	if rhs == "" {
		return ""
	}
	// 跳过 path/git/editable 表
	low := strings.ToLower(rhs)
	if strings.HasPrefix(rhs, "{") {
		if strings.Contains(low, "path") || strings.Contains(low, "git") || strings.Contains(low, "editable") {
			return ""
		}
		if m := regexp.MustCompile(`(?i)version\s*=\s*["']([^"']+)["']`).FindStringSubmatch(rhs); m != nil {
			return name + normalizePipVersion(m[1])
		}
		return ""
	}
	rhs = strings.Trim(rhs, `"'`)
	if rhs == "*" || rhs == "" {
		return name
	}
	return name + normalizePipVersion(rhs)
}

func normalizePipVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.Trim(v, `"'`)
	if v == "" || v == "*" {
		return ""
	}
	if strings.HasPrefix(v, "=") || strings.HasPrefix(v, ">") || strings.HasPrefix(v, "<") ||
		strings.HasPrefix(v, "~") || strings.HasPrefix(v, "!") || strings.HasPrefix(v, "^") {
		return v
	}
	return "==" + v
}

func splitTOMLTables(text, header string) []string {
	idx := strings.Index(text, header)
	if idx < 0 {
		return nil
	}
	rest := text[idx:]
	parts := strings.Split(rest, header)
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
