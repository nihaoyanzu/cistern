package npm

import (
	npmhandler "github.com/livehl/mirrorhub/internal/handlers/npm"
	"github.com/livehl/mirrorhub/internal/platform"
)

const textDetectPriorityNPM = 100

func (p *NPMPlatform) TextDetectPriority() int { return textDetectPriorityNPM }

func (p *NPMPlatform) LookLikePrefetchText(text string) bool {
	return npmhandler.LookLikeLockfile(text) || npmhandler.LookLikePackageJSON(text)
}

func (p *NPMPlatform) ParsePrefetchText(text string) (platform.TextDetectResult, bool) {
	if npmhandler.LookLikeLockfile(text) {
		parsed, skip := npmhandler.ParseLockfile(text)
		return platform.TextDetectResult{Items: parsed, Skipped: skip, Kind: "npm_lock"}, true
	}
	if npmhandler.LookLikePackageJSON(text) {
		refs, skip := npmhandler.ParsePackageJSON(text)
		items := make([]string, 0, len(refs))
		for _, r := range refs {
			items = append(items, r.Spec())
		}
		return platform.TextDetectResult{Items: items, Skipped: skip, Kind: "npm_package_json"}, true
	}
	return platform.TextDetectResult{}, false
}
