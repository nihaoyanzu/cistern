package router

type Strategy string

const (
	StrategyProxy    Strategy = "proxy"
	StrategyParallel Strategy = "parallel"
)

type Match struct {
	Platform       string
	Strategy       Strategy
	UpstreamBase   string
	TargetURL      string
	IsIndex        bool
	IsMetadata     bool
	SmallFileBoost bool
	// ReadOnly 为 true 时仅允许 GET/HEAD。
	ReadOnly bool
	// SkipUpstreamHead 交互冷路径跳过上游 HEAD（npm/goproxy/maven 等）。
	// Docker blob / HuggingFace 等需 Content-Length 校验的路径不得设置。
	SkipUpstreamHead bool
}
