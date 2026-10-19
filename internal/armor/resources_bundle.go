package armor

import "embed"

// 本地研究资料与公开发行内容分开；发布工具生成不含研究正文的同名文件。
const DistributionProfile = "research"
const bundledPresetPattern = "assets/presets/*.md"

//go:embed assets/library.json assets/presets/*.md
var bundledResources embed.FS
