package npm

import (
	"net/http"
	"strings"

	npmhandler "github.com/nihaoyanzu/cistern/internal/handlers/npm"
)

func (p *NPMPlatform) IndexCacheVariant(accept string) string {
	return npmhandler.IndexCacheVariant(accept)
}

func (p *NPMPlatform) PrepareIndexHeaders(clientAccept string, headers http.Header) {
	if npmhandler.PrefersAbbreviated(clientAccept) {
		headers.Set("Accept", "application/vnd.npm.install-v1+json")
	} else if strings.TrimSpace(headers.Get("Accept")) == "" {
		headers.Set("Accept", "application/json")
	}
}

func (p *NPMPlatform) DetectIndexContentType(upstreamCT, accept string, body []byte) string {
	return npmhandler.DetectJSONContentType(upstreamCT, accept, body)
}
