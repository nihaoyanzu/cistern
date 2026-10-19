package maven

import (
	"encoding/xml"
	"fmt"
	"strings"
)

const (
	maxPomParentDepth = 8
	maxPomBOMImports  = 24
)

// PomFetch 回源拉取指定 GAV 的 POM 正文；失败时返回 error。
type PomFetch func(groupID, artifactID, version string) ([]byte, error)

type managedCoord struct {
	Version    string
	Packaging  string
	Classifier string
}

// managedKey Maven DM 按 g:a:type:classifier 区分（同坐标可同时有 jar 与 test-jar）。
func managedKey(groupID, artifactID, typ, classifier string) string {
	if typ == "" {
		typ = "jar"
	}
	return groupID + ":" + artifactID + ":" + typ + ":" + strings.TrimSpace(classifier)
}

// ResolvePomDependencies 解析 POM，并在提供 fetch 时沿 parent / import BOM 补全缺失版本。
//
// 行为概要：
//   - packaging=pom，或仅有 dependencyManagement：把本层 DM（含 import 展开后）也作为可预取坐标（BOM 粘贴）
//   - 普通应用/jar 模块：仅返回 <dependencies>，用 parent+BOM 补版本，不把整个 BOM 清单入队
//   - fetch==nil：不回源，行为接近旧版本地解析（无 version 则跳过）
func ResolvePomDependencies(text string, fetch PomFetch) (coords []Coordinate, skipped []string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	leaf, err := parsePomDocument(text)
	if err != nil {
		return nil, []string{"pom parse error: " + err.Error()}
	}

	chain := []pomXML{leaf}
	// 向上解析 parent 时，用已累积 properties 展开 ${}（子优先）
	walkProps := flattenProps(parsePropertyMap(leaf.Properties.Raw))
	cur := leaf
	for depth := 0; depth < maxPomParentDepth; depth++ {
		pg := resolveProps(strings.TrimSpace(cur.Parent.GroupID), walkProps)
		pa := resolveProps(strings.TrimSpace(cur.Parent.ArtifactID), walkProps)
		pv := resolveProps(strings.TrimSpace(cur.Parent.Version), walkProps)
		if pg == "" || pa == "" || pv == "" || strings.Contains(pv, "${") {
			if strings.TrimSpace(cur.Parent.GroupID) != "" && fetch == nil {
				skipped = append(skipped, fmt.Sprintf("parent %s:%s:%s（需回源补版本）", pg, pa, pv))
			}
			break
		}
		if fetch == nil {
			skipped = append(skipped, fmt.Sprintf("parent %s:%s:%s（需回源补版本）", pg, pa, pv))
			break
		}
		body, ferr := fetch(pg, pa, pv)
		if ferr != nil || len(body) == 0 {
			msg := fmt.Sprintf("parent %s:%s:%s 拉取失败", pg, pa, pv)
			if ferr != nil {
				msg += ": " + ferr.Error()
			}
			skipped = append(skipped, msg)
			break
		}
		parentPom, perr := parsePomDocument(string(body))
		if perr != nil {
			skipped = append(skipped, fmt.Sprintf("parent %s:%s:%s 解析失败: %v", pg, pa, pv, perr))
			break
		}
		chain = append(chain, parentPom)
		// 父 properties 垫底，不覆盖子已有键
		for k, v := range parsePropertyMap(parentPom.Properties.Raw) {
			if strings.TrimSpace(v) == "" {
				continue
			}
			if _, ok := walkProps[k]; !ok {
				walkProps[k] = v
			}
		}
		walkProps = flattenProps(walkProps)
		cur = parentPom
	}

	props := mergePomProps(chain)
	managed := map[string]managedCoord{}
	importBudget := maxPomBOMImports
	// 远→近合并 DM，近端覆盖远端
	for i := len(chain) - 1; i >= 0; i-- {
		applyDependencyManagement(chain[i], props, managed, fetch, &importBudget, &skipped)
	}

	pack := strings.ToLower(strings.TrimSpace(leaf.Packaging))
	includeManaged := pack == "pom" ||
		(len(leaf.Dependencies.Dependency) == 0 && len(leaf.DependencyManagement.Dependencies.Dependency) > 0)

	seen := map[string]struct{}{}
	appendResolved := func(d pomDependency) {
		g := resolveProps(strings.TrimSpace(d.GroupID), props)
		a := resolveProps(strings.TrimSpace(d.ArtifactID), props)
		v := resolveProps(strings.TrimSpace(d.Version), props)
		scope := strings.ToLower(strings.TrimSpace(d.Scope))
		typ := strings.ToLower(strings.TrimSpace(d.Type))
		cls := resolveProps(strings.TrimSpace(d.Classifier), props)
		if typ == "" {
			typ = "jar"
		}
		// import 仅用于补版本，不作为制品入队
		if scope == "import" {
			return
		}
		if v == "" {
			if m, ok := managed[managedKey(g, a, typ, cls)]; ok {
				v = m.Version
				if cls == "" && m.Classifier != "" {
					cls = m.Classifier
				}
			}
		}
		if g == "" || a == "" || v == "" {
			skipped = append(skipped, g+":"+a+":"+v)
			return
		}
		if strings.Contains(v, "${") || strings.Contains(g, "${") || strings.Contains(a, "${") {
			skipped = append(skipped, g+":"+a+":"+v)
			return
		}
		if scope == "test" || scope == "provided" || scope == "system" {
			skipped = append(skipped, g+":"+a+":"+v+" ("+scope+")")
			return
		}
		if SkipPrefetchPackaging(typ) || SkipPrefetchClassifier(cls) {
			reason := typ
			if SkipPrefetchClassifier(cls) {
				reason = "classifier:" + cls
			}
			skipped = append(skipped, g+":"+a+":"+v+" ("+reason+")")
			return
		}
		if strings.EqualFold(strings.TrimSpace(d.Optional), "true") {
			skipped = append(skipped, g+":"+a+":"+v+" (optional)")
			return
		}
		c := Coordinate{GroupID: g, ArtifactID: a, Version: v, Scope: scope, Packaging: typ, Classifier: cls}
		key := c.String()
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		coords = append(coords, c)
	}

	for _, d := range leaf.Dependencies.Dependency {
		appendResolved(d)
	}
	if includeManaged {
		// 已展开 import 后的 managed 表 → 坐标（key = g:a:type:classifier）
		for key, m := range managed {
			parts := strings.SplitN(key, ":", 4)
			if len(parts) != 4 {
				continue
			}
			appendResolved(pomDependency{
				GroupID:    parts[0],
				ArtifactID: parts[1],
				Version:    m.Version,
				Type:       m.Packaging,
				Classifier: m.Classifier,
			})
		}
	}
	return coords, skipped
}

func parsePomDocument(text string) (pomXML, error) {
	text = sanitizePomXML(strings.TrimSpace(text))
	var pom pomXML
	err := xml.Unmarshal([]byte(text), &pom)
	if err == nil {
		return pom, nil
	}
	// 回退：抠 parent / dependencies / dependencyManagement
	if block := extractParentInner(text); block != "" {
		var p pomParent
		if err2 := xml.Unmarshal([]byte("<parent>"+block+"</parent>"), &p); err2 == nil {
			pom.Parent = p
		}
	}
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
	if g, a, v, pack := extractProjectCoords(text); g != "" || a != "" || v != "" || pack != "" {
		if pom.GroupID == "" {
			pom.GroupID = g
		}
		if pom.ArtifactID == "" {
			pom.ArtifactID = a
		}
		if pom.Version == "" {
			pom.Version = v
		}
		if pom.Packaging == "" {
			pom.Packaging = pack
		}
	}
	if len(pom.Dependencies.Dependency) == 0 && len(pom.DependencyManagement.Dependencies.Dependency) == 0 &&
		strings.TrimSpace(pom.Parent.GroupID) == "" {
		return pomXML{}, err
	}
	return pom, nil
}

func mergePomProps(chain []pomXML) map[string]string {
	props := map[string]string{}
	// 远→近：子覆盖父
	for i := len(chain) - 1; i >= 0; i-- {
		for k, v := range parsePropertyMap(chain[i].Properties.Raw) {
			if strings.TrimSpace(v) != "" {
				props[k] = v
			}
		}
	}
	leaf := chain[0]
	// project.* / parent.* 内置属性
	pg, pa, pv := inheritProjectCoords(chain)
	setProp(props, "project.groupId", pg)
	setProp(props, "project.artifactId", pa)
	setProp(props, "project.version", pv)
	setProp(props, "groupId", pg)
	setProp(props, "artifactId", pa)
	setProp(props, "version", pv)
	if len(chain) > 1 || strings.TrimSpace(leaf.Parent.GroupID) != "" {
		setProp(props, "parent.groupId", strings.TrimSpace(leaf.Parent.GroupID))
		setProp(props, "parent.artifactId", strings.TrimSpace(leaf.Parent.ArtifactID))
		setProp(props, "parent.version", resolveProps(strings.TrimSpace(leaf.Parent.Version), props))
	}
	return flattenProps(props)
}

func inheritProjectCoords(chain []pomXML) (groupID, artifactID, version string) {
	leaf := chain[0]
	groupID = strings.TrimSpace(leaf.GroupID)
	artifactID = strings.TrimSpace(leaf.ArtifactID)
	version = strings.TrimSpace(leaf.Version)
	for _, p := range chain {
		if groupID == "" {
			groupID = strings.TrimSpace(p.GroupID)
		}
		if groupID == "" {
			groupID = strings.TrimSpace(p.Parent.GroupID)
		}
		if version == "" {
			version = strings.TrimSpace(p.Version)
		}
		if version == "" {
			version = strings.TrimSpace(p.Parent.Version)
		}
		if groupID != "" && version != "" {
			break
		}
	}
	return groupID, artifactID, version
}

func setProp(props map[string]string, key, val string) {
	val = strings.TrimSpace(val)
	if val == "" {
		return
	}
	props[key] = val
}

func applyDependencyManagement(
	pom pomXML,
	props map[string]string,
	managed map[string]managedCoord,
	fetch PomFetch,
	importBudget *int,
	skipped *[]string,
) {
	for _, d := range pom.DependencyManagement.Dependencies.Dependency {
		g := resolveProps(strings.TrimSpace(d.GroupID), props)
		a := resolveProps(strings.TrimSpace(d.ArtifactID), props)
		v := resolveProps(strings.TrimSpace(d.Version), props)
		scope := strings.ToLower(strings.TrimSpace(d.Scope))
		typ := strings.ToLower(strings.TrimSpace(d.Type))
		if g == "" || a == "" {
			continue
		}
		if scope == "import" && (typ == "pom" || typ == "") {
			if v == "" || strings.Contains(v, "${") {
				*skipped = append(*skipped, g+":"+a+":"+v+" (bom import)")
				continue
			}
			if fetch == nil {
				// 无回源：保留 import@pom，供后续 Expand 拉取 BOM
				managed[managedKey(g, a, "pom", "")] = managedCoord{Version: v, Packaging: "pom"}
				continue
			}
			if *importBudget <= 0 {
				*skipped = append(*skipped, g+":"+a+":"+v+" (bom import 未展开)")
				continue
			}
			*importBudget--
			body, err := fetch(g, a, v)
			if err != nil || len(body) == 0 {
				msg := fmt.Sprintf("bom %s:%s:%s 拉取失败", g, a, v)
				if err != nil {
					msg += ": " + err.Error()
				}
				*skipped = append(*skipped, msg)
				// 展开失败仍入队 BOM 本身，避免整份 POM 因上游 404 无法预取
				managed[managedKey(g, a, "pom", "")] = managedCoord{Version: v, Packaging: "pom"}
				continue
			}
			bom, err := parsePomDocument(string(body))
			if err != nil {
				*skipped = append(*skipped, fmt.Sprintf("bom %s:%s:%s 解析失败: %v", g, a, v, err))
				continue
			}
			// 合并 BOM 自身 properties（不覆盖已有近端 props）
			bomProps := map[string]string{}
			for k, val := range props {
				bomProps[k] = val
			}
			for k, val := range parsePropertyMap(bom.Properties.Raw) {
				if strings.TrimSpace(val) == "" {
					continue
				}
				if _, ok := bomProps[k]; !ok {
					bomProps[k] = val
				}
			}
			setProp(bomProps, "project.groupId", strings.TrimSpace(bom.GroupID))
			setProp(bomProps, "project.artifactId", strings.TrimSpace(bom.ArtifactID))
			setProp(bomProps, "project.version", strings.TrimSpace(bom.Version))
			if strings.TrimSpace(bom.Version) == "" {
				setProp(bomProps, "project.version", v)
			}
			bomProps = flattenProps(bomProps)
			applyDependencyManagement(bom, bomProps, managed, fetch, importBudget, skipped)
			continue
		}
		if v == "" || strings.Contains(v, "${") || strings.Contains(g, "${") || strings.Contains(a, "${") {
			continue
		}
		if typ == "" {
			typ = "jar"
		}
		cls := resolveProps(strings.TrimSpace(d.Classifier), props)
		managed[managedKey(g, a, typ, cls)] = managedCoord{Version: v, Packaging: typ, Classifier: cls}
	}
}

func extractParentInner(text string) string {
	low := strings.ToLower(text)
	i := strings.Index(low, "<parent")
	if i < 0 {
		return ""
	}
	// 跳过属性到 '>'
	gt := strings.Index(low[i:], ">")
	if gt < 0 {
		return ""
	}
	start := i + gt + 1
	j := strings.Index(low[start:], "</parent>")
	if j < 0 {
		return ""
	}
	return text[start : start+j]
}

func extractProjectCoords(text string) (groupID, artifactID, version, packaging string) {
	low := strings.ToLower(text)
	// 去掉 parent 段，避免误取 parent 的 groupId
	cleaned := text
	cleanedLow := low
	if i := strings.Index(cleanedLow, "<parent"); i >= 0 {
		if j := strings.Index(cleanedLow[i:], "</parent>"); j >= 0 {
			end := i + j + len("</parent>")
			cleaned = text[:i] + text[end:]
			cleanedLow = strings.ToLower(cleaned)
		}
	}
	groupID = firstXMLTag(cleaned, cleanedLow, "groupid")
	artifactID = firstXMLTag(cleaned, cleanedLow, "artifactid")
	version = firstXMLTag(cleaned, cleanedLow, "version")
	packaging = firstXMLTag(cleaned, cleanedLow, "packaging")
	return
}

func firstXMLTag(text, low, tag string) string {
	open := "<" + tag
	i := strings.Index(low, open)
	if i < 0 {
		return ""
	}
	gt := strings.Index(low[i:], ">")
	if gt < 0 {
		return ""
	}
	start := i + gt + 1
	close := "</" + tag + ">"
	j := strings.Index(low[start:], close)
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(text[start : start+j])
}
