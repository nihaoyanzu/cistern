package docker

import (
	"strings"
)

// BearerChallenge 解析自 WWW-Authenticate: Bearer realm=...,service=...,scope=...
type BearerChallenge struct {
	Realm   string
	Service string
	Scope   string
}

// ParseBearerChallenge 从响应头解析 Bearer 质询；找不到则 ok=false。
func ParseBearerChallenge(h map[string][]string) (BearerChallenge, bool) {
	if h == nil {
		return BearerChallenge{}, false
	}
	vals := h["Www-Authenticate"]
	if len(vals) == 0 {
		vals = h["WWW-Authenticate"]
	}
	for _, raw := range vals {
		if c, ok := parseOneBearerChallenge(raw); ok {
			return c, true
		}
	}
	return BearerChallenge{}, false
}

func parseOneBearerChallenge(raw string) (BearerChallenge, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return BearerChallenge{}, false
	}
	low := strings.ToLower(raw)
	if !strings.HasPrefix(low, "bearer ") {
		return BearerChallenge{}, false
	}
	rest := strings.TrimSpace(raw[len("bearer "):])
	var c BearerChallenge
	for _, part := range splitAuthParams(rest) {
		k, v, ok := splitAuthKV(part)
		if !ok {
			continue
		}
		switch strings.ToLower(k) {
		case "realm":
			c.Realm = v
		case "service":
			c.Service = v
		case "scope":
			c.Scope = v
		}
	}
	if c.Realm == "" {
		return BearerChallenge{}, false
	}
	return c, true
}

func splitAuthParams(s string) []string {
	var parts []string
	var b strings.Builder
	inQuote := false
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inQuote {
			b.WriteByte(ch)
			if ch == quote {
				inQuote = false
			}
			continue
		}
		if ch == '"' || ch == '\'' {
			inQuote = true
			quote = ch
			b.WriteByte(ch)
			continue
		}
		if ch == ',' {
			parts = append(parts, strings.TrimSpace(b.String()))
			b.Reset()
			continue
		}
		b.WriteByte(ch)
	}
	if b.Len() > 0 {
		parts = append(parts, strings.TrimSpace(b.String()))
	}
	return parts
}

func splitAuthKV(part string) (key, val string, ok bool) {
	i := strings.IndexByte(part, '=')
	if i <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(part[:i])
	val = strings.TrimSpace(part[i+1:])
	if len(val) >= 2 {
		if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		}
	}
	if key == "" || val == "" {
		return "", "", false
	}
	return key, val, true
}
