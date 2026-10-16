package pypi

import (
	"strings"
	"unicode"

	pypihandler "github.com/livehl/mirrorhub/internal/handlers/pypi"
	"github.com/livehl/mirrorhub/internal/platform"
)

const textDetectPriorityPyPI = 0

func (p *PyPIPlatform) TextDetectPriority() int { return textDetectPriorityPyPI }

// LookLikePrefetchText 认 requirements / PEP 508 / pyproject.toml / poetry.lock / uv.lock / Pipfile；异平台格式排除。
func (p *PyPIPlatform) LookLikePrefetchText(text string) bool {
	s := strings.TrimSpace(text)
	if s == "" {
		return false
	}
	if pypihandler.LookLikePoetryLock(s) || pypihandler.LookLikeUVLock(s) ||
		pypihandler.LookLikePipfile(s) || pypihandler.LookLikePyProject(s) {
		return true
	}
	if looksLikeForeignPrefetchBlob(s) {
		return false
	}

	lines := 0
	hits := 0
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines++
		if looksLikeRequirementLine(line) {
			hits++
		}
	}
	return lines > 0 && hits*2 >= lines
}

// looksLikeForeignPrefetchBlob 粗判明显属于其它生态的粘贴（lockfile / pom / go.mod 等）。
func looksLikeForeignPrefetchBlob(s string) bool {
	head := s
	if len(head) > 800 {
		head = head[:800]
	}
	low := strings.ToLower(head)

	// yarn / npm / pnpm lockfile 或 package.json
	if strings.Contains(low, "# yarn lockfile") || strings.Contains(low, `"lockfileversion"`) || strings.Contains(low, "lockfileversion:") {
		return true
	}
	// docker compose
	if strings.Contains(low, "services:") && strings.Contains(low, "image:") {
		return true
	}
	// gradle build / version catalog
	if strings.Contains(low, "[libraries]") && (strings.Contains(low, "[versions]") || strings.Contains(low, "module")) {
		return true
	}
	if strings.Contains(low, "implementation") && strings.Count(s, ":") >= 2 &&
		(strings.Contains(low, "dependencies") || strings.Contains(low, "plugins")) {
		return true
	}
	if strings.Contains(low, `"node_modules/"`) && strings.Contains(low, `"packages"`) {
		return true
	}
	if strings.Contains(low, "packages:") && (strings.Contains(low, "resolution:") || strings.Contains(low, "specifier:")) {
		return true
	}
	if strings.HasPrefix(strings.TrimSpace(s), "{") &&
		(strings.Contains(low, `"dependencies"`) || strings.Contains(low, `"devdependencies"`)) &&
		(strings.Contains(low, `"name"`) || strings.Contains(low, `"private"`)) {
		return true
	}

	// Maven pom
	if strings.Contains(low, "<project") && (strings.Contains(low, "maven.apache.org") ||
		strings.Contains(low, "<artifactid") || strings.Contains(low, "<groupid")) {
		return true
	}
	if strings.Contains(low, "<dependencies>") && strings.Contains(low, "<dependency>") {
		return true
	}
	// gradle.properties：多行 key=group:artifact:version
	if looksLikeGradlePropertiesBlob(s) {
		return true
	}

	// go.mod / go.sum
	if strings.Contains(low, "\nmodule ") || strings.HasPrefix(low, "module ") ||
		strings.Contains(low, "\nrequire ") || strings.HasPrefix(low, "require ") {
		return true
	}
	if strings.Contains(low, "h1:") && strings.Contains(s, " v") {
		return true
	}
	return false
}

func looksLikeRequirementLine(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	lower := strings.ToLower(line)
	if strings.HasPrefix(lower, "-r ") || strings.HasPrefix(lower, "--requirement") {
		return true
	}
	if strings.HasPrefix(line, "-") {
		return false
	}
	// 排除明显镜像 / GAV / go module（本包启发式，不调其它平台解析器）
	if looksLikeDockerImageLine(line) {
		return false
	}
	if strings.Count(line, ":") >= 2 && !strings.Contains(line, " ") {
		// group:artifact:version
		return false
	}
	if looksLikeGoModuleLine(line) {
		return false
	}
	if looksLikeHFRepoLine(line) {
		return false
	}
	req, err := pypihandler.ParseRequirement(line)
	if err != nil {
		return false
	}
	name := pypihandler.NormalizeName(req.Name)
	return pypihandler.IsPlausiblePackageName(name)
}

// looksLikeDockerImageLine：无空白、无 PEP508 比较符的 name:tag / name@sha256:...
func looksLikeDockerImageLine(line string) bool {
	if strings.ContainsAny(line, "; \t") {
		return false
	}
	if strings.Contains(line, "==") || strings.Contains(line, ">=") ||
		strings.Contains(line, "<=") || strings.Contains(line, "~=") ||
		strings.Contains(line, "!=") {
		return false
	}
	if strings.Contains(line, "@sha256:") || strings.Contains(line, "@SHA256:") {
		return true
	}
	i := strings.LastIndex(line, ":")
	if i <= 0 || i == len(line)-1 {
		return false
	}
	tag := line[i+1:]
	if strings.Contains(tag, "/") {
		return false
	}
	// tag 以字母数字开头（docker tag / digest 前缀已在上方处理）
	r := []rune(tag)
	if len(r) == 0 || !unicode.IsLetter(r[0]) && !unicode.IsDigit(r[0]) {
		return false
	}
	return true
}

// looksLikeGoModuleLine：path@version 且 version 以 v+数字开头、path 含域名点。
func looksLikeGoModuleLine(line string) bool {
	if strings.ContainsAny(line, " \t;") {
		return false
	}
	i := strings.LastIndex(line, "@")
	if i <= 0 || i == len(line)-1 {
		return false
	}
	path, ver := line[:i], line[i+1:]
	if !strings.HasPrefix(ver, "v") || len(ver) < 2 || ver[1] < '0' || ver[1] > '9' {
		return false
	}
	first, _, _ := strings.Cut(path, "/")
	return strings.Contains(first, ".")
}

// looksLikeGradlePropertiesBlob：非空行均为 key=value，且至少一行值像 GAV（不调 maven handler）。
func looksLikeGradlePropertiesBlob(s string) bool {
	propLines, gavHits := 0, 0
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			return false
		}
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		if i := strings.IndexByte(val, '#'); i >= 0 {
			val = strings.TrimSpace(val[:i])
		}
		propLines++
		if strings.Count(val, ":") >= 2 && !strings.ContainsAny(val, " \t") {
			gavHits++
		}
	}
	return propLines > 0 && gavHits > 0
}

// looksLikeHFRepoLine：owner/repo 或 datasets/...（无 PEP508 运算符），避免误认成包名。
func looksLikeHFRepoLine(line string) bool {
	if strings.ContainsAny(line, "; \t=<>!~") {
		return false
	}
	if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") || strings.HasPrefix(line, "hf://") {
		return strings.Contains(line, "/") && !strings.Contains(line, "==")
	}
	s := line
	if i := strings.LastIndex(s, "@"); i > 0 {
		s = s[:i]
	}
	s = strings.TrimPrefix(s, "datasets/")
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return false
	}
	for _, p := range parts {
		if p == "" || strings.ContainsAny(p, "@#?:") {
			return false
		}
	}
	// 首段含点更像 go 模块路径，已由 looksLikeGoModuleLine 处理
	return !strings.Contains(parts[0], ".")
}

func (p *PyPIPlatform) ParsePrefetchText(text string) (platform.TextDetectResult, bool) {
	s := strings.TrimSpace(text)
	if pypihandler.LookLikePoetryLock(s) {
		parsed, skip := pypihandler.ParsePoetryOrUVLock(s)
		if len(parsed) == 0 {
			return platform.TextDetectResult{Skipped: skip, Kind: "pypi_poetry_lock"}, false
		}
		return platform.TextDetectResult{Items: parsed, Skipped: skip, Kind: "pypi_poetry_lock"}, true
	}
	if pypihandler.LookLikeUVLock(s) {
		parsed, skip := pypihandler.ParsePoetryOrUVLock(s)
		if len(parsed) == 0 {
			return platform.TextDetectResult{Skipped: skip, Kind: "pypi_uv_lock"}, false
		}
		return platform.TextDetectResult{Items: parsed, Skipped: skip, Kind: "pypi_uv_lock"}, true
	}
	if pypihandler.LookLikePipfile(s) {
		parsed, skip := pypihandler.ParsePipfile(s)
		if len(parsed) == 0 {
			return platform.TextDetectResult{Skipped: skip, Kind: "pypi_pipfile"}, false
		}
		return platform.TextDetectResult{Items: parsed, Skipped: skip, Kind: "pypi_pipfile"}, true
	}
	if pypihandler.LookLikePyProject(s) {
		parsed, skip := pypihandler.ParseDependencyText(s)
		if len(parsed) == 0 {
			return platform.TextDetectResult{Skipped: skip, Kind: "pypi_pyproject"}, false
		}
		return platform.TextDetectResult{Items: parsed, Skipped: skip, Kind: "pypi_pyproject"}, true
	}
	if !p.LookLikePrefetchText(text) {
		return platform.TextDetectResult{}, false
	}
	parsed, skip := pypihandler.ParseDependencyText(text)
	if len(parsed) == 0 {
		return platform.TextDetectResult{Skipped: skip, Kind: "pypi_req"}, false
	}
	return platform.TextDetectResult{Items: parsed, Skipped: skip, Kind: "pypi_req"}, true
}
