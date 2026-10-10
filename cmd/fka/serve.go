package main

import (
	"context"
	"fmt"
	"os"

	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/channels/ilink"
)

// runServe 常驻：接渠道、收消息、跑问答。
//
// ## 与 ask 的区别
//
// `ask` 跑一轮就退，所以不需要渠道、不需要订阅、不需要信号处理。`serve` 三样都要——
// 而**顺序是硬要求**：必须先订阅再开收，反过来会有一个丢消息的窗口（渠道一开收就
// 可能来消息，那时还没有订阅者）。这个顺序由 app.Serve 保证。
func runServe(ctx context.Context, parsed cliArgs) int {
	application := build(ctx, channelProviders()...)
	defer application.Close()

	// MCP 预热放在起渠道**之前**：渠道一开收就可能来消息，而那时模型还没连上，
	// 第一条消息会平白多花一个连接往返
	application.WarmMcp(ctx)

	if err := application.Serve(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "服务起不来："+err.Error())
		return exitFail
	}
	return exitOK
}

// channelProviders 这个安装要接的渠道种类。
//
// ## 为什么是代码而不是配置文件
//
// Node 版用 `cordis.yml` 的 `providers` 一行列模块路径。这里是构造函数链，
// **加一个渠道 = 在这个切片里多一个 provider**——改完就编译过，不需要重启才知道
// 配错了。这正是当初不用 cordis.yml 的理由之一。
//
// 接缝（`internal/channels`）不认识任何实现，**这里是装配根唯一认识它们的地方**。
func channelProviders() []channels.Provider {
	return []channels.Provider{ilink.NewProvider()}
}
