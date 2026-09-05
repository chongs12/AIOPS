// aiops 服务入口：serve（默认）启动 HTTP 服务；probe 执行环境连通性自检。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"aiops/pkg/config"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "probe":
			runProbe(os.Args[2:])
			return
		case "serve":
			if err := runServe(os.Args[2:]); err != nil {
				slog.Error("服务退出", "err", err)
				os.Exit(1)
			}
			return
		case "help", "-h", "--help":
			usage()
			return
		default:
			fmt.Fprintf(os.Stderr, "未知子命令 %q\n", os.Args[1])
			usage()
			os.Exit(2)
		}
	}
	if err := runServe(nil); err != nil {
		slog.Error("服务退出", "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`aiops —— AIOps 运维工单多智能体系统

用法:
  aiops serve [flags]   启动服务（默认）
  aiops probe [flags]   环境连通性自检（Prometheus/Jaeger/K8s/Redis/PG/OTLP 回环）

通用 flags:
  -config string   配置文件路径（默认 configs/config.yaml）`)
}

// loadConfig 加载配置并对缺失的环境占位符输出告警（不阻塞启动）。
func loadConfig(path string) (*config.Config, error) {
	cfg, missing, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		slog.Warn("以下环境变量未设置，对应占位符已置空", "vars", missing)
	}
	return cfg, nil
}

// newSignalContext 返回随 SIGINT/SIGTERM 取消的 context。
func newSignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
