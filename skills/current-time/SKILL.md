---
name: current-time
description: 获取当前日期与时间（本地时区，另附 UTC 与星期）。回答「现在几点」「今天几号」「今天星期几」这类问题时用它，不要凭猜。
---

# 当前时间

本技能自带一个 Python 脚本 `scripts/now.py`，打印当前日期与时间（本地时区，另附一行 UTC）。

## 用法

用 `python3` 运行脚本：

```
python3 scripts/now.py
```

常用变体：

```
python3 scripts/now.py --tz Asia/Shanghai   # 指定时区
python3 scripts/now.py --utc                # UTC
python3 scripts/now.py --format "%Y-%m-%d"  # 自定义格式
```

按脚本输出回答，**不要自己推算或编造**日期、时间、星期。用户问「明天」「上周」这类
相对时间时，先拿到当前日期再据此推算，并说明以脚本输出为准。

## 注意

- 脚本只读系统时钟、不联网、不写盘，纯 Python 标准库。
- 脚本显示的是运行环境的时钟与时区；用户关心特定时区时用 `--tz`。
