package maven

import (
	"net/http"

	mavenhandler "github.com/nihaoyanzu/cistern/internal/handlers/maven"
)

func (p *MavenPlatform) PackageCacheIdentity(_, _ string, _ http.Header, defaultKey string) (string, string) {
	return "maven:pkg:" + defaultKey, ""
}

func (p *MavenPlatform) DetectIndexContentType(upstreamCT, _ string, body []byte) string {
	return mavenhandler.DetectContentType("", upstreamCT, body)
}
