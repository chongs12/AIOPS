// Package web 内嵌前端构建产物（Vite dist）。
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist 返回以 dist 为根的文件系统。
func Dist() (fs.FS, error) {
	return fs.Sub(dist, "dist")
}
