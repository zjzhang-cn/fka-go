# fka-go 的开发入口。
#
# ## 为什么是 Makefile 而不是 npm scripts / justfile
#
# Go 项目自带 `go build` / `go test`，**再套一层任务运行器只是换一种写法**。
# 而这里真正需要固化的不是「怎么编译」（一行 go build 谁都会），是**三件容易
# 漏、漏了要花很久才发现的事**：
#
#   1. **零 CGO**。这是硬约束（`decisions.md` 有一条专门讲它），而漏了不会有
#      任何报错——只会产出一个在别的机器上跑不起来的二进制。所以每个构建目标
#      都显式带 CGO_ENABLED=0，而不是靠环境变量。
#   2. **先 fmt-check 再 vet 再 test**。顺序错了的话，vet 会在 gofmt 之前就
#      报一堆噪音，把真正的问题埋掉。
#   3. **gofmt -l 的输出为空才算过**。`gofmt -w` 顺手改了文件还返回 0，
#      所以「跑一下 gofmt」根本检查不出格式问题。
#
# ## 目标分三层
#
#   make            = help，列出全部
#   make dev-*      日常开发（build / test / fmt / smoke）
#   make verify     **提交前的闸门**，也是 CI 该跑的那一条
#   make real-*     需要真机 / 真账号的

.DEFAULT_GOAL := help
SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

# ── 变量 ────────────────────────────────────────────────

# 安装根。`.env` / `data/` / `logs/` / `skills/` / `mcp.json` 全按它解析。
#
# 默认取仓库根——静态二进制的天然锚点：不设 FKA_HOME 时程序会退化成
# 「可执行文件所在目录」，那会让 `bin/fka` 把配置读成 `bin/.env`，
# 而你在仓库根找 `.env` 找不到。
FKA_HOME ?= $(CURDIR)
export FKA_HOME

BIN        := $(FKA_HOME)/bin
FKA        := $(BIN)/fka
FKA_MEMORY := $(BIN)/fka-memory

# 零 CGO 硬约束。**每个构建目标都显式带上**，不靠外部环境变量——
# 靠环境变量的话，一次 `CGO_ENABLED=1 make` 就悄悄破了这个约束。
export CGO_ENABLED := 0

GO      ?= go
GOFLAGS ?=

# 冒烟用的临时安装根与输出。**必须隔离**：冒烟要写 .env / data / logs，
# 而那些是你的真实数据。
smoke_dir     = $(CURDIR)/.smoke
smoke_home    = $(smoke_dir)/home
smoke_log     = $(smoke_dir)/serve.log

.DEFAULT_GOAL := help

# 颜色只在 TTY 下用，否则重定向进文件时满屏转义序列
BOLD  := $(shell [ -t 1 ] && printf '\033[1m' || printf '')
DIM   := $(shell [ -t 1 ] && printf '\033[2m' || printf '')
RESET := $(shell [ -t 1 ] && printf '\033[0m' || printf '')

.PHONY: help
help: ## 列出全部目标
	@echo "$(BOLD)fka-go$(RESET)  $(DIM)FKA_HOME=$(FKA_HOME)$(RESET)"
	@echo
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| sort \
		| awk 'BEGIN {FS = ":.*?## "} {printf "  $(BOLD)%-16s$(RESET) %s\n", $$1, $$2}'
	@echo
	@echo "$(DIM)真机验证见 docs/real-machine-test.md（未离线覆盖的部分）$(RESET)"

# ── 版本 ────────────────────────────────────────────────
#
# **能从二进制里查出它是哪个提交的**，比什么都重要：线上出问题时第一句话就是
# 「你跑的是哪一版」，而答不上来就只能靠猜。
#
# 取不到 git 信息时**照常编**（`VERSION=dev`）——构建不该因为没有 .git 而失败，
# 那会让打包环境（通常是没有历史的一份源码）编不出来。
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# -s -w 去符号表与调试信息（体积约 -25%）；**不牺牲任何运行时能力**。
# -trimpath 去掉编译机的绝对路径——否则二进制里嵌着你家目录名，
# 而 panic 栈会把它打出来。
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(DATE)
GOFLAGS_BUILD := -trimpath -ldflags "$(LDFLAGS)"

# ── 构建 ────────────────────────────────────────────────

.PHONY: build
build: $(FKA) $(FKA_MEMORY) ## 构建全部可执行文件（零 CGO）
	@echo "$(BOLD)✓$(RESET) $(FKA)  $(DIM)$(VERSION)$(RESET)"
	@echo "$(BOLD)✓$(RESET) $(FKA_MEMORY)  $(DIM)$(VERSION)$(RESET)"

$(FKA): $(shell find cmd internal -name '*.go' 2>/dev/null) go.mod go.sum
	@mkdir -p $(BIN)
	$(GO) build $(GOFLAGS) $(GOFLAGS_BUILD) -o $@ ./cmd/fka

# 记忆 MCP server。**它默认建自己的库**（<FKA_HOME>/data/memory.sqlite），
# 所以 `make build` 之后它不需要任何额外配置就能被 mcp.json 拉起来。
$(FKA_MEMORY): $(shell find mcp -name '*.go' 2>/dev/null) go.mod go.sum
	@mkdir -p $(BIN)
	$(GO) build $(GOFLAGS) $(GOFLAGS_BUILD) -o $@ ./mcp/memory

.PHONY: rebuild
rebuild: ## 强制重建（改了依赖之后用）
	@rm -f $(FKA) $(FKA_MEMORY)
	@$(MAKE) --no-print-directory build

.PHONY: version
version: ## 版本信息（进二进制的那份）
	@echo "version    $(VERSION)"
	@echo "commit     $(COMMIT)"
	@echo "buildDate  $(DATE)"

# ── 交叉编译 ────────────────────────────────────────────
#
# 零 CGO 换来的好处：**一个 Go 工具链就能出全平台产物**，不需要在每种机器上
# 各编一遍。纯 Go 依赖（mark3labs / modernc.org/sqlite）让这件事真的成立——
# 只要有一个包要 cgo，darwin/linux 之外的目标就全废了。
TARGETS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64

.PHONY: release
release: ## 交叉编译全部平台到 dist/（带 sha256）
	@rm -rf dist; mkdir -p dist
	@for target in $(TARGETS); do \
		goos=$${target%/*}; goarch=$${target#*/}; \
		out=dist/fka-$${goos}-$${goarch}; \
		if [ "$$goos" = windows ]; then out=$${out}.exe; fi; \
		printf '  %-22s' "$$goos/$$goarch"; \
		GOOS=$$goos GOARCH=$$goarch CGO_ENABLED=0 \
			$(GO) build $(GOFLAGS_BUILD) -o $$out ./cmd/fka || exit 1; \
		GOOS=$$goos GOARCH=$$goarch CGO_ENABLED=0 \
			$(GO) build $(GOFLAGS_BUILD) -o $${out%.exe}-memory \
			./mcp/memory || exit 1; \
		echo "✓"; \
	done
	@cd dist && shasum -a 256 ./* > SHA256SUMS
	@echo "$(BOLD)✓$(RESET) $$(ls dist | grep -c -v SHA256SUMS) 个产物 + SHA256SUMS"

# ── 安装 ────────────────────────────────────────────────
#
# 装到 PREFIX 而不是就地覆盖：**装出去的 fka 与仓库里的源码是两个东西**，
# 而 FKA_HOME 默认取「可执行文件所在目录」——把二进制拷进仓库根会让
# `.env` / `data/` / `logs/` 的解析位置悄悄变掉。
PREFIX ?= /usr/local

.PHONY: install
install: build ## 装到 PREFIX（默认 /usr/local）
	@install -d $(DESTDIR)$(PREFIX)/bin
	@install -m 0755 $(FKA) $(FKA_MEMORY) $(DESTDIR)$(PREFIX)/bin/
	@echo "$(BOLD)✓$(RESET) 装到 $(DESTDIR)$(PREFIX)/bin"
	@echo "  注意：fka 默认拿可执行文件所在目录当安装根，"
	@echo "       所以配置请放 $(DESTDIR)$(PREFIX)/bin/$(BOLD).env$(RESET)，或用 FKA_HOME 指过去。"

.PHONY: uninstall
uninstall: ## 从 PREFIX 卸掉
	@rm -f $(DESTDIR)$(PREFIX)/bin/fka $(DESTDIR)$(PREFIX)/bin/fka-memory
	@echo "$(BOLD)✓$(RESET) 已卸载（配置与 data 保留）")

# ── 闸门 ────────────────────────────────────────────────

.PHONY: fmt
fmt: ## 就地改格式
	@gofmt -w .

# fmt-check 单列的原因：**`gofmt -w` 顺手改完还返回 0**，
# 所以「跑一下 gofmt 检查格式」是查不出问题的。必须用 -l 看输出。
.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -l . 2>/dev/null); \
	if [ -n "$$unformatted" ]; then \
		echo "$(BOLD)格式不对：$(RESET)"; echo "$$unformatted"; \
		echo "跑 make fmt"; exit 1; \
	fi

.PHONY: vet
vet: ## go vet
	$(GO) vet ./...

.PHONY: test
test: ## 全部测试
	$(GO) test ./...

.PHONY: test-race
test-race: ## 带竞态检测的测试。**并发是这里最容易出错的地方**
	$(GO) test -race ./...

.PHONY: test-count
test-count: ## 跑两遍。**抓测试之间的顺序依赖与共享状态**
	$(GO) test -count=2 ./...

.PHONY: cover
cover: ## 覆盖率
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

# verify 是**提交前的闸门**。顺序是有意的：
# fmt → vet → test → build。先 fmt 是因为后两者的报错里会混进格式噪音。
.PHONY: verify
verify: fmt-check vet test build smoke ## 提交前跑这一条就够
	@echo
	@echo "$(BOLD)✓ 全部通过$(RESET)"

# ci 比 verify 多两层，都是**单靠一次跑看不出来**的：
#   - race  并发（长轮询 goroutine、会话状态表）——竞态只在特定时序下出现
#   - count=2  测试之间的顺序依赖与共享状态
.PHONY: ci
ci: verify test-race test-count
	@echo "$(BOLD)✓ CI 通过$(RESET)"

# ── 冒烟（不碰网络）────────────────────────────────────

# smoke 验证**装配这一层**真的接上了。分两半：
#
#   1. 空配置：服务该**明确说**「没有接上渠道」并以 1 退出，而不是安静地
#      收不到任何消息。
#   2. 装好：skills 与 MCP server 该真的出现在 `fka tools` 里——**两条能力
#      来源各验一次**。
#
# 刻意**不要求 LLM**：它验的是「装好了没有」，不是「模型答不答得出」。
# 答得好不好是 real-check 的事。
.PHONY: smoke
smoke: build ## 冒烟：空配置与装好两种情况下都该表现正确
	@rm -rf $(smoke_dir); mkdir -p $(smoke_dir)
	@echo "$(BOLD)── 1. 空配置 ──$(RESET)"
	@# **落盘再截前 3 行，而不是 `fka tools | head -3`**：recipe 开着 pipefail，
	@# 而 head 读够就退出，fka tools 写剩余部分时收到 SIGPIPE（141）——
	@# 整条 smoke 于是挂在「打印工具列表」上，跟渠道接没接上毫无关系。
	@$(FKA) tools > $(smoke_dir)/tools.txt
	@head -3 $(smoke_dir)/tools.txt
	@# **结构化输出不许被日志污染**：`--json` 的第一个字节必须是 `{`。
	@# 默认控制台级别一旦退回 info/debug，日志就插到 JSON 前面，而**退出码仍是 0**
	@# ——调用方只看到「JSON 解析失败」，看不出是日志干的。这一步是那条的闸门。
	@$(FKA) tools --json > $(smoke_dir)/tools.json
	@if [ "$$(head -c 1 $(smoke_dir)/tools.json)" = "{" ]; then \
		echo "$(BOLD)✓$(RESET) tools --json 从 JSON 开头，没被日志污染"; \
	else \
		echo "$(BOLD)✗$(RESET) tools --json 被日志污染了，第一行是："; \
		head -1 $(smoke_dir)/tools.json; exit 1; \
	fi
	@echo
	@# **只读子命令零副作用**：`version` 连日志目录都不该建。`config.Log()` 是惰性
	@# 构造（import 不产生 IO），而 CLI 里那句「定下控制台级别」会把它唤醒——纯输出
	@# 命令必须在那之前走掉。这条只能在**独立进程**里验：同一个进程里 logger 一旦
	@# 建过就不会再建，用例之间会互相掩盖。
	@rm -rf $(smoke_dir)/version-home; mkdir -p $(smoke_dir)/version-home
	@FKA_HOME=$(smoke_dir)/version-home $(FKA) version >/dev/null
	@if [ -e $(smoke_dir)/version-home/logs ]; then \
		echo "$(BOLD)✗$(RESET) fka version 建了 logs/ ——只读子命令不该有副作用"; exit 1; \
	else \
		echo "$(BOLD)✓$(RESET) fka version 零副作用（没建 logs/）"; \
	fi
	@echo
	@echo "$(BOLD)── 2. 参数认错该以 2 退出，而不是混进问题 ──$(RESET)"
	@# **不用模型就能验**，而它守的正是最容易静默失效的一件事：参数被当问题送进提示词
	@# （模型照样答得好好的，只是把 --session 复述了一遍）
	@set +e; $(FKA) ask --sesion aabbcc 问 >$(smoke_log) 2>&1; code=$$?; set -e; \
		if [ $$code -eq 2 ] && grep -q "认不出的参数" $(smoke_log); then \
			echo "$(BOLD)✓$(RESET) 认不出的参数以 2 退出并说清候选"; \
		else \
			echo "$(BOLD)✗$(RESET) 退出码 $$code（参数认错该是 2），或信息里没有「认不出的参数」"; \
			cat $(smoke_log); exit 1; \
		fi
	@echo
	@echo "$(BOLD)── 3. 空配置下 serve 该明确说没有渠道并以 1 退出 ──$(RESET)"
	@set +e; $(FKA) serve >$(smoke_log) 2>&1; code=$$?; set -e; \
		if [ $$code -eq 1 ]; then \
			echo "$(BOLD)✓$(RESET) $$(tail -1 $(smoke_log))"; \
		else \
			echo "$(BOLD)✗$(RESET) 退出码 $$code（无账号时该是 1）"; \
			cat $(smoke_log); exit 1; \
		fi
	@echo
	@echo "$(BOLD)── 4. 装上技能与 MCP server 后该看得到工具 ──$(RESET)"
	@$(MAKE) --no-print-directory smoke-wiring
	@echo
	@echo "$(BOLD)── 5. 记忆 server 的端到端：记一条、跨进程查回来、越权查不到 ──$(RESET)"
	@$(MAKE) --no-print-directory smoke-memory

# JSON-RPC 报文。stdio 传输**按行分帧**，所以一行一条。
#
# `2025-11-25` 是 SDK 的 legacy 版本（仍走 initialize 握手），与 agent 侧
# `internal/tools/mcp/client.go` 报的是同一个——不是随手写的字符串。
mem_init   = {"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}
mem_inited = {"jsonrpc":"2.0","method":"notifications/initialized"}
mem_remember = {"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"remember_memory","arguments":{"content":"冒烟记忆","viewer_wxid":"wx-smoke","visibility":"private"}}}
mem_search_owner = {"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_memories","arguments":{"query":"冒烟","viewer_wxid":"wx-smoke"}}}
mem_search_other = {"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"search_memories","arguments":{"query":"冒烟","viewer_wxid":"wx-other"}}}

# smoke-memory 记忆 server 的**端到端**闸门：A 进程记、B 进程查回来、再验权限边界。
#
# ## 为什么必须是两个进程
#
# 那才验到「落库」——落盘、下次启动读得到。同一个进程内记得住是另一回事，
# 而「重启后记忆还在吗」恰恰是这个 server 存在的全部意义。
#
# ## 为什么用协议而不是直接读 SQLite
#
# 那才验到 JSON-RPC 那条路：工具名、**参数名**、结果文本。而且不必依赖 `sqlite3`
# 这个外部命令——闸门不该因为缺一个工具就整条不跑。
#
# ## ⚠️ stdin 不能喂完就关
#
# mcp-go 在输入流结束时**取消请求上下文**，边写边关会让 INSERT 以
# `context canceled` 失败（实测）。所以最后 `sleep` 一下再关——这是这一条闸门里
# 唯一不显然的地方。
.PHONY: smoke-memory
smoke-memory: build
	@rm -rf $(smoke_dir)/memory; mkdir -p $(smoke_dir)/memory
	@{ printf '%s\n' '$(mem_init)' '$(mem_inited)' '$(mem_remember)'; sleep 2; } \
		| $(FKA_MEMORY) --db $(smoke_dir)/memory/memory.sqlite \
		> $(smoke_dir)/memory/remember.json 2> $(smoke_dir)/memory/remember.err
	@if grep -q "已记住" $(smoke_dir)/memory/remember.json && \
			grep -q "只有你自己能问到" $(smoke_dir)/memory/remember.json; then \
		echo "$(BOLD)✓$(RESET) 写一条 private：$$(grep -o '已记住[^"]*' $(smoke_dir)/memory/remember.json)"; \
	else \
		echo "$(BOLD)✗$(RESET) 记忆没写进去（或 visibility 没照实落）："; \
		cat $(smoke_dir)/memory/remember.json; \
		tail -5 $(smoke_dir)/memory/remember.err; exit 1; \
	fi
	@{ printf '%s\n' '$(mem_init)' '$(mem_inited)' '$(mem_search_owner)' '$(mem_search_other)'; sleep 2; } \
		| $(FKA_MEMORY) --db $(smoke_dir)/memory/memory.sqlite \
		> $(smoke_dir)/memory/search.json 2> $(smoke_dir)/memory/search.err
	@if grep '"id":3' $(smoke_dir)/memory/search.json | grep -q "冒烟记忆"; then \
		echo "$(BOLD)✓$(RESET) 另一个进程查回来了（真的落库了）"; \
	else \
		echo "$(BOLD)✗$(RESET) 另一个进程查不到："; cat $(smoke_dir)/memory/search.json; exit 1; \
	fi
	@if grep '"id":4' $(smoke_dir)/memory/search.json | grep -q "没有找到"; then \
		echo "$(BOLD)✓$(RESET) 别人查不到（权限过滤在真实链路上生效）"; \
	else \
		echo "$(BOLD)✗$(RESET) 越权查到了别人的记忆——这是数据泄漏："; \
		grep '"id":4' $(smoke_dir)/memory/search.json; exit 1; \
	fi
	@if [ "$$(head -c 1 $(smoke_dir)/memory/search.json)" = "{" ]; then \
		echo "$(BOLD)✓$(RESET) stdout 只有 JSON-RPC 帧，日志没混进来"; \
	else \
		echo "$(BOLD)✗$(RESET) stdout 被日志污染了，协议流就此报废："; \
		head -1 $(smoke_dir)/memory/search.json; exit 1; \
	fi

# smoke-wiring 单独跑第 4 步。**这是能力链路唯一的自动闸门**——
# 「技能读到了吗」「MCP server 连上了吗」「mcp.json 里的 cwd 生效了吗」这三件事，
# 静默失败时从界面上都看不出来：工具列表就是空的，而你没法区分「没配」与
# 「配了但没生效」。
#
# 库路径故意给**相对**的 `cwd-probe/memory.sqlite`：fka-memory 按自己的 cwd
# 解析它，而 cwd 由 mcp.json 的 `cwd` 决定。所以最后那条断言查的是「库落在
# <安装根> 里」——**cwd 没生效时工具照样列得出来**（子进程只是跑在了 make
# 所在的仓库根，把库写在那儿），不查文件就等于没验。这条检查之所以要有牙齿，
# 是因为 `cwd` 失效的表现是**静默写错地方**，不是报错。
.PHONY: smoke-wiring
smoke-wiring: build
	@rm -rf $(smoke_home)
	@mkdir -p $(smoke_home)/bin $(smoke_home)/skills/echo $(smoke_home)/cwd-probe
	@cp $(FKA) $(FKA_MEMORY) $(smoke_home)/bin/
	@printf -- '---\nname: 回声\ndescription: 复述输入\n---\n原样复述一遍。\n' \
		> $(smoke_home)/skills/echo/SKILL.md
	@printf '{"mcpServers":{"memory":{"command":"%s","args":["--db","cwd-probe/memory.sqlite"],"cwd":"."}}}' \
		"$(smoke_home)/bin/fka-memory" > $(smoke_home)/mcp.json
	@FKA_HOME=$(smoke_home) LLM_TOOL_EFFECTS=read,external \
		$(smoke_home)/bin/fka tools > $(smoke_home)/out.txt 2>&1 || true
	@cat $(smoke_home)/out.txt
	@for want in skills__load skills__list mcp__memory__search_memories mcp__memory__remember_memory; do \
		if grep -q "$$want" $(smoke_home)/out.txt; then \
			echo "$(BOLD)✓$(RESET) $$want"; \
		else \
			echo "$(BOLD)✗$(RESET) 少了 $$want —— 技能目录、mcp.json 或 mcp.json 里的 cwd 没生效"; \
			exit 1; \
		fi; \
	done
	@if [ -f $(smoke_home)/cwd-probe/memory.sqlite ]; then \
		echo "$(BOLD)✓$(RESET) mcp.json 的 cwd 生效了（子进程的工作目录 = 安装根）"; \
	else \
		echo "$(BOLD)✗$(RESET) 库不在 <安装根>/cwd-probe/ —— mcp.json 里的 cwd 没生效"; \
		exit 1; \
	fi
	@rm -rf $(smoke_home)
	@echo "$(BOLD)✓$(RESET) 两条能力来源都接上了"

# ── 日常使用 ────────────────────────────────────────────

.PHONY: login
login: build ## 扫码登录 iLink（凭证写进 <FKA_HOME>/.env，权限 0600）
	$(FKA) login $(LOGIN_ARGS)

.PHONY: serve
serve: build ## 常驻：接渠道、收消息、跑问答
	$(FKA) serve

.PHONY: ask
ask: build ## 无头问一句：make ask QUESTION="…"
	@test -n "$(QUESTION)" || { echo "用法：make ask QUESTION=\"...\""; exit 2; }
	$(FKA) ask $(if $(PRINCIPAL),--principal $(PRINCIPAL),) "$(QUESTION)"

.PHONY: tools
tools: build ## 列出模型现在能看到的工具
	$(FKA) tools

# ── 真机（需要真实微信账号）─────────────────────────────

# real-check 是**提交前该跑但跑不了**的那一步的占位：
# 它明确告诉人「这里还没验过」，而不是让人以为 verify 通过就等于全对。
.PHONY: real-check
real-check: ## 真机检查（需要已登录的账号）
	@if [ ! -f "$(FKA_HOME)/.env" ]; then \
		echo "还没有 .env。先跑 make login"; exit 2; \
	fi
	@if ! grep -q 'ILINK_ACCOUNT_.*_BOT_TOKEN=..' "$(FKA_HOME)/.env"; then \
		echo ".env 里没有已登录的账号。跑 make login"; exit 2; \
	fi
	@echo "$(BOLD)开始真机验证。步骤与每步该看到什么：$(RESET)"
	@echo "  docs/real-machine-test.md"
	@echo
	@echo "$(BOLD)下一步：$(RESET)开另一个终端跑 make serve，"
	@echo "然后用微信给这个 bot 发一条纯文字消息。"

# ── 清理 ────────────────────────────────────────────────

.PHONY: clean
clean: ## 删构建产物与覆盖率文件（**不碰 .env / data** —— 那是你的数据）
	rm -f $(FKA) $(FKA_MEMORY) coverage.out
	rm -rf $(FKA_HOME)/logs $(smoke_dir)

.PHONY: distclean
distclean: clean ## 连本机数据一起删（**会丢记忆与游标**）
	@echo "$(BOLD)这会删掉 $(FKA_HOME)/data —— 记忆库与游标都在里面。$(RESET)"
	@read -p "确认？输入 yes 继续：" ok; [ "$$ok" = yes ] || { echo "已取消"; exit 1; }
	rm -rf $(FKA_HOME)/data

# ── 依赖 ────────────────────────────────────────────────

.PHONY: tidy
tidy: ## 整理依赖
	$(GO) mod tidy

.PHONY: why
why: ## 看某个包为什么在：make why PKG=modernc.org/sqlite
	@test -n "$(PKG)" || { echo "用法：make why PKG=<包名>"; exit 2; }
	$(GO) mod why $(PKG)
