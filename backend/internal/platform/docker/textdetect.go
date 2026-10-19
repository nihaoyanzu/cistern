package docker

import (
	"strings"

	dockerhandler "github.com/nihaoyanzu/cistern/internal/handlers/docker"
	"github.com/nihaoyanzu/cistern/internal/platform"
)

const textDetectPriorityDocker = 60

func (p *DockerPlatform) TextDetectPriority() int { return textDetectPriorityDocker }

func (p *DockerPlatform) LookLikePrefetchText(text string) bool {
	return dockerhandler.LookLikeCompose(text) || dockerhandler.LookLikeImageList(text)
}

func (p *DockerPlatform) ParsePrefetchText(text string) (platform.TextDetectResult, bool) {
	if dockerhandler.LookLikeCompose(text) {
		refs, skip := dockerhandler.ParseComposeImages(text)
		items := imageRefItems(refs)
		if len(items) == 0 {
			return platform.TextDetectResult{Skipped: skip, Kind: "docker_compose"}, false
		}
		return platform.TextDetectResult{Items: items, Skipped: skip, Kind: "docker_compose"}, true
	}
	if !dockerhandler.LookLikeImageList(text) {
		return platform.TextDetectResult{}, false
	}
	refs, skip := dockerhandler.ParseImageList(text)
	items := imageRefItems(refs)
	if len(items) == 0 {
		return platform.TextDetectResult{Skipped: skip, Kind: "docker_image"}, false
	}
	return platform.TextDetectResult{Items: items, Skipped: skip, Kind: "docker_image"}, true
}

func imageRefItems(refs []dockerhandler.ImageRef) []string {
	items := make([]string, 0, len(refs))
	for _, ref := range refs {
		if strings.HasPrefix(ref.Tag, "sha256:") {
			items = append(items, ref.Repo+"@"+ref.Tag)
		} else {
			items = append(items, ref.Repo+":"+ref.Tag)
		}
	}
	return items
}
