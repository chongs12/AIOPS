// Package web 内嵌聊天式工单页静态资源。
package web

import "embed"

//go:embed static
var Static embed.FS
