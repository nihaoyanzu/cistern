package pypi

import (
	"bufio"
	"regexp"
	"strings"
)

var (
	eggNameRe        = regexp.MustCompile(`(?i)(?:#|&)egg=([A-Za-z0-9_.-]+)`)
	tomlDepsArrayRe  = regexp.MustCompile(`(?im)(?:^|[\n\r])[ \t]*dependencies[ \t]*=[ \t]*\[`)
	tomlSectionRe    = regexp.MustCompile(`(?m)^\[([^\]]+)\][ \t]*$`)
	poetryDepTableRe = regexp.MustCompile(`(?i)^tool\.poetry(?:\.group\.[^.\]]+)?\.dependencies$`)
	optDepTableRe    = regexp.MustCompile(`(?i)^(project\.optional-dependencies|dependency-groups)$`)
	poetryAssignRe   = regexp.MustCompile(`^\s*([A-Za-z0-9][A-Za-z0-9_.-]*)\s*=\s*(.+)$`)
	poetryVersionInRe = regexp.MustCompile(`(?i)version\s*=\s*["']([^"']+)["']`)
)

// ParseDependencyText 解析粘贴的依赖文本：
//   - requirements.txt（含注释、续行、部分 pip 选项跳过）
//   - 多行 PEP 508 规格
//   - pyproject.toml / poetry 中 dependencies 条目（仅依赖字段，不扫全文引号）
func ParseDependencyText(text string) (items []string, skipped []string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	if looksLikeTOMLDeps(text) {
		return parseTOMLDependencies(text)
	}
	return parseRequirementsText(text)
}

func looksLikeTOMLDeps(text string) bool {
	low := strings.ToLower(text)
	return strings.Contains(low, "dependencies") &&
		(strings.Contains(text, "[project]") ||
			strings.Contains(text, "[tool.poetry") ||
			strings.Contains(low, "dependencies =") ||
			strings.Contains(low, "dependencies="))
}

func parseTOMLDependencies(text string) (items []string, skipped []string) {
	seen := map[string]struct{}{}

	// PEP 621：仅 dependencies = [ ... ] 数组内的引号字符串
	for _, body := range findNamedTOMLArrays(text, tomlDepsArrayRe) {
		collectQuotedSpecs(body, &items, &skipped, seen)
	}

	// optional-dependencies / dependency-groups 下各数组
	for _, section := range findTOMLSectionBodies(text, optDepTableRe) {
		for _, body := range findAllTOMLArrays(section) {
			collectQuotedSpecs(body, &items, &skipped, seen)
		}
	}

	// Poetry：表键为包名（跳过 python）
	for _, section := range findTOMLSectionBodies(text, poetryDepTableRe) {
		collectPoetryTableSpecs(section, &items, &skipped, seen)
	}

	return items, skipped
}

func collectQuotedSpecs(body string, items, skipped *[]string, seen map[string]struct{}) {
	for _, raw := range extractQuotedStrings(body) {
		acceptTOMLSpec(strings.TrimSpace(raw), items, skipped, seen)
	}
}

// extractQuotedStrings 按开闭引号提取（支持 "…'…'" 这类环境标记）。
// 跳过 # 行注释，避免 Starlette's 等撇号被当成单引号字符串。
func extractQuotedStrings(body string) []string {
	var out []string
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c == '#' {
			for i < len(body) && body[i] != '\n' && body[i] != '\r' {
				i++
			}
			continue
		}
		if c != '"' && c != '\'' {
			continue
		}
		quote := c
		i++
		start := i
		for i < len(body) {
			if body[i] == '\\' && i+1 < len(body) {
				i += 2
				continue
			}
			if body[i] == quote {
				out = append(out, body[start:i])
				break
			}
			i++
		}
	}
	return out
}

func collectPoetryTableSpecs(section string, items, skipped *[]string, seen map[string]struct{}) {
	sc := bufio.NewScanner(strings.NewReader(section))
	for sc.Scan() {
		line := strings.TrimSpace(stripTOMLComment(sc.Text()))
		if line == "" {
			continue
		}
		m := poetryAssignRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		if strings.EqualFold(name, "python") {
			continue
		}
		val := strings.TrimSpace(m[2])
		spec := name
		switch {
		case strings.HasPrefix(val, "{"):
			if vm := poetryVersionInRe.FindStringSubmatch(val); len(vm) == 2 {
				if s := poetryVersionToSpec(name, vm[1]); s != "" {
					spec = s
				}
			} else {
				// path/git/url 等复杂依赖：跳过
				*skipped = append(*skipped, line)
				continue
			}
		case strings.HasPrefix(val, `"`) || strings.HasPrefix(val, `'`):
			ver := strings.Trim(val, `"'`)
			if s := poetryVersionToSpec(name, ver); s != "" {
				spec = s
			}
		default:
			*skipped = append(*skipped, line)
			continue
		}
		acceptTOMLSpec(spec, items, skipped, seen)
	}
}

// poetryVersionToSpec 尽力把 poetry 约束转成可解析规格；无法转换时只保留包名。
func poetryVersionToSpec(name, ver string) string {
	ver = strings.TrimSpace(ver)
	if ver == "" || ver == "*" {
		return name
	}
	if strings.HasPrefix(ver, "^") || strings.HasPrefix(ver, "~") {
		return name
	}
	// 已是 PEP 440 运算符或裸版本
	if strings.ContainsAny(ver, "><=!~") {
		return name + ver
	}
	return name + "==" + ver
}

func acceptTOMLSpec(raw string, items, skipped *[]string, seen map[string]struct{}) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") ||
		strings.Contains(raw, "://") || strings.HasPrefix(raw, "git+") {
		*skipped = append(*skipped, raw)
		return
	}
	req, err := ParseRequirement(raw)
	if err != nil {
		*skipped = append(*skipped, raw)
		return
	}
	if !IsPlausiblePackageName(req.Name) {
		*skipped = append(*skipped, raw)
		return
	}
	if _, ok := seen[raw]; ok {
		return
	}
	seen[raw] = struct{}{}
	*items = append(*items, raw)
}

func findNamedTOMLArrays(text string, re *regexp.Regexp) []string {
	var out []string
	for _, loc := range re.FindAllStringIndex(text, -1) {
		// loc 匹配到 '[' 为止（含），找 '[' 位置
		open := strings.LastIndexByte(text[loc[0]:loc[1]], '[')
		if open < 0 {
			continue
		}
		openIdx := loc[0] + open
		closeIdx, ok := scanTOMLBracket(text, openIdx)
		if !ok {
			continue
		}
		out = append(out, text[openIdx+1:closeIdx])
	}
	return out
}

func findAllTOMLArrays(text string) []string {
	var out []string
	for i := 0; i < len(text); i++ {
		if text[i] != '[' {
			continue
		}
		// 跳过表头 [section]
		if isTOMLTableHeaderAt(text, i) {
			continue
		}
		closeIdx, ok := scanTOMLBracket(text, i)
		if !ok {
			break
		}
		out = append(out, text[i+1:closeIdx])
		i = closeIdx
	}
	return out
}

func isTOMLTableHeaderAt(text string, i int) bool {
	// '[' 位于行首（忽略前导空白）才可能是表头
	j := i - 1
	for j >= 0 && (text[j] == ' ' || text[j] == '\t') {
		j--
	}
	if j >= 0 && text[j] != '\n' && text[j] != '\r' {
		return false
	}
	closeIdx, ok := scanTOMLBracket(text, i)
	if !ok {
		return false
	}
	rest := strings.TrimSpace(text[closeIdx+1:])
	return rest == "" || rest[0] == '\n' || rest[0] == '\r' || rest[0] == '#'
}

func findTOMLSectionBodies(text string, nameRe *regexp.Regexp) []string {
	locs := tomlSectionRe.FindAllStringSubmatchIndex(text, -1)
	if len(locs) == 0 {
		return nil
	}
	var out []string
	for i, loc := range locs {
		name := text[loc[2]:loc[3]]
		if !nameRe.MatchString(name) {
			continue
		}
		start := loc[1]
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		out = append(out, text[start:end])
	}
	return out
}

func scanTOMLBracket(text string, openIdx int) (int, bool) {
	depth := 0
	inSingle, inDouble := false, false
	for i := openIdx; i < len(text); i++ {
		c := text[i]
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		if inDouble {
			if c == '"' {
				inDouble = false
			}
			continue
		}
		switch c {
		case '#':
			// 行注释：撇号等不得干扰括号/引号配对
			for i+1 < len(text) && text[i+1] != '\n' && text[i+1] != '\r' {
				i++
			}
		case '\'':
			inSingle = true
		case '"':
			inDouble = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

func stripTOMLComment(line string) string {
	inSingle, inDouble := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch c {
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
				return line[:i]
			}
		}
	}
	return line
}

func parseRequirementsText(text string) (items []string, skipped []string) {
	// 合并续行
	var lines []string
	var buf strings.Builder
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		trimRight := strings.TrimRight(line, " \t")
		if strings.HasSuffix(trimRight, "\\") {
			buf.WriteString(strings.TrimSuffix(trimRight, "\\"))
			buf.WriteByte(' ')
			continue
		}
		buf.WriteString(line)
		lines = append(lines, buf.String())
		buf.Reset()
	}
	if buf.Len() > 0 {
		lines = append(lines, buf.String())
	}

	seen := map[string]struct{}{}
	for _, line := range lines {
		line = stripReqComment(line)
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// pip 选项 / 嵌套文件
		if strings.HasPrefix(line, "-") {
			skipped = append(skipped, line)
			// -e git+...#egg=pkg
			if strings.HasPrefix(line, "-e ") || strings.HasPrefix(line, "--editable") {
				if m := eggNameRe.FindStringSubmatch(line); len(m) == 2 {
					name := m[1]
					if _, ok := seen[name]; !ok {
						seen[name] = struct{}{}
						items = append(items, name)
					}
				}
			}
			continue
		}
		if isVCSReq(line) {
			if m := eggNameRe.FindStringSubmatch(line); len(m) == 2 {
				name := m[1]
				if _, ok := seen[name]; !ok {
					seen[name] = struct{}{}
					items = append(items, name)
				} else {
					skipped = append(skipped, line)
				}
			} else {
				skipped = append(skipped, line)
			}
			continue
		}
		if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			if _, ok := seen[line]; !ok {
				seen[line] = struct{}{}
				items = append(items, line)
			}
			continue
		}
		// 去掉每行末尾环境标记已由 ParseRequirement 处理；这里先粗校验
		if _, err := ParseRequirement(line); err != nil {
			skipped = append(skipped, line)
			continue
		}
		if _, ok := seen[line]; ok {
			continue
		}
		seen[line] = struct{}{}
		items = append(items, line)
	}
	return items, skipped
}

func stripReqComment(line string) string {
	inSingle, inDouble := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch c {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if inSingle || inDouble {
				continue
			}
			// 仅「行首 #」或「空白后的 #」视为注释，避免剥掉 URL fragment（如 #egg=）
			if i == 0 || line[i-1] == ' ' || line[i-1] == '\t' {
				return line[:i]
			}
		}
	}
	return line
}

func isVCSReq(line string) bool {
	low := strings.ToLower(line)
	return strings.HasPrefix(low, "git+") ||
		strings.HasPrefix(low, "hg+") ||
		strings.HasPrefix(low, "svn+") ||
		strings.HasPrefix(low, "bzr+") ||
		strings.Contains(low, "git+")
}
