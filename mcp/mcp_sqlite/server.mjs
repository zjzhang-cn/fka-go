#!/usr/bin/env node
/**
 * Chinook SQLite 查询 MCP 服务。
 *
 * ## 它是什么
 *
 * 一个走 **stdio** 的 MCP 服务器，把 `chinook.db` 的**每张表**暴露成一个查询工具
 * （`query_album` / `query_artist` / …）。FKA 的 `tools/mcp/` 用 `StdioClientTransport`
 * 把它当子进程拉起，工具以 `mcp__sqlite__query_album` 的形式交给模型
 * （MCP 工具一律 `effect: 'external'`，要 `LLM_TOOL_EFFECTS=read,external`）。
 *
 * ## 三类工具
 *
 * - **表名清单**（`list_tables`）：列出所有用户表名，先看有哪些表。
 * - **按表查询**（`query_<表名>`）：列过滤 + 分页，覆盖大多数「看某张表」的场景；
 *   不认识 JOIN。
 * - **复杂查询**（`query_sql`）：跑任意**只读** SELECT/WITH，支持 JOIN / GROUP BY /
 *   聚合 / 子查询 / 窗口函数；`describe_schema` 先给出表结构与外键，便于拼复杂 SQL。
 *
 * ## 三个要点
 *
 * 1. **stdout 是协议通道**：只能用 `console.error` 打日志，`console.log` 会污染
 *    JSON-RPC 报文，服务端和客户端都会解析失败。
 * 2. **数据库只读**：`DatabaseSync` 以 `readOnly` 打开；按表查询的 SQL 全部走参数
 *    占位符、表名列名只从解析结果来；`query_sql` 只放行单条 SELECT/WITH，写入由
 *    只读连接兜底。
 * 3. **业务错误用 `isError: true` 回**，不要把异常抛出去——那会中断整条连接。
 *
 * ## 表结构从哪来
 *
 * `sqlite.sql` 是权威来源；文件缺失时退回 `PRAGMA table_info` 反射实际库结构。
 * 所以换了 `chinook.db` 或改了建表脚本，工具清单会跟着变。
 *
 * 运行：node mcp_sqlite/server.mjs
 * 依赖：node:sqlite（Node 内置）+ @modelcontextprotocol/sdk（仓库根 node_modules 已有）
 */
import { readFileSync } from 'node:fs';
import { DatabaseSync } from 'node:sqlite';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import { z } from 'zod';

const HERE = dirname(fileURLToPath(import.meta.url));

const SERVER_INFO = { name: 'sqlite', version: '0.1.0' };
const DB_PATH = process.env.CHINOOK_DB_PATH?.trim() || join(HERE, 'chinook.db');
const SQL_PATH = join(HERE, 'sqlite.sql');
const DEFAULT_LIMIT = 20;
const MAX_LIMIT = 200;
const MAX_CELL = 40;
const DEFAULT_SQL_LIMIT = 100;
const MAX_SQL_LIMIT = 1000;

/** 数值列的类型；其余一律当文本。 */
const NUMERIC_TYPES = new Set(['INTEGER', 'NUMERIC', 'REAL', 'FLOAT', 'DOUBLE']);

const IDENT = /^[A-Za-z_][A-Za-z0-9_]*$/;

/**
 * 从 `sqlite.sql` 解析 `CREATE TABLE` 的列。
 * 返回 `{ table, columns: [{ name, numeric }] }[]`；解析不出任何表时返回 `null`。
 */
function parseSchemaSql(sql) {
	const tables = [];
	const createRe = /CREATE\s+TABLE\s+\[?(\w+)\]?\s*\(([\s\S]*?)\n\s*\)\s*;/gi;

	for (const match of sql.matchAll(createRe)) {
		const table = match[1];
		const columns = [];
		const seen = new Set();

		for (const rawLine of match[2].split('\n')) {
			const line = rawLine.trim().replace(/,$/, '');
			if (line.length === 0) continue;

			// 约束行不是列定义（含外键末尾独立成行的 ON DELETE / ON UPDATE）
			if (/^(CONSTRAINT|PRIMARY|FOREIGN|UNIQUE|CHECK|ON)\b/i.test(line)) continue;

			const col = line.match(/^\[?(\w+)\]?\s+([A-Za-z]+)/);
			if (!col || seen.has(col[1])) continue;

			seen.add(col[1]);
			columns.push({ name: col[1], numeric: NUMERIC_TYPES.has(col[2].toUpperCase()) });
		}

		if (columns.length > 0) tables.push({ table, columns });
	}

	return tables.length > 0 ? tables : null;
}

/** 建表脚本缺失/解析不出时，反射实际库结构。 */
function introspectSchema(db) {
	const names = db
		.prepare("SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name")
		.all();

	return names
		.filter((row) => !row.name.startsWith('sqlite_'))
		.map((row) => {
			const columns = db.prepare(`PRAGMA table_info("${row.name}")`).all();
			return {
				table: row.name,
				columns: columns.map((col) => ({
					name: col.name,
					numeric: /INT|REAL|FLOA|DOUB|NUMERIC|DECIMAL/i.test(col.type ?? ''),
				})),
			};
		});
}

/** 读取表结构：优先 `sqlite.sql`，退回库反射。 */
function readSchema(db) {
	try {
		const parsed = parseSchemaSql(readFileSync(SQL_PATH, 'utf-8'));
		if (parsed) return parsed;
	} catch (error) {
		console.error(
			`[${SERVER_INFO.name}] 读不了 ${SQL_PATH}，改用库结构反射：${error instanceof Error ? error.message : String(error)
			}`
		);
	}
	return introspectSchema(db);
}

/** 单元格转成一行里的文字，过长截断。 */
function cellText(value) {
	if (value === null || value === undefined) return 'NULL';
	const text = typeof value === 'string' ? value : String(value);
	return text.length > MAX_CELL ? `${text.slice(0, MAX_CELL - 1)}…` : text;
}

/** 把行渲染成对齐的文本表，宽列也会被截断，方便模型读。 */
function renderTable(columns, rows) {
	const cells = rows.map((row) => columns.map((col) => cellText(row[col])));
	const widths = columns.map((col, i) =>
		Math.max(col.length, ...cells.map((row) => row[i].length), 1)
	);
	const line = (values) => values.map((v, i) => v.padEnd(widths[i])).join(' | ');

	return [
		line(columns),
		widths.map((w) => '-'.repeat(w)).join('-+-'),
		...cells.map(line),
	].join('\n');
}

/** 校验并引用一个标识符——表名/列名只来自解析结果，仍不放松检查。 */
function quoteIdent(ident, known) {
	if (!IDENT.test(ident) || !known.has(ident)) {
		throw new Error(`未知的列或表：${ident}`);
	}
	return `"${ident}"`;
}

const db = new DatabaseSync(DB_PATH, { readOnly: true });
const schema = readSchema(db);
const server = new McpServer(SERVER_INFO);

/** 每个 SQL 工具都复用的查询实现。 */
function queryTable(spec, args) {
	const known = new Set(spec.columns.map((col) => col.name));
	const numeric = new Set(spec.columns.filter((c) => c.numeric).map((c) => c.name));

	const { limit, offset, orderBy, order, columns: wants, ...filters } = args ?? {};

	const selected = Array.isArray(wants) && wants.length > 0 ? wants : spec.columns.map((c) => c.name);
	const selectList = selected.map((name) => quoteIdent(name, known)).join(', ');

	const whereParts = [];
	const params = [];
	for (const [name, value] of Object.entries(filters)) {
		if (value === undefined) continue;
		if (typeof value === 'object' && value !== null) {
			// { like: '%foo%' } / { gt: 5 } / { in: [...] } 这几种写法
			const ops = Object.entries(value);
			if (ops.length !== 1) throw new Error(`列 ${name} 的过滤条件只支持一个操作符`);
			const [op, operand] = ops[0];
			const col = quoteIdent(name, known);

			switch (op) {
				case 'like':
					whereParts.push(`${col} LIKE ?`);
					params.push(String(operand));
					break;
				case 'gt':
				case 'gte':
				case 'lt':
				case 'lte': {
					const symbol = { gt: '>', gte: '>=', lt: '<', lte: '<=' }[op];
					whereParts.push(`${col} ${symbol} ?`);
					params.push(operand);
					break;
				}
				case 'in': {
					if (!Array.isArray(operand) || operand.length === 0) {
						throw new Error(`列 ${name} 的 in 需要一个非空数组`);
					}
					whereParts.push(`${col} IN (${operand.map(() => '?').join(', ')})`);
					params.push(...operand);
					break;
				}
				default:
					throw new Error(`列 ${name} 不支持的操作符：${op}`);
			}
			continue;
		}

		// 标量 = 等值比较；文本列允许 % 通配，等价于 LIKE
		if (!numeric.has(name) && typeof value === 'string' && value.includes('%')) {
			whereParts.push(`${quoteIdent(name, known)} LIKE ?`);
			params.push(value);
		} else {
			whereParts.push(`${quoteIdent(name, known)} = ?`);
			params.push(value);
		}
	}

	const where = whereParts.length > 0 ? ` WHERE ${whereParts.join(' AND ')}` : '';
	const table = `"${spec.table}"`;

	const total = db.prepare(`SELECT COUNT(*) AS n FROM ${table}${where}`).get(...params).n;

	const rows = db
		.prepare(
			`SELECT ${selectList} FROM ${table}${where}` +
			(orderBy ? ` ORDER BY ${quoteIdent(orderBy, known)} ${order === 'desc' ? 'DESC' : 'ASC'}` : '') +
			' LIMIT ? OFFSET ?'
		)
		.all(...params, limit, offset);

	const header = `表 ${spec.table}：共 ${total} 行，本页返回 ${rows.length} 行` +
		(orderBy ? `（按 ${orderBy} ${order === 'desc' ? '降序' : '升序'}）` : '');

	if (rows.length === 0) return `${header}\n（没有符合条件的行）`;
	return `${header}\n${renderTable(selected, rows)}`;
}

/** 只读 SQL 的准入：单条语句 + SELECT/WITH 开头。写入最终由只读连接兜底。 */
function assertReadOnlySql(sql) {
	const text = String(sql ?? '').trim();
	if (text.length === 0) throw new Error('sql 不能为空');

	const body = text.replace(/;\s*$/, '');
	if (body.includes(';')) throw new Error('一次只允许执行一条语句');
	if (!/^(SELECT|WITH)\b/i.test(body)) {
		throw new Error('只允许 SELECT / WITH 开头的只读查询');
	}

	return body;
}

/** `bigint` 不能直接 JSON 化，转成字符串。 */
function jsonReplacer(_key, value) {
	return typeof value === 'bigint' ? value.toString() : value;
}

/** 跑一条只读 SQL，最多取 `limit` 行。行数达上限时标记截断。 */
function runSql(sql, binds, limit) {
	const stmt = db.prepare(assertReadOnlySql(sql));
	const rows = [];
	let truncated = false;

	for (const row of stmt.iterate(...binds)) {
		if (rows.length >= limit) {
			truncated = true;
			break;
		}
		rows.push(row);
	}

	const header = `SQL 查询：返回 ${rows.length} 行` +
		(truncated ? `（达到上限 ${limit}，可能还有更多，请加 LIMIT/OFFSET 或聚合）` : '');
	if (rows.length === 0) return `${header}\n（没有结果）`;

	return `${header}\n${rows.map((row) => JSON.stringify(row, jsonReplacer)).join('\n')}`;
}

/** 从库里反射完整结构：列（类型/PK/NOT NULL）+ 外键。 */
function introspectFull(db) {
	const tables = db
		.prepare(
			"SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name"
		)
		.all();

	return tables.map(({ name }) => {
		const quoted = `"${String(name).replace(/"/g, '""')}"`;
		const columns = db.prepare(`PRAGMA table_info(${quoted})`).all().map((col) => ({
			name: col.name,
			type: col.type || '',
			notNull: col.notnull === 1,
			primaryKey: col.pk > 0,
		}));
		const foreignKeys = db.prepare(`PRAGMA foreign_key_list(${quoted})`).all().map((fk) => ({
			column: fk.from,
			table: fk.table,
			referencesColumn: fk.to,
		}));

		return { table: name, columns, foreignKeys };
	});
}

/** 把结构渲染成给模型看的一行行文本。 */
function renderSchema(entries) {
	const lines = [];

	for (const entry of entries) {
		lines.push(`## ${entry.table}`);
		lines.push(
			`列：${entry.columns
				.map((col) => {
					const flags = [col.primaryKey ? 'PK' : '', col.notNull ? 'NOT NULL' : '']
						.filter(Boolean)
						.join(' ');
					return `${col.name} ${col.type}${flags ? ` [${flags}]` : ''}`.trim();
				})
				.join('；')}`
		);
		if (entry.foreignKeys.length > 0) {
			lines.push(
				`外键：${entry.foreignKeys
					.map((fk) => `${fk.column} → ${fk.table}(${fk.referencesColumn || 'PK'})`)
					.join('；')}`
			);
		}
		lines.push('');
	}

	return lines.join('\n').trimEnd();
}
// 列出表名工具（list_tables）注册
if (1) {
	server.registerTool(
		'list_tables',
		{
			title: '列出全部表名',
			description:
				'返回库里所有用户表的名称（按字母序）。不确定有哪些表、或想先看看再决定查哪张时用它。',
			inputSchema: {},
		},
		async () => {
			try {
				const names = introspectFull(db).map((entry) => entry.table);
				const text =
					names.length === 0 ? '（库里没有任何用户表）' : `共 ${names.length} 张表：\n${names.join('\n')}`;
				return { content: [{ type: 'text', text }] };
			} catch (error) {
				const detail = error instanceof Error ? error.message : String(error);
				return { isError: true, content: [{ type: 'text', text: `列出表名失败：${detail}` }] };
			}
		}
	);
};
// 表结构查看工具（describe_schema）注册
if (1) {
	server.registerTool(
		'describe_schema',
		{
			title: '查看表结构与外键',
			description:
				'返回 SQLite 库的表结构：每张表的列（类型 / PK / NOT NULL）与外键关系。' +
				'写复杂 SQL（JOIN / GROUP BY）前先用它确认字段与外键；可传 table 只看一张表。',
			inputSchema: {
				table: z.string().optional().describe('只看这张表；不传返回全部表'),
			},
		},
		async (args = {}) => {
			try {
				const all = introspectFull(db);
				const wanted = args.table
					? all.filter((entry) => entry.table.toLowerCase() === String(args.table).toLowerCase())
					: all;

				if (wanted.length === 0) throw new Error(`没有叫 ${args.table} 的表`);
				return { content: [{ type: 'text', text: renderSchema(wanted) }] };
			} catch (error) {
				const detail = error instanceof Error ? error.message : String(error);
				return { isError: true, content: [{ type: 'text', text: `查看表结构失败：${detail}` }] };
			}
		}
	);
};
// SQL 读
server.registerTool(
	'query_sql',
	{
		title: '执行只读 SQL（复杂查询）',
		description:
			'对 SQLite 库执行一条**只读** SQL（SELECT 或 WITH 开头），支持 JOIN / GROUP BY / 聚合 / ' +
			'子查询 / 窗口函数。值请用 ? 占位符并在 params 里按顺序给，别拼字符串。' +
			'例：SELECT a.Name, COUNT(t.TrackId) n FROM Artist a JOIN Album al ON al.ArtistId=a.ArtistId ' +
			'JOIN Track t ON t.AlbumId=al.AlbumId GROUP BY a.ArtistId ORDER BY n DESC LIMIT 5',
		inputSchema: {
			sql: z.string().describe('一条 SELECT / WITH 只读查询；不要带多条语句或结尾分号以外的分号'),
			params: z
				.array(z.union([z.string(), z.number(), z.null()]))
				.optional()
				.describe('按顺序替换 sql 里的 ? 占位符；没有则省略'),
			limit: z.number().int().min(1).max(MAX_SQL_LIMIT).optional()
				.describe(`最多返回行数，默认 ${DEFAULT_SQL_LIMIT}，最大 ${MAX_SQL_LIMIT}`),
		},
	},
	async (args = {}) => {
		try {
			return {
				content: [{
					type: 'text',
					text: runSql(args.sql, args.params ?? [], args.limit ?? DEFAULT_SQL_LIMIT),
				}],
			};
		} catch (error) {
			const detail = error instanceof Error ? error.message : String(error);
			return { isError: true, content: [{ type: 'text', text: `SQL 查询失败：${detail}` }] };
		}
	}
);
// 按表查询工具（query_开头）注册，调试用，默认关闭
if (0) {
	for (const spec of schema) {
		const toolName = `query_${spec.table.toLowerCase()}`;
		const columnList = spec.columns
			.map((col) => `${col.name}(${col.numeric ? '数值' : '文本'})`)
			.join('、');

		const shape = {};

		// 每列既可传标量（等值；文本可含 % 等价 LIKE），也可传操作符对象
		const operators = z.object({
			like: z.string().optional(),
			gt: z.number().optional(),
			gte: z.number().optional(),
			lt: z.number().optional(),
			lte: z.number().optional(),
			in: z.array(z.union([z.string(), z.number()])).optional(),
		});

		for (const col of spec.columns) {
			const scalar = col.numeric ? z.number() : z.string();
			shape[col.name] = z
				.union([scalar, operators])
				.optional()
				.describe(
					`按 ${col.name} 过滤；标量为等值（文本可含 % 通配），或 {like}/{gt}/{gte}/{lt}/{lte}/{in}`
				);
		}

		const columnNames = spec.columns.map((col) => col.name);
		shape.limit = z.number().int().min(1).max(MAX_LIMIT).optional()
			.describe(`返回行数，默认 ${DEFAULT_LIMIT}，最大 ${MAX_LIMIT}`);
		shape.offset = z.number().int().min(0).optional().describe('跳过的行数，默认 0');
		shape.orderBy = z.enum(columnNames).optional().describe('排序列');
		shape.order = z.enum(['asc', 'desc']).optional().describe('排序方向，默认 asc');
		shape.columns = z.array(z.enum(columnNames)).optional().describe('只返回这些列，默认全部');

		server.registerTool(
			toolName,
			{
				title: `查询 ${spec.table}`,
				description:
					`查询 SQLite 数据库的 ${spec.table} 表。列：${columnList}。` +
					'标量参数为等值过滤（文本可含 % 通配），对象参数支持 {like}/{gt}/{gte}/{lt}/{lte}/{in}。',
				inputSchema: shape,
			},
			async (args = {}) => {
				try {
					return {
						content: [{
							type: 'text',
							text: queryTable(spec, {
								...args,
								limit: args.limit ?? DEFAULT_LIMIT,
								offset: args.offset ?? 0,
							}),
						}],
					};
				} catch (error) {
					const detail = error instanceof Error ? error.message : String(error);
					return { isError: true, content: [{ type: 'text', text: `查询 ${spec.table} 失败：${detail}` }] };
				}
			}
		);
	}
};

await server.connect(new StdioServerTransport());
console.error(
	`[${SERVER_INFO.name}] MCP 服务已启动（stdio）：${DB_PATH}，` +
	`${schema.length} 张表 → ${schema.length} 个按表查询 + list_tables / query_sql / describe_schema`
);
