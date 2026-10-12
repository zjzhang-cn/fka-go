#!/usr/bin/env node
/**
 * 最小的 MCP 服务示例：一个 tool + 一个 resource + 一个 resource 模板 + 一个 prompt。
 *
 * ## 它是什么
 *
 * 一个走 **stdio** 的 MCP 服务器——FKA 的 `tools/mcp/` 把它当子进程拉起，握手后把它
 * 声明的原语挂成工具交给模型：
 *
 *   - tools     → `mcp__time_demo__current_time`
 *   - resources → `mcp__time_demo__list_resources` / `__read_resource`
 *   - prompts   → `mcp__time_demo__list_prompts` / `__get_prompt`
 *
 * resources / prompts 的合成工具是 **agent 侧按能力探测生成**的（见 internal/tools/mcp
 * 的 primitives.go）：这里只要声明了，客户端就能看到；没声明的服务器不会多出这些工具。
 * 所有 MCP 工具一律 `effect: external`，要 `LLM_TOOL_EFFECTS=read,external`。
 *
 * ## 三种原语各是什么（本示例各给一个）
 *
 * 1. **tool**：模型主动调用的动作，有入参、有返回值。这里 `current_time` 取当前时间。
 * 2. **resource**：服务端可读的数据，靠 uri 定位，模型「读」而不是「调」。
 *    - 固定资源 `time://now`：随时读都返回当下时间。
 *    - 资源模板 `time://zone/{zone}`：一个模式覆盖多份数据；`list` 回调告诉客户端
 *      有哪些实例，读的时候客户端把 `{zone}` 填好传进来。
 * 3. **prompt**：服务端预置的提示词模板，取出来是一段会话（messages），可带参数。
 *    这里 `time_report` 生成一条「请播报某地时间」的 user 消息。
 *
 * ## 三个要点
 *
 * 1. **stdout 是协议通道**：只能用 `console.error` 打日志，`console.log` 会污染
 *    JSON-RPC 报文，服务端和客户端都会解析失败。
 * 2. **清单即契约**：工具看 description + inputSchema；资源看 name + uri + mimeType；
 *    提示词看 name + 参数。模型只看得到这些，写清楚「干什么、参数怎么填」。
 * 3. **业务错误别抛异常**：工具用 `isError: true` 回，资源/提示词同理（抛出去会中断连接）。
 *
 * 运行：node mcp_time/server.mjs
 * 依赖：@modelcontextprotocol/sdk（本目录 node_modules 已有，无需另装）
 */
import { McpServer, ResourceTemplate } from '@modelcontextprotocol/sdk/server/mcp.js';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import { z } from 'zod';

const SERVER_INFO = { name: 'time-demo', version: '0.2.0' };
const TOOL_CURRENT_TIME = 'current_time';

/** 资源模板列出的「已知时区」——只是给客户端的样例，任意 IANA 名都能读。 */
const KNOWN_ZONES = ['Asia/Shanghai', 'America/New_York', 'Europe/London', 'UTC'];

/** URI 模板里的变量是 `string | string[]`，统一成一个字符串。 */
function firstString(value) {
  return Array.isArray(value) ? value[0] : value;
}

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

// ── tool ────────────────────────────────────────────────
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

// ── resource：固定 uri，读一次得到当下时间 ───────────────
server.registerResource(
  '当前时间',
  'time://now',
  { description: '服务器本地时区的当前时间（纯文本）。', mimeType: 'text/plain' },
  async (uri) => ({
    contents: [{ uri: uri.href, mimeType: 'text/plain', text: currentTime() }],
  }),
);

// ── resource 模板：一个模式覆盖任意时区，list 告诉客户端有哪些实例 ──
server.registerResource(
  '指定时区的时间',
  new ResourceTemplate('time://zone/{+zone}', {
    // 变量用 `{+zone}` 而不是 `{zone}`：IANA 名里带 `/`（Asia/Shanghai），
    // 普通 `{zone}` 按 RFC 6570 不匹配 `/`，读取会被判成「资源不存在」。
    // list 是必需的：没有它，客户端「列资源」时看不到模板下的任何实例。
    list: async () => ({
      resources: KNOWN_ZONES.map((zone) => ({
        uri: `time://zone/${zone}`,
        name: zone,
        mimeType: 'text/plain',
        description: `${zone} 的当前时间`,
      })),
    }),
    // 可选：客户端补全 zone 变量时用（不是所有客户端都会问）。
    complete: {
      zone: (value) =>
        KNOWN_ZONES.filter((zone) => zone.toLowerCase().startsWith((value ?? '').toLowerCase())),
    },
  }),
  { description: '把当前时间换算到某个 IANA 时区（如 Asia/Shanghai）。', mimeType: 'text/plain' },
  async (uri, variables) => ({
    contents: [
      { uri: uri.href, mimeType: 'text/plain', text: currentTime(firstString(variables.zone)) },
    ],
  }),
);

// ── prompt：预置提示词模板，取出是一段会话（可带参数）────────
server.registerPrompt(
  'time_report',
  {
    description: '生成一段「请播报某地当前时间」的提示词，可指定时区与语气。',
    argsSchema: {
      timeZone: z.string().optional().describe('IANA 时区名；不传用本地时区。'),
      tone: z.enum(['简洁', '正式', '俏皮']).optional().describe('播报语气，默认简洁。'),
    },
  },
  ({ timeZone, tone }) => ({
    messages: [
      {
        role: 'user',
        content: {
          type: 'text',
          text:
            `请用${tone ?? '简洁'}的语气播报 ${timeZone ?? '本地时区'} 的当前时间。` +
            '先用 current_time 工具取时间，再组织成一句话回复。',
        },
      },
    ],
  }),
);

await server.connect(new StdioServerTransport());
console.error(`[${SERVER_INFO.name}] MCP 服务已启动（stdio）：1 tool / 2 resources（含 1 模板）/ 1 prompt`);
