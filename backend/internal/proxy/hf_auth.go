package proxy

import (
	"net/http"

	hfhandler "github.com/nihaoyanzu/cistern/internal/handlers/huggingface"
)

func stripXetFromHeader(h http.Header) {
	if h == nil {
		return
	}
	hfhandler.StripXetHeaders(h)
}
