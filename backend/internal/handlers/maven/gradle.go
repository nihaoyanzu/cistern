package maven

import (
	"regexp"
	"strings"
)

var (
	gradleQuotedGAVRe = regexp.MustCompile(`(?i)\b(?:implementation|api|compileOnly|runtimeOnly|annotationProcessor|compile|runtime)\s*[\(]?\s*['"]([A-Za-z0-9_.-]+:[A-Za-z0-9_.-]+:[A-Za-z0-9._+-]+)['"]`)
	gradleMapGroupRe  = regexp.MustCompile(`(?i)\bgroup\s*:\s*['"]([^'"]+)['"]`)
	gradleMapNameRe   = regexp.MustCompile(`(?i)\bname\s*:\s*['"]([^'"]+)['"]`)
	gradleMapVerRe    = regexp.MustCompile(`(?i)\bversion\s*:\s*['"]([^'"]+)['"]`)
	gradleTestConfRe  = regexp.MustCompile(`(?i)\b(testImplementation|testCompileOnly|testRuntimeOnly|testCompile|testRuntime|androidTestImplementation)\b`)
	tomlVerLineRe     = regexp.MustCompile(`(?m)^\s*([A-Za-z0-9_.-]+)\s*=\s*["']([^"']+)["']\s*$`)
	tomlLibInlineRe   = regexp.MustCompile(`(?im)^\s*[A-Za-z0-9_.-]+\s*=\s*\{([^}]+)\}\s*$`)
	tomlLibShortRe    = regexp.MustCompile(`(?im)^\s*[A-Za-z0-9_.-]+\s*=\s*["']([A-Za-z0-9_.-]+:[A-Za-z0-9_.-]+:[A-Za-z0-9._+-]+)["']\s*$`)
	tomlModuleRe      = regexp.MustCompile(`(?i)module\s*=\s*["']([^"']+)["']`)
	tomlGroupRe       = regexp.MustCompile(`(?i)group\s*=\s*["']([^'"]+)["']`)
	tomlNameRe        = regexp.MustCompile(`(?i)(?:^|[,{]\s*)name\s*=\s*["']([^'"]+)["']`)
	tomlVersionRe     = regexp.MustCompile(`(?i)version(?:\.ref)?\s*=\s*["']([^'"]+)["']`)
)

// LookLikeBuildGradle 粗判 build.gradle / .kts。
func LookLikeBuildGradle(text string) bool {
	s := strings.TrimSpace(text)
	if s == "" || LookLikePom(s) || LookLikeLibsVersionsTOML(s) {
		return false
	}
	low := strings.ToLower(s)
	if !strings.Contains(low, "implementation") && !strings.Contains(low, "api") &&
		!strings.Contains(low, "compile") && !strings.Contains(low, "runtimeonly") {
		return false
	}
	return gradleQuotedGAVRe.MatchString(s) ||
		(gradleMapGroupRe.MatchString(s) && gradleMapNameRe.MatchString(s) && gradleMapVerRe.MatchString(s))
}

// LookLikeLibsVersionsTOML 粗判 Gradle Version Catalog（libs.versions.toml）。
func LookLikeLibsVersionsTOML(text string) bool {
	s := strings.TrimSpace(text)
	if s == "" || LookLikePom(s) {
		return false
	}
	low := strings.ToLower(s)
	hasVersions := strings.Contains(low, "[versions]")
	hasLibraries := strings.Contains(low, "[libraries]")
	if !hasLibraries && !(hasVersions && strings.Contains(low, "module")) {
		return false
	}
	return hasLibraries || (hasVersions && (strings.Contains(low, "module") || strings.Contains(low, "group")))
}

// ParseBuildGradle 从 build.gradle / .kts 提取 GAV；跳过 test* 配置。
func ParseBuildGradle(text string) (coords []Coordinate, skipped []string) {
	seen := map[string]struct{}{}
	add := func(c Coordinate) {
		key := c.String()
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		coords = append(coords, c)
	}

	for _, m := range gradleQuotedGAVRe.FindAllStringSubmatchIndex(text, -1) {
		full := text[m[0]:m[1]]
		if gradleTestConfRe.MatchString(full) {
			skipped = append(skipped, strings.TrimSpace(full)+" (test)")
			continue
		}
		gav := text[m[2]:m[3]]
		if c, ok := ParseCoordinate(gav); ok {
			add(c)
		} else {
			skipped = append(skipped, gav)
		}
	}

	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		low := strings.ToLower(line)
		if !strings.Contains(low, "group") || !strings.Contains(low, "name") || !strings.Contains(low, "version") {
			continue
		}
		if gradleTestConfRe.MatchString(line) {
			skipped = append(skipped, line+" (test)")
			continue
		}
		gm := gradleMapGroupRe.FindStringSubmatch(line)
		nm := gradleMapNameRe.FindStringSubmatch(line)
		vm := gradleMapVerRe.FindStringSubmatch(line)
		if gm == nil || nm == nil || vm == nil {
			continue
		}
		if c, ok := ParseCoordinate(gm[1] + ":" + nm[1] + ":" + vm[1]); ok {
			add(c)
		}
	}
	return coords, skipped
}

// ParseLibsVersionsTOML 解析 [versions] + [libraries]。
func ParseLibsVersionsTOML(text string) (coords []Coordinate, skipped []string) {
	versions := parseTOMLVersionTable(text)
	seen := map[string]struct{}{}
	add := func(c Coordinate) {
		key := c.String()
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		coords = append(coords, c)
	}

	inLibraries := false
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		low := strings.ToLower(line)
		if strings.HasPrefix(low, "[") && strings.HasSuffix(low, "]") {
			inLibraries = low == "[libraries]"
			continue
		}
		if !inLibraries {
			continue
		}
		if m := tomlLibShortRe.FindStringSubmatch(line); m != nil {
			if c, ok := ParseCoordinate(m[1]); ok {
				add(c)
			} else {
				skipped = append(skipped, m[1])
			}
			continue
		}
		if m := tomlLibInlineRe.FindStringSubmatch(line); m != nil {
			if c, ok := coordFromCatalogBody(m[1], versions); ok {
				add(c)
			} else {
				skipped = append(skipped, line)
			}
		}
	}
	return coords, skipped
}

func parseTOMLVersionTable(text string) map[string]string {
	out := map[string]string{}
	in := false
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		low := strings.ToLower(line)
		if strings.HasPrefix(low, "[") && strings.HasSuffix(low, "]") {
			in = low == "[versions]"
			continue
		}
		if !in {
			continue
		}
		if m := tomlVerLineRe.FindStringSubmatch(line); m != nil {
			out[m[1]] = m[2]
		}
	}
	return out
}

func coordFromCatalogBody(body string, versions map[string]string) (Coordinate, bool) {
	var group, name, ver string
	if m := tomlModuleRe.FindStringSubmatch(body); m != nil {
		mod := strings.TrimSpace(m[1])
		if i := strings.IndexByte(mod, ':'); i > 0 {
			group, name = mod[:i], mod[i+1:]
		}
	}
	if m := tomlGroupRe.FindStringSubmatch(body); m != nil {
		group = strings.TrimSpace(m[1])
	}
	if m := tomlNameRe.FindStringSubmatch(body); m != nil {
		name = strings.TrimSpace(m[1])
	}
	if m := tomlVersionRe.FindStringSubmatch(body); m != nil {
		ver = strings.TrimSpace(m[1])
		if v, ok := versions[ver]; ok {
			ver = v
		}
	}
	if group == "" || name == "" || ver == "" {
		return Coordinate{}, false
	}
	return ParseCoordinate(group + ":" + name + ":" + ver)
}
