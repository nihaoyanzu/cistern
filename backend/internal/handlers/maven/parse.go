package maven

import (
	"encoding/xml"
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
	if s == "" || LookLikePom(s) {
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
	Packaging            string             `xml:"packaging"`
	Properties           pomProperties      `xml:"properties"`
	Dependencies         pomDependencies    `xml:"dependencies"`
	DependencyManagement pomDepManagement   `xml:"dependencyManagement"`
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

// ParsePomDependencies 解析 pom.xml 的 <dependencies> 与 <dependencyManagement>。
// 适用于普通模块 POM，以及若依/Spring 一类仅 BOM import 的父 POM。
// 会清洗 xmlns / 带前缀属性，并用 <properties> 多轮替换 ${name}。
func ParsePomDependencies(text string) (coords []Coordinate, skipped []string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	text = sanitizePomXML(text)
	var pom pomXML
	if err := xml.Unmarshal([]byte(text), &pom); err != nil {
		// 回退：分别抠顶层 dependencies / dependencyManagement
		if block := extractDependenciesInner(text, false); block != "" {
			var deps pomDependencies
			if err2 := xml.Unmarshal([]byte("<dependencies>"+block+"</dependencies>"), &deps); err2 == nil {
				pom.Dependencies = deps
			}
		}
		if block := extractDependenciesInner(text, true); block != "" {
			var deps pomDependencies
			if err2 := xml.Unmarshal([]byte("<dependencies>"+block+"</dependencies>"), &deps); err2 == nil {
				pom.DependencyManagement.Dependencies = deps
			}
		}
		if len(pom.Dependencies.Dependency) == 0 && len(pom.DependencyManagement.Dependencies.Dependency) == 0 {
			return nil, []string{"pom parse error: " + err.Error()}
		}
	}

	props := flattenProps(parsePropertyMap(pom.Properties.Raw))
	seen := map[string]struct{}{}
	appendDep := func(d pomDependency) {
		g := resolveProps(strings.TrimSpace(d.GroupID), props)
		a := resolveProps(strings.TrimSpace(d.ArtifactID), props)
		v := resolveProps(strings.TrimSpace(d.Version), props)
		if g == "" || a == "" || v == "" {
			skipped = append(skipped, g+":"+a+":"+v)
			return
		}
		if strings.Contains(v, "${") || strings.Contains(g, "${") || strings.Contains(a, "${") {
			skipped = append(skipped, g+":"+a+":"+v)
			return
		}
		scope := strings.ToLower(strings.TrimSpace(d.Scope))
		if scope == "test" || scope == "provided" || scope == "system" {
			skipped = append(skipped, g+":"+a+":"+v+" ("+scope+")")
			return
		}
		if strings.EqualFold(strings.TrimSpace(d.Optional), "true") {
			skipped = append(skipped, g+":"+a+":"+v+" (optional)")
			return
		}
		pack := strings.ToLower(strings.TrimSpace(d.Type))
		if pack == "" {
			pack = "jar"
		}
		// BOM import：必须按 pom 拉取以便展开其 dependencyManagement
		if scope == "import" || pack == "pom" {
			pack = "pom"
		}
		c := Coordinate{GroupID: g, ArtifactID: a, Version: v, Scope: scope, Packaging: pack}
		key := c.String()
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		coords = append(coords, c)
	}

	for _, d := range pom.Dependencies.Dependency {
		appendDep(d)
	}
	for _, d := range pom.DependencyManagement.Dependencies.Dependency {
		appendDep(d)
	}
	return coords, skipped
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
