package goproxy

import (
	goproxyhandler "github.com/nihaoyanzu/cistern/internal/handlers/goproxy"
	"github.com/nihaoyanzu/cistern/internal/platform"
)

const textDetectPriorityGoproxy = 90

func (p *GoproxyPlatform) TextDetectPriority() int { return textDetectPriorityGoproxy }

func (p *GoproxyPlatform) LookLikePrefetchText(text string) bool {
	return goproxyhandler.LookLikeGoSum(text) || goproxyhandler.LookLikeGoMod(text)
}

func (p *GoproxyPlatform) ParsePrefetchText(text string) (platform.TextDetectResult, bool) {
	if goproxyhandler.LookLikeGoSum(text) {
		refs, skip := goproxyhandler.ParseGoSum(text)
		items := make([]string, 0, len(refs))
		for _, ref := range refs {
			items = append(items, ref.Path+"@"+ref.Version)
		}
		if len(items) == 0 {
			return platform.TextDetectResult{Skipped: skip, Kind: "go_sum"}, false
		}
		return platform.TextDetectResult{Items: items, Skipped: skip, Kind: "go_sum"}, true
	}
	if goproxyhandler.LookLikeGoMod(text) {
		refs, skip := goproxyhandler.ParseGoMod(text)
		items := make([]string, 0, len(refs))
		for _, ref := range refs {
			items = append(items, ref.Path+"@"+ref.Version)
		}
		if len(items) == 0 {
			return platform.TextDetectResult{Skipped: skip, Kind: "go_mod"}, false
		}
		return platform.TextDetectResult{Items: items, Skipped: skip, Kind: "go_mod"}, true
	}
	return platform.TextDetectResult{}, false
}
