package maven

import (
	mavenhandler "github.com/livehl/mirrorhub/internal/handlers/maven"
	"github.com/livehl/mirrorhub/internal/platform"
)

const textDetectPriorityMaven = 70

func (p *MavenPlatform) TextDetectPriority() int { return textDetectPriorityMaven }

func (p *MavenPlatform) LookLikePrefetchText(text string) bool {
	return mavenhandler.LookLikePom(text) ||
		mavenhandler.LookLikeGradleProperties(text) ||
		mavenhandler.LookLikeGAVList(text)
}

func (p *MavenPlatform) ParsePrefetchText(text string) (platform.TextDetectResult, bool) {
	if mavenhandler.LookLikePom(text) {
		coords, skip := mavenhandler.ParsePomDependencies(text)
		items := make([]string, 0, len(coords))
		for _, c := range coords {
			items = append(items, c.String())
		}
		return platform.TextDetectResult{Items: items, Skipped: skip, Kind: "maven_pom"}, true
	}
	if mavenhandler.LookLikeGradleProperties(text) {
		coords, skip := mavenhandler.ParseGradleProperties(text)
		items := make([]string, 0, len(coords))
		for _, c := range coords {
			items = append(items, c.String())
		}
		return platform.TextDetectResult{Items: items, Skipped: skip, Kind: "maven_gradle_props"}, true
	}
	if mavenhandler.LookLikeGAVList(text) {
		coords, skip := mavenhandler.ParseGAVList(text)
		items := make([]string, 0, len(coords))
		for _, c := range coords {
			items = append(items, c.String())
		}
		return platform.TextDetectResult{Items: items, Skipped: skip, Kind: "maven_gav"}, true
	}
	return platform.TextDetectResult{}, false
}
