"""打印当前日期与时间：默认按系统本地时区，另附一行 UTC。

用法：
    python3 now.py [--utc] [--tz <IANA 时区名>] [--format <strftime 格式>]

只用 Python 标准库，不联网、不写盘。
"""

import argparse
from datetime import datetime, timezone


def format_offset(now):
    offset = now.strftime("%z")
    if len(offset) == 5:
        return "UTC" + offset[:3] + ":" + offset[3:]
    return "UTC"


def main():
    parser = argparse.ArgumentParser(description="打印当前日期与时间")
    parser.add_argument("--utc", action="store_true", help="按 UTC 显示")
    parser.add_argument("--tz", default="", help="IANA 时区名，如 Asia/Shanghai")
    parser.add_argument("--format", default="", help="strftime 格式串")
    args = parser.parse_args()

    if args.tz:
        try:
            from zoneinfo import ZoneInfo

            now = datetime.now(ZoneInfo(args.tz))
        except Exception as err:
            print("时区不可用：" + str(err))
            return 1
    elif args.utc:
        now = datetime.now(timezone.utc)
    else:
        now = datetime.now().astimezone()

    if args.format:
        print(now.strftime(args.format))
        return 0

    tzname = now.tzname() or ""
    stamp = now.strftime("%Y-%m-%d %H:%M:%S")
    print("%s %s (%s)" % (stamp, tzname, format_offset(now)))
    print("UTC: " + datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M:%S"))
    print("星期：" + "一二三四五六日"[now.weekday()])
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
