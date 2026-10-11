package docker

import (
	"regexp"
	"strings"
)

var composeImageLineRe = regexp.MustCompile(`(?i)^\s*image:\s*['"]?([^'"\s#]+)['"]?\s*(?:#.*)?$`)

// LookLikeCompose 粗判 docker-compose / compose.yaml（含 services 与 image:）。
func LookLikeCompose(text string) bool {
	s := strings.TrimSpace(text)
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "{") {
		return false
	}
	low := strings.ToLower(s)
	if strings.Contains(low, "lockfileversion") {
		return false
	}
	hasServices := strings.Contains(low, "services:")
	hasImage := composeImageLineRe.MatchString(s) || strings.Contains(low, "\n    image:") || strings.Contains(low, "\n  image:")
	return hasServices && hasImage
}

// ParseComposeImages 从 compose 文件提取 image: 引用。
func ParseComposeImages(text string) (refs []ImageRef, skipped []string) {
	seen := map[string]struct{}{}
	for _, raw := range strings.Split(text, "\n") {
		m := composeImageLineRe.FindStringSubmatch(raw)
		if m == nil {
			continue
		}
		val := strings.TrimSpace(m[1])
		val = strings.Trim(val, `"'`)
		if val == "" || strings.Contains(val, "${") {
			skipped = append(skipped, strings.TrimSpace(raw))
			continue
		}
		ref, ok := ParseImageRef(val)
		if !ok {
			skipped = append(skipped, val)
			continue
		}
		key := ref.Repo + "@" + ref.Tag
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		refs = append(refs, ref)
	}
	return refs, skipped
}
