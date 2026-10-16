---
name: current-time
description: 获取当前日期与时间（本地时区，另附 UTC 与星期）。回答「现在几点」「今天几号」「今天星期几」这类问题时用它，不要凭猜。
---

# 当前时间

本技能自带一个 Python 脚本，打印运行环境的当前日期与时间。**脚本不在 agent 里执行**，
要按下面步骤落到沙盒里、用沙盒的 `run` 跑。前提是这份安装配了沙盒 MCP server，并且设了
`FKA_SKILL_SINK`（如 `mcp__sandbox__import_skill`）。

## 步骤

1. **落地脚本**：调用沙盒的 `import_skill` 工具，`skill` 填 `current-time`。
   它返回一组**相对沙盒根的路径**（形如 `skills/<技能>/scripts/now.py`）。

2. **运行**（用第 1 步返回的**真实路径**，别自己拼）：

   ```
   python3 <脚本根>/scripts/now.py
   ```

   常用变体：

   ```
   python3 <脚本根>/scripts/now.py --tz Asia/Shanghai   # 指定时区
   python3 <脚本根>/scripts/now.py --utc                # UTC
   python3 <脚本根>/scripts/now.py --format "%Y-%m-%d"  # 自定义格式
   ```

3. **按结果回答**：把脚本输出的日期、时间、星期直接告诉用户，**不要自己推算或编造**。
   用户问「明天」「上周」这类相对时间时，先拿到当前日期再据此推算，并说明是以脚本输出为准。

## 注意

- 脚本只读系统时钟、不联网、不写盘，纯 Python 标准库。
- 脚本显示的是**沙盒运行环境的时钟与时区**（一般就是宿主）。用户关心特定时区时用 `--tz`。
- 路径带哈希（`skills/current-time-xxxxxxxx/`），**每次都以工具返回值为准**。
