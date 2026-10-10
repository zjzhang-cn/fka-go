#!/usr/bin/env node
/**
 * 最小的 MCP 服务示例：提供「获取当前时间」工具。
 *
 * ## 它是什么
 *
 * 一个走 **stdio** 的 MCP 服务器——FKA 的 `tools/mcp/` 用 `StdioClientTransport`
 * 把它当子进程拉起，握手后把它导出的工具挂成 `mcp__time_demo__get_current_time`
 * 交给模型（工具一律 `effect: 'external'`，要 `LLM_TOOL_EFFECTS=read,external`）。
 *
 * ## 三个要点
 *
 * 1. **stdout 是协议通道**：只能用 `console.error` 打日志，`console.log` 会污染
 *    JSON-RPC 报文，服务端和客户端都会解析失败。
 * 2. **工具清单 = description + inputSchema（JSON Schema）**：模型只看得到这两样，
 *    写清楚「这个工具干什么、参数怎么填」。
 * 3. **业务错误用 `isError: true` 回**，不要把异常抛出去——那会中断整条连接。
 *
 * 运行：node mcp_demo/server.mjs
 * 依赖：@modelcontextprotocol/sdk（仓库根 node_modules 已有，无需另装）
 */
import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import { z } from 'zod';

const SERVER_INFO = { name: 'demo', version: '0.1.0' };
const TOOL_CURRENT_TIME = "current_time";
/** 这个服务对外暴露的工具表。模型看到的描述就是这里的 description。 */
/** 取当前时间并格式化成一段给模型看的文字。时区非法时抛错，由调用方转成 isError。 */
function currentTime(timeZone) {
  const now = new Date();
  const options = { dateStyle: 'full', timeStyle: 'long' };
  if (timeZone !== undefined) options.timeZone = timeZone;

  // Intl 对非法时区会抛 RangeError，正好当作参数校验
  const label = timeZone ?? '本地时区';
  const text = new Intl.DateTimeFormat('zh-CN', options).format(now);

  return `${text}（${label}）\nISO 8601: ${now.toISOString()}\nUnix 毫秒: ${now.getTime()}`;
}

const server = new McpServer(SERVER_INFO);

server.registerTool(
  TOOL_CURRENT_TIME,
  {
    description:
      '获取当前时间。可传 IANA 时区名（如 Asia/Shanghai、America/New_York）换算到指定时区；不传则用服务器本地时区。',
    inputSchema: {
      timeZone: z
        .string()
        .optional()
        .describe('IANA 时区名，例如 Asia/Shanghai；不传用本地时区。'),
    },
  },
  async (args = {}) => {
  try {
    return { content: [{ type: 'text', text: currentTime(args.timeZone) }] };
  } catch (error) {
    const detail = error instanceof Error ? error.message : String(error);
    return { isError: true, content: [{ type: 'text', text: `取时间失败：${detail}` }] };
  }
  },
);

await server.connect(new StdioServerTransport());
console.error(`[${SERVER_INFO.name}] MCP 服务已启动（stdio）`);
