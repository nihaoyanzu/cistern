package maven

import (
	"regexp"
	"strings"
)

// Coordinate Maven GAV 坐标。
type Coordinate struct {
	GroupID    string
	ArtifactID string
	Version    string
	Scope      string
	Packaging  string // 默认 jar；pom 表示仅元数据（含 BOM import）
}

// String 返回 group:artifact:version；非 jar 包装时追加 @packaging（如 BOM 的 @pom）。
func (c Coordinate) String() string {
	s := c.GroupID + ":" + c.ArtifactID + ":" + c.Version
	pack := strings.ToLower(strings.TrimSpace(c.Packaging))
	if pack != "" && pack != "jar" {
		s += "@" + pack
	}
	return s
}

var (
	gavLineRe         = regexp.MustCompile(`(?i)^([A-Za-z0-9_.-]+):([A-Za-z0-9_.-]+):([A-Za-z0-9._+-]+)$`)
	xmlnsAttrRe       = regexp.MustCompile(`\sxmlns(:[A-Za-z0-9_]+)?="[^"]*"`)
	xmlPrefixedAttrRe = regexp.MustCompile(`\s[A-Za-z_][\w.-]*:[A-Za-z_][\w.-]*="[^"]*"`)
	propRefRe         = regexp.MustCompile(`\$\{([A-Za-z0-9_.-]+)\}`)
)

// LookLikePom 粗判是否为 pom.xml 内容。
func LookLikePom(text string) bool {
	s := strings.TrimSpace(text)
	if s == "" {
		return false
	}
	head := s
	if len(head) > 800 {
		head = head[:800]
	}
	low := strings.ToLower(head)
	if strings.Contains(low, "<project") && (strings.Contains(low, "maven.apache.org") ||
		strings.Contains(low, "<artifactid") || strings.Contains(low, "<groupid")) {
		return true
	}
	return strings.Contains(low, "<dependencies>") && strings.Contains(low, "<dependency>")
}

// LookLikeGAVList 粗判是否为多行 g:a:v（Gradle/Maven 坐标列表）。
func LookLikeGAVList(text string) bool {
	s := strings.TrimSpace(text)
	if s == "" || LookLikePom(s) || LookLikeGradleProperties(s) ||
		LookLikeBuildGradle(s) || LookLikeLibsVersionsTOML(s) {
		return false
	}
	lines, hits := 0, 0
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		lines++
		if _, ok := ParseCoordinate(line); ok {
			hits++
		}
	}
	return lines > 0 && hits*2 >= lines
}

// LookLikeGradleProperties 粗判 gradle.properties：非空行均为 key=value。
// 含 GAV 值可预取；仅版本号/开关（如 Spring 的 xxxVersion=1.2.3）也会命中，以便给出明确错误而非「不符合格式」。
func LookLikeGradleProperties(text string) bool {
	s := strings.TrimSpace(text)
	if s == "" || LookLikePom(s) || LookLikeBuildGradle(s) || LookLikeLibsVersionsTOML(s) {
		return false
	}
	propLines, gavHits, versionish := 0, 0, 0
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || isPropertiesComment(line) {
			continue
		}
		key, val, ok := splitPropertiesKV(line)
		if !ok {
			return false
		}
		propLines++
		if _, ok := ParseCoordinate(val); ok {
			gavHits++
			continue
		}
		lowKey := strings.ToLower(key)
		if strings.Contains(lowKey, "version") || looksLikeVersionToken(val) {
			versionish++
		}
	}
	if propLines == 0 {
		return false
	}
	if gavHits > 0 {
		return true
	}
	// 纯版本目录：至少一半行像版本属性，且行数不宜太少（避免误伤）
	return versionish >= 3 && versionish*2 >= propLines
}

// ParseGradleProperties 从 gradle.properties 中提取值为 GAV 的条目；其它键跳过。
// 若整份只有版本号而无任何 GAV，skipped[0] 为说明文案。
func ParseGradleProperties(text string) (coords []Coordinate, skipped []string) {
	seen := map[string]struct{}{}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || isPropertiesComment(line) {
			continue
		}
		_, val, ok := splitPropertiesKV(line)
		if !ok {
			skipped = append(skipped, line)
			continue
		}
		c, ok := ParseCoordinate(val)
		if !ok {
			skipped = append(skipped, line)
			continue
		}
		key := c.String()
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		coords = append(coords, c)
	}
	if len(coords) == 0 && len(skipped) > 0 {
		msg := "gradle.properties 仅有版本号无法预取；请粘贴 pom.xml / build.gradle / libs.versions.toml，或 key=group:artifact:version"
		skipped = append([]string{msg}, skipped...)
	}
	return coords, skipped
}

func looksLikeVersionToken(val string) bool {
	val = strings.TrimSpace(val)
	if val == "" || val == "true" || val == "false" {
		return false
	}
	// 3.27.7 / 2.21.7 / 7.1.0-SNAPSHOT / 0.0.48
	dots, digits := 0, 0
	for i := 0; i < len(val); i++ {
		c := val[i]
		switch {
		case c >= '0' && c <= '9':
			digits++
		case c == '.':
			dots++
		case c == '-' || c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
		default:
			return false
		}
	}
	return digits >= 1 && (dots >= 1 || strings.Contains(strings.ToUpper(val), "SNAPSHOT"))
}

func isPropertiesComment(line string) bool {
	return strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") || strings.HasPrefix(line, "//")
}

// splitPropertiesKV 解析 key=value（允许空格）；去掉值两侧引号与行尾 # 注释。
func splitPropertiesKV(line string) (key, val string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || isPropertiesComment(line) {
		return "", "", false
	}
	eq := strings.IndexByte(line, '=')
	if eq <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:eq])
	if key == "" {
		return "", "", false
	}
	val = strings.TrimSpace(line[eq+1:])
	val = stripPropertiesInlineComment(val)
	val = strings.TrimSpace(val)
	val = strings.Trim(val, `"'`)
	val = strings.TrimSpace(val)
	if val == "" {
		return "", "", false
	}
	return key, val, true
}

func stripPropertiesInlineComment(val string) string {
	inSingle, inDouble := false, false
	for i := 0; i < len(val); i++ {
		switch val[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble {
				return strings.TrimSpace(val[:i])
			}
		}
	}
	return val
}

// ParseCoordinate 解析单行 group:artifact:version（可带 @pom / @jar 包装）。
func ParseCoordinate(raw string) (Coordinate, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "#") || strings.HasPrefix(raw, "//") {
		return Coordinate{}, false
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return Coordinate{}, false
	}
	// 去掉 Gradle 引号与尾部分类器简写以外的噪音
	raw = strings.Trim(raw, `"' `)
	if i := strings.IndexAny(raw, " \t"); i >= 0 {
		raw = raw[:i]
	}
	pack := "jar"
	if i := strings.LastIndex(raw, "@"); i > 0 {
		pack = strings.ToLower(strings.TrimSpace(raw[i+1:]))
		raw = raw[:i]
		if pack == "" {
			pack = "jar"
		}
	}
	m := gavLineRe.FindStringSubmatch(raw)
	if m == nil {
		return Coordinate{}, false
	}
	return Coordinate{
		GroupID:    m[1],
		ArtifactID: m[2],
		Version:    m[3],
		Packaging:  pack,
	}, true
}

// ParseGAVList 解析多行坐标列表。
func ParseGAVList(text string) (coords []Coordinate, skipped []string) {
	seen := map[string]struct{}{}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		c, ok := ParseCoordinate(line)
		if !ok {
			skipped = append(skipped, line)
			continue
		}
		key := c.String()
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		coords = append(coords, c)
	}
	return coords, skipped
}

// pomXML 精简 POM 结构。
// 故意不设 XMLName：若依等 POM 含 <name> 子元素，会与 xml.Name 字段冲突导致反序列化失败。
type pomXML struct {
	GroupID              string           `xml:"groupId"`
	ArtifactID           string           `xml:"artifactId"`
	Version              string           `xml:"version"`
	Packaging            string           `xml:"packaging"`
	Parent               pomParent        `xml:"parent"`
	Properties           pomProperties    `xml:"properties"`
	Dependencies         pomDependencies  `xml:"dependencies"`
	DependencyManagement pomDepManagement `xml:"dependencyManagement"`
}

type pomParent struct {
	GroupID      string `xml:"groupId"`
	ArtifactID   string `xml:"artifactId"`
	Version      string `xml:"version"`
	RelativePath string `xml:"relativePath"`
}

type pomProperties struct {
	Raw []byte `xml:",innerxml"`
}

type pomDepManagement struct {
	Dependencies pomDependencies `xml:"dependencies"`
}

type pomDependencies struct {
	Dependency []pomDependency `xml:"dependency"`
}

type pomDependency struct {
	GroupID    string `xml:"groupId"`
	ArtifactID string `xml:"artifactId"`
	Version    string `xml:"version"`
	Scope      string `xml:"scope"`
	Optional   string `xml:"optional"`
	Type       string `xml:"type"`
}

// ParsePomDependencies 解析 pom.xml 的 <dependencies> 与 <dependencyManagement>（本地、不回源）。
// 适用于普通模块 POM，以及若依/Spring 一类仅 BOM import 的父 POM。
// Spring Boot 应用 POM 依赖无 version、靠 parent BOM 管理时，请用 ResolvePomDependencies。
func ParsePomDependencies(text string) (coords []Coordinate, skipped []string) {
	return ResolvePomDependencies(text, nil)
}

// sanitizePomXML 剥默认/前缀命名空间声明，以及 xsi:schemaLocation 等带前缀属性。
// 否则 encoding/xml 在 xmlns 被删后仍见 xsi:* 会报 undefined namespace prefix。
func sanitizePomXML(s string) string {
	s = xmlnsAttrRe.ReplaceAllString(s, "")
	s = xmlPrefixedAttrRe.ReplaceAllString(s, "")
	return s
}

func parsePropertyMap(inner []byte) map[string]string {
	out := map[string]string{}
	if len(inner) == 0 {
		return out
	}
	// <foo>bar</foo>
	re := regexp.MustCompile(`(?s)<([A-Za-z0-9_.-]+)>([^<]*)</([A-Za-z0-9_.-]+)>`)
	for _, m := range re.FindAllSubmatch(inner, -1) {
		if string(m[1]) != string(m[3]) {
			continue
		}
		out[string(m[1])] = strings.TrimSpace(string(m[2]))
	}
	return out
}

// flattenProps 多轮展开属性值中的 ${...}（如 compiler.source → java.version）。
func flattenProps(props map[string]string) map[string]string {
	if len(props) == 0 {
		return props
	}
	for i := 0; i < 8; i++ {
		changed := false
		for k, v := range props {
			nv := resolveProps(v, props)
			if nv != v {
				props[k] = nv
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return props
}

func resolveProps(s string, props map[string]string) string {
	if s == "" || !strings.Contains(s, "${") {
		return s
	}
	for i := 0; i < 8 && strings.Contains(s, "${"); i++ {
		next := propRefRe.ReplaceAllStringFunc(s, func(m string) string {
			sub := propRefRe.FindStringSubmatch(m)
			if len(sub) < 2 {
				return m
			}
			if v, ok := props[sub[1]]; ok && v != "" {
				return v
			}
			return m
		})
		if next == s {
			break
		}
		s = next
	}
	return s
}

// extractDependenciesInner 提取 <dependencies>…</dependencies> 内侧。
// fromManagement=true 时取 dependencyManagement 内的那一段；否则取不在 management 内的顶层段。
func extractDependenciesInner(text string, fromManagement bool) string {
	low := strings.ToLower(text)
	if fromManagement {
		i := strings.Index(low, "<dependencymanagement")
		if i < 0 {
			return ""
		}
		j := strings.Index(low[i:], "</dependencymanagement>")
		if j < 0 {
			return ""
		}
		seg := text[i : i+j]
		segLow := strings.ToLower(seg)
		start := strings.Index(segLow, "<dependencies>")
		end := strings.Index(segLow, "</dependencies>")
		if start < 0 || end < 0 || end <= start {
			return ""
		}
		return seg[start+len("<dependencies>") : end]
	}

	cleaned := text
	cleanedLow := low
	if i := strings.Index(cleanedLow, "<dependencymanagement"); i >= 0 {
		if j := strings.Index(cleanedLow[i:], "</dependencymanagement>"); j >= 0 {
			end := i + j + len("</dependencymanagement>")
			cleaned = text[:i] + text[end:]
			cleanedLow = strings.ToLower(cleaned)
		}
	}
	start := strings.Index(cleanedLow, "<dependencies>")
	end := strings.Index(cleanedLow, "</dependencies>")
	if start < 0 || end < 0 || end <= start {
		return ""
	}
	return cleaned[start+len("<dependencies>") : end]
}
