package huggingface

import (
	hfhandler "github.com/livehl/mirrorhub/internal/handlers/huggingface"
	"github.com/livehl/mirrorhub/internal/platform"
)

const textDetectPriorityHF = 80

func (p *HuggingFacePlatform) TextDetectPriority() int { return textDetectPriorityHF }

func (p *HuggingFacePlatform) LookLikePrefetchText(text string) bool {
	return hfhandler.LookLikeHFRepoList(text)
}

func (p *HuggingFacePlatform) ParsePrefetchText(text string) (platform.TextDetectResult, bool) {
	if !hfhandler.LookLikeHFRepoList(text) {
		return platform.TextDetectResult{}, false
	}
	parsed, skip := hfhandler.ParseRepoList(text)
	if len(parsed) == 0 {
		return platform.TextDetectResult{Skipped: skip, Kind: "hf_repo"}, false
	}
	return platform.TextDetectResult{Items: parsed, Skipped: skip, Kind: "hf_repo"}, true
}
