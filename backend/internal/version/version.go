// Package version 保存构建期注入的版本信息。
package version

// Version 当前版本号（无 v 前缀）。构建时用 ldflags 注入，例如：
//
//	-X github.com/livehl/mirrorhub/internal/version.Version=2.4
//
// 未注入时为 "dev"。
var Version = "dev"

// Commit 可选 git commit 短哈希，构建时注入。
var Commit = ""
