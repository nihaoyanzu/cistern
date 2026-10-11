package goproxy

import (
	"strings"
)

// ModuleRef 预取用的 module@version。
type ModuleRef struct {
	Path    string
	Version string
}

// LookLikeGoMod 粗判是否为 go.mod 内容。
func LookLikeGoMod(text string) bool {
	s := strings.TrimSpace(text)
	if s == "" {
		return false
	}
	// go.sum 行 dense，优先排除
	if LookLikeGoSum(s) && !hasGoModModuleDirective(s) {
		return false
	}
	// Gradle Version Catalog 含 `module = "..."`，不能当 go.mod
	lowFull := strings.ToLower(s)
	if strings.Contains(lowFull, "[libraries]") &&
		(strings.Contains(lowFull, "[versions]") || strings.Contains(lowFull, "module")) {
		return false
	}
	return hasGoModModuleDirective(s) || hasGoModRequireDirective(s)
}

// hasGoModModuleDirective 识别 go.mod 的 `module path` 行（排除 `module =` 赋值）。
func hasGoModModuleDirective(text string) bool {
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if !strings.HasPrefix(line, "module ") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "module "))
		if rest == "" || strings.HasPrefix(rest, "=") {
			continue
		}
		return true
	}
	return false
}

// hasGoModRequireDirective 识别 go.mod 的 require 指令。
func hasGoModRequireDirective(text string) bool {
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "require (" || strings.HasPrefix(line, "require (") {
			return true
		}
		if strings.HasPrefix(line, "require ") {
			rest := strings.TrimSpace(strings.TrimPrefix(line, "require "))
			// 排除偶然出现的 require = ...
			if rest != "" && !strings.HasPrefix(rest, "=") {
				return true
			}
		}
	}
	return false
}

// LookLikeGoSum 粗判是否为 go.sum（hash 行）。
func LookLikeGoSum(text string) bool {
	s := strings.TrimSpace(text)
	if s == "" {
		return false
	}
	lines := 0
	hits := 0
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		lines++
		parts := strings.Fields(line)
		if len(parts) >= 3 && strings.HasPrefix(parts[1], "v") && (strings.HasPrefix(parts[2], "h1:") || strings.Contains(parts[1], "/go.mod")) {
			hits++
		}
	}
	return lines > 0 && hits*2 >= lines
}

// ParseGoMod 解析 require 块与单行 require（不含 replace/exclude）。
func ParseGoMod(text string) (refs []ModuleRef, skipped []string) {
	seen := map[string]struct{}{}
	inRequire := false
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "module ") || strings.HasPrefix(line, "go ") || strings.HasPrefix(line, "toolchain ") {
			continue
		}
		if line == "require (" {
			inRequire = true
			continue
		}
		if inRequire {
			if line == ")" {
				inRequire = false
				continue
			}
			ref, ok := parseRequireLine(line)
			if !ok {
				skipped = append(skipped, line)
				continue
			}
			key := ref.Path + "@" + ref.Version
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			refs = append(refs, ref)
			continue
		}
		if strings.HasPrefix(line, "require ") {
			rest := strings.TrimSpace(strings.TrimPrefix(line, "require "))
			if rest == "(" {
				inRequire = true
				continue
			}
			ref, ok := parseRequireLine(rest)
			if !ok {
				skipped = append(skipped, line)
				continue
			}
			key := ref.Path + "@" + ref.Version
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			refs = append(refs, ref)
			continue
		}
		if strings.HasPrefix(line, "replace ") || strings.HasPrefix(line, "exclude ") || strings.HasPrefix(line, "retract ") {
			skipped = append(skipped, line)
		}
	}
	return refs, skipped
}

// ParseGoSum 解析 go.sum，去重 module@version（忽略 /go.mod 后缀行的重复 path）。
func ParseGoSum(text string) (refs []ModuleRef, skipped []string) {
	seen := map[string]struct{}{}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 3 {
			skipped = append(skipped, line)
			continue
		}
		path, ver := parts[0], parts[1]
		ver = strings.TrimSuffix(ver, "/go.mod")
		if !strings.HasPrefix(ver, "v") {
			skipped = append(skipped, line)
			continue
		}
		key := path + "@" + ver
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		refs = append(refs, ModuleRef{Path: path, Version: ver})
	}
	return refs, skipped
}

// ParseModuleRef 解析 module@version（预取队列项）。
func ParseModuleRef(raw string) (ModuleRef, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "#") {
		return ModuleRef{}, false
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return ModuleRef{}, false
	}
	i := strings.LastIndex(raw, "@")
	if i <= 0 || i == len(raw)-1 {
		return ModuleRef{}, false
	}
	path, ver := raw[:i], raw[i+1:]
	if !strings.HasPrefix(ver, "v") {
		return ModuleRef{}, false
	}
	if !strings.Contains(path, ".") && !strings.Contains(path, "/") {
		return ModuleRef{}, false
	}
	return ModuleRef{Path: path, Version: ver}, true
}

func parseRequireLine(line string) (ModuleRef, bool) {
	line = strings.TrimSpace(line)
	line = strings.TrimSuffix(line, " // indirect")
	line = strings.TrimSpace(line)
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return ModuleRef{}, false
	}
	path, ver := fields[0], fields[1]
	if strings.HasPrefix(ver, "=>") {
		return ModuleRef{}, false
	}
	if !strings.HasPrefix(ver, "v") {
		return ModuleRef{}, false
	}
	return ModuleRef{Path: path, Version: ver}, true
}
