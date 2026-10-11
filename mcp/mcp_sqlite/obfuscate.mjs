#!/usr/bin/env node
/**
 * 音乐商店示例库的**名称混淆器**：把 `chinook.original.db` 的表名 / 列名 / 索引名
 * 换成与原名无关的稳定短码，写成 `chinook.db`，并同步渲染 `sqlite.sql` 与
 * `skills/music-catalog-analytics/SKILL.md`。
 *
 * ## 为什么这么做
 *
 * 目的是**测 Text-to-SQL**：模型若能从预训练里认出 Chinook 的表列名，测的就不是它
 * 读 schema 写 SQL 的能力，而是它背没背过这份示例库。把名字换成无意义短码，再给一份
 * 「短码 → 业务含义」的字典技能，才测得到真本事。
 *
 * ## 三条不变量（`--check` 会守）
 *
 * 1. **原名一个都不留**——包括 `sqlite_master.sql` 里那段建表语句、以及
 *    `IFK_AlbumArtistId` 这类**索引名**（它自己就把原列名带出来了）。所以索引名也换；
 *    `CONSTRAINT [PK_...]` 这类约束名则**直接删掉**（SQLite 不需要它们）。
 * 2. **数据一个都不动**——值、行数、类型、PK/FK/NOT NULL 全保留；只改名。
 * 3. **可复现**——短码 = `hash(seed + 原名)`，同一 seed 必得同一份库与同一份技能。
 *
 * ## 用法
 *
 *   node obfuscate.mjs [--seed chinook-obf] [--in chinook.original.db]
 *                      [--out chinook.db] [--sql sqlite.sql]
 *                      [--skill ../../skills/music-catalog-analytics/SKILL.md]
 *                      [--map obfuscate.map.json]
 *
 * 每次运行**都会自检**（无原名残留 + 行数一致），不通过就非零退出。
 *
 * `--map` 里带**原名**（code ↔ 含义 ↔ 原名），只给工具/校验用，**不要入库**
 * （.gitignore 已忽略）。技能里只出现业务含义，绝不出现原名。
 */
import { readFileSync, writeFileSync, existsSync, unlinkSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { DatabaseSync } from 'node:sqlite';

const HERE = dirname(fileURLToPath(import.meta.url));

// ── 参数 ────────────────────────────────────────────────────
function parseArgs(argv) {
	const flags = {};
	for (let i = 0; i < argv.length; i++) {
		const token = argv[i];
		if (!token.startsWith('--')) continue;
		const key = token.slice(2);
		const value = argv[i + 1];
		if (value === undefined || value.startsWith('--')) throw new Error(`参数 --${key} 缺值`);
		flags[key] = value;
		i++;
	}
	return flags;
}

const args = parseArgs(process.argv.slice(2));
const SEED = args.seed ?? 'chinook-obf';
const IN_DB = resolve(HERE, args.in ?? 'chinook.original.db');
const OUT_DB = resolve(HERE, args.out ?? 'chinook.db');
const OUT_SQL = resolve(HERE, args.sql ?? 'sqlite.sql');
const OUT_SKILL = resolve(HERE, args.skill ?? '../../skills/music-catalog-analytics/SKILL.md');
const OUT_MAP = resolve(HERE, args.map ?? 'obfuscate.map.json');

// ── 语义字典（生成技能用；只在这份脚本里出现原名）──────────
// 表名 → 业务含义
const TABLE_MEANING = {
	Artist: '歌手 / 表演者',
	Album: '专辑',
	Track: '曲目（歌曲）',
	Genre: '音乐流派',
	MediaType: '媒体格式（编码 / 载体）',
	Playlist: '播放列表（歌单）',
	PlaylistTrack: '歌单与曲目的多对多关联表',
	Customer: '客户',
	Invoice: '发票（订单）',
	InvoiceLine: '发票明细行',
	Employee: '员工',
};

// 列名 → 业务含义（全局双射：同名同义）
const COLUMN_MEANING = {
	Address: '街道地址',
	AlbumId: '专辑 ID',
	ArtistId: '歌手 ID',
	BillingAddress: '账单街道地址',
	BillingCity: '账单城市',
	BillingCountry: '账单国家',
	BillingPostalCode: '账单邮编',
	BillingState: '账单州 / 省',
	BirthDate: '出生日期',
	Bytes: '文件字节数',
	City: '城市',
	Company: '公司',
	Composer: '作曲者',
	Country: '国家',
	CustomerId: '客户 ID',
	Email: '邮箱',
	EmployeeId: '员工 ID',
	Fax: '传真',
	FirstName: '名',
	GenreId: '流派 ID',
	HireDate: '入职日期',
	InvoiceDate: '订单日期',
	InvoiceId: '发票 ID',
	InvoiceLineId: '发票明细行 ID',
	LastName: '姓',
	MediaTypeId: '媒体格式 ID',
	Milliseconds: '时长（毫秒）',
	Name: '名称（含义随所在表而定：歌手名 / 专辑名 / 曲名 / 流派名 / 格式名 / 歌单名）',
	Phone: '电话',
	PlaylistId: '歌单 ID',
	PostalCode: '邮编',
	Quantity: '数量',
	ReportsTo: '上级员工 ID',
	State: '州 / 省',
	SupportRepId: '客服代表（员工）ID',
	Title: '头衔 / 标题（员工表为职务，专辑表为专辑名）',
	Total: '订单总额',
	TrackId: '曲目 ID',
	UnitPrice: '单价',
};

// 外键 → 关系场景（键是 `子表.子列`）
const FK_SCENE = {
	'Album.ArtistId': '专辑属于某歌手',
	'Track.AlbumId': '曲目属于某专辑',
	'Track.GenreId': '曲目属于某流派',
	'Track.MediaTypeId': '曲目属于某格式',
	'PlaylistTrack.PlaylistId': '歌单 ↔ 曲目（多对多）',
	'PlaylistTrack.TrackId': '同上',
	'Invoice.CustomerId': '客户的发票',
	'InvoiceLine.InvoiceId': '发票的明细',
	'InvoiceLine.TrackId': '明细里的曲目',
	'Customer.SupportRepId': '客户的客服',
	'Employee.ReportsTo': '员工上级（自关联）',
};

// 常用模板：写**原名**，渲染时统一替换成短码（别名 g/t/il… 不在映射里，保持不变）
const TEMPLATES = [
	{
		title: '各流派销售额 Top5',
		sql: `SELECT g.Name AS genre, ROUND(SUM(il.UnitPrice*il.Quantity),2) AS revenue
FROM InvoiceLine il JOIN Track t ON t.TrackId=il.TrackId JOIN Genre g ON g.GenreId=t.GenreId
GROUP BY g.GenreId ORDER BY revenue DESC LIMIT 5`,
	},
	{
		title: '歌手曲目数 Top10',
		sql: `SELECT ar.Name AS artist, COUNT(t.TrackId) AS tracks
FROM Artist ar JOIN Album al ON al.ArtistId=ar.ArtistId JOIN Track t ON t.AlbumId=al.AlbumId
GROUP BY ar.ArtistId ORDER BY tracks DESC LIMIT 10`,
	},
	{
		title: '客户消费额 Top5',
		sql: `SELECT c.CustomerId, c.FirstName||' '||c.LastName AS name, ROUND(SUM(i.Total),2) AS total
FROM Customer c JOIN Invoice i ON i.CustomerId=c.CustomerId
GROUP BY c.CustomerId ORDER BY total DESC LIMIT 5`,
	},
	{
		title: '按月销售额',
		sql: `SELECT strftime('%Y-%m', InvoiceDate) AS ym, ROUND(SUM(Total),2) AS total
FROM Invoice GROUP BY ym ORDER BY ym`,
	},
	{
		title: '每张专辑时长最长的曲目',
		sql: `WITH r AS (
  SELECT al.Title AS album, t.Name AS track, t.Milliseconds AS ms,
         ROW_NUMBER() OVER (PARTITION BY al.AlbumId ORDER BY t.Milliseconds DESC) AS rn
  FROM Track t JOIN Album al ON al.AlbumId=t.AlbumId)
SELECT album, track, ms FROM r WHERE rn=1 ORDER BY ms DESC LIMIT 10`,
	},
	{
		title: '员工上下级层级',
		sql: `WITH RECURSIVE org(id,name,mgr,lvl) AS (
  SELECT EmployeeId, FirstName||' '||LastName, ReportsTo, 0 FROM Employee WHERE ReportsTo IS NULL
  UNION ALL SELECT e.EmployeeId, e.FirstName||' '||e.LastName, e.ReportsTo, org.lvl+1
  FROM Employee e JOIN org ON e.ReportsTo=org.id)
SELECT lvl, mgr, name FROM org ORDER BY lvl, mgr`,
	},
	{
		title: '歌单曲目数 Top5',
		sql: `SELECT p.PlaylistId, p.Name AS playlist, COUNT(pt.TrackId) AS tracks
FROM Playlist p JOIN PlaylistTrack pt ON pt.PlaylistId=p.PlaylistId
GROUP BY p.PlaylistId ORDER BY tracks DESC LIMIT 5`,
	},
];

// ── 短码：全局双射 原名 → z_<sha1(seed·name)> ────────────────
function buildCodeMap(names, seed) {
	const taken = new Set();
	const codeOf = new Map();
	for (const name of [...names].sort()) {
		let len = 6;
		let code;
		do {
			code = 'z_' + createHash('sha1').update(`${seed}\u0000${name}`).digest('hex').slice(0, len);
			len++;
		} while (taken.has(code));
		taken.add(code);
		codeOf.set(name, code);
	}
	return codeOf;
}

// 建表语句里的标识符被 `[]` 包着，但正则按裸词匹配即可（`[` 不是词字符，不参与）
const TOKEN = /[A-Za-z_][A-Za-z0-9_]*/g;

function stripConstraintNames(sql) {
	// `CONSTRAINT [PK_Album] ` / `CONSTRAINT PK_Album ` —— 约束名会带出原表名，整段删掉
	return sql.replace(/CONSTRAINT\s+(?:\[[^\]]*\]|"[^"]*"|[A-Za-z_][A-Za-z0-9_]*)\s+/gi, '');
}

function replaceTokens(sql, codeOf) {
	return sql.replace(TOKEN, (token) => codeOf.get(token) ?? token);
}

function obfuscateSql(sql, codeOf) {
	return replaceTokens(stripConstraintNames(sql), codeOf);
}

const quote = (ident) => `"${String(ident).replace(/"/g, '""')}"`;

// ── 读原库结构 ──────────────────────────────────────────────
const src = new DatabaseSync(IN_DB, { readOnly: true });

const tableRows = src
	.prepare("SELECT name, sql FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	.all();

const tables = tableRows.map(({ name, sql }) => ({
	name,
	sql,
	columns: src.prepare(`PRAGMA table_info(${quote(name)})`).all().map((c) => ({
		name: c.name,
		type: c.type,
		notNull: c.notnull === 1,
		pk: c.pk, // 0 = 非主键；>0 是主键里的位置（支持复合主键）
	})),
	foreignKeys: src.prepare(`PRAGMA foreign_key_list(${quote(name)})`).all().map((fk) => ({
		from: fk.from,
		table: fk.table,
		to: fk.to,
	})),
	rowCount: src.prepare(`SELECT COUNT(*) AS n FROM ${quote(name)}`).get().n,
}));

const indexRows = src
	.prepare("SELECT name, sql FROM sqlite_master WHERE type='index' AND sql IS NOT NULL ORDER BY name")
	.all()
	.map(({ name, sql }) => ({ name, sql }));

// ── 建映射 ──────────────────────────────────────────────────
const allNames = new Set();
for (const t of tables) {
	allNames.add(t.name);
	for (const c of t.columns) allNames.add(c.name);
}
for (const idx of indexRows) allNames.add(idx.name);

const codeOf = buildCodeMap(allNames, SEED);

// ── 重建混淆库 ──────────────────────────────────────────────
if (existsSync(OUT_DB)) unlinkSync(OUT_DB);
const out = new DatabaseSync(OUT_DB);
out.exec('PRAGMA foreign_keys = OFF;');
out.exec(`ATTACH DATABASE '${IN_DB.replace(/'/g, "''")}' AS orig;`);

const obfuscatedTableSql = new Map(); // 原名 → 混淆后的 CREATE TABLE
for (const t of tables) {
	const ddl = obfuscateSql(t.sql, codeOf);
	obfuscatedTableSql.set(t.name, ddl);
	out.exec(ddl.endsWith(';') ? ddl : ddl + ';');
}

const obfuscatedIndexSql = [];
for (const idx of indexRows) {
	const ddl = obfuscateSql(idx.sql, codeOf);
	obfuscatedIndexSql.push(ddl);
	out.exec(ddl.endsWith(';') ? ddl : ddl + ';');
}

// 拷数据：按列位置一一对应；列名走同一个映射
for (const t of tables) {
	const newTable = codeOf.get(t.name);
	const newCols = t.columns.map((c) => quote(codeOf.get(c.name))).join(', ');
	const oldCols = t.columns.map((c) => quote(c.name)).join(', ');
	out.exec(`INSERT INTO ${quote(newTable)} (${newCols}) SELECT ${oldCols} FROM orig.${quote(t.name)};`);
}

out.exec('DETACH DATABASE orig;');
out.exec('VACUUM;');

// ── 渲染 sqlite.sql ─────────────────────────────────────────
const sqlFile = [
	'/*******************************************************************************',
	'   本文件由 mcp_sqlite/obfuscate.mjs 生成（表名/列名/索引名为混淆后的短码）。',
	`   seed=${SEED}；源库=chinook.original.db。改回原名的映射只在这份生成器的语义字典里。`,
	'********************************************************************************/',
	'',
	...tables.map((t) => obfuscatedTableSql.get(t.name).replace(/;\s*$/, '') + ';'),
	'',
	...obfuscatedIndexSql.map((ddl) => ddl.replace(/;\s*$/, '') + ';'),
	'',
].join('\n');
writeFileSync(OUT_SQL, sqlFile);

// ── 渲染 SKILL.md ───────────────────────────────────────────
function renderSkill() {
	const lines = [];
	const t = (name) => codeOf.get(name);

	lines.push('---');
	lines.push('name: music-catalog-analytics');
	lines.push(
		'description: 音乐商店（Music Store）关系型数据库的查询技能。**本库的表名与列名已被混淆**' +
		'（如 z_a1b2），本技能是唯一权威的名称字典：把混淆名对应到业务含义、表间关系与常用 SQL 模板。' +
		'适用：跨表联合查询（热销流派、客户消费排行、员工业绩）、资产多维清点（按流派/格式/年度统计曲目）、' +
		'异常排查（无销量的孤儿曲目、未分配客服的客户）。**不要凭记忆臆造表名或列名；不确定就用 ' +
		'describe_schema 核对，一切以本技能与库结构为准。**'
	);
	lines.push('---');
	lines.push('');
	lines.push('# 音乐商店数据库查询指南（混淆标识符）');
	lines.push('');
	lines.push('本库的表名、列名、索引名都是**与含义无关的短码**（`z_` 开头）。');
	lines.push('mcp 工具（`mcp__sqlite__*`）需 `LLM_TOOL_EFFECTS=read,external`：');
	lines.push('');
	lines.push('- `list_tables`：列出全部表名（不确定有哪些表时先看它）。');
	lines.push('- `describe_schema`：查表结构（列 / PK / NOT NULL / 外键），可传 `table` 只看一张表。');
	lines.push('- `query_sql`：一条只读 SELECT/WITH，支持 JOIN / GROUP BY / 聚合 / 子查询 / 窗口 / 递归 CTE；值用 `?` 占位符放进 `params`。');
	lines.push('');
	lines.push('## 执行流程（照做，少走弯路）');
	lines.push('');
	lines.push('1. 用下面的**名称对照**确定要用的表 / 列（都用混淆短码写 SQL）。');
	lines.push('2. 字段或外键不确定就先 `describe_schema`（可只传一张表），**不要臆造短码**。');
	lines.push('3. 跨表用 `query_sql`：每张表起别名，列名带别名前缀（短码在多表之间可能重名）。');
	lines.push('4. 聚合列用 `AS` 起名（返回 JSON 的键就是别名）；加 `LIMIT` 防刷屏。');
	lines.push('5. 报错就按错误里的表名/列名/语法改正后重试；只允许单条 SELECT/WITH。');
	lines.push('');
	lines.push('## 名称对照');
	lines.push('');
	lines.push('记法：`PK` 主键、`FK→表.列` 外键、未标即可空。');
	lines.push('');

	for (const table of tables) {
		const tableCode = codeOf.get(table.name);
		const fkByFrom = new Map(table.foreignKeys.map((fk) => [fk.from, fk]));
		const parts = table.columns.map((col) => {
			const colCode = codeOf.get(col.name);
			const marks = [];
			if (col.pk > 0) marks.push('PK');
			const fk = fkByFrom.get(col.name);
			if (fk) marks.push(`FK→${codeOf.get(fk.table)}.${codeOf.get(fk.to)}`);
			const suffix = marks.length > 0 ? ` [${marks.join(' ')}]` : '';
			return `${colCode}（${COLUMN_MEANING[col.name] ?? '未知'}）${suffix}`;
		});
		lines.push(`- **${tableCode}**（${TABLE_MEANING[table.name] ?? '未知'}）：${parts.join('；')}。`);
	}
	lines.push('');

	lines.push('## 表间关系');
	lines.push('');
	lines.push('| 子表.列 | → 父表.列 | 关系 | 场景 |');
	lines.push('|---|---|---|---|');
	for (const table of tables) {
		for (const fk of table.foreignKeys) {
			const scene = FK_SCENE[`${table.name}.${fk.from}`] ?? '';
			lines.push(
				`| ${codeOf.get(table.name)}.${codeOf.get(fk.from)} | → ${codeOf.get(fk.table)}.${codeOf.get(fk.to)} | N:1 | ${scene} |`
			);
		}
	}
	lines.push('');

	lines.push('## SQL 编写规则（提准）');
	lines.push('');
	lines.push(
		`- **金额**：实际成交额 = ${codeOf.get('UnitPrice')}（单价）× ${codeOf.get('Quantity')}（数量）之和；` +
		`${codeOf.get('Total')}（订单总额）已是整单总额。二者按问题选一个，**不要重复相加**。`
	);
	lines.push(
		`- **重名列**：${codeOf.get('Name')}（名称）在歌手/专辑/曲目/流派/格式/歌单多张表里都有，` +
		`${codeOf.get('UnitPrice')}（单价）在曲目/明细里都有——必须带表别名。`
	);
	lines.push(
		`- **时长**：${codeOf.get('Milliseconds')} 是毫秒，展示秒用 ROUND(${codeOf.get('Milliseconds')}/1000.0, 1)。`
	);
	lines.push(
		`- **日期**：${codeOf.get('InvoiceDate')} 是文本 'YYYY-MM-DD 00:00:00'；按年/月用 strftime('%Y-%m', ${codeOf.get('InvoiceDate')})。`
	);
	lines.push('- **文本拼接**：SQLite 用 `||`。');
	lines.push(
		`- **NULL 外键**：${codeOf.get('AlbumId')} / ${codeOf.get('GenreId')}（曲目）、${codeOf.get('SupportRepId')}（客户）、` +
		`${codeOf.get('ReportsTo')}（员工）可为空；JOIN 会丢这些行，要保留就 LEFT JOIN，判空用 IS NULL 不要用 = NULL。`
	);
	lines.push(
		'- **计数去重**：歌单可能有同名项，计数按歌单主键而非名称列；可能重复的行用 COUNT(DISTINCT ...)。'
	);
	lines.push('- **参数**：值用 `?` 占位符按顺序放进 `params`，不要拼字符串。`query_sql` 默认最多 100 行（可传 `limit`，上限 1000）。');
	lines.push('');

	lines.push('## 常用 SQL 模板（改条件即用）');
	lines.push('');
	for (const tpl of TEMPLATES) {
		lines.push(`${tpl.title}：`);
		lines.push('');
		lines.push('```sql');
		lines.push(replaceTokens(tpl.sql, codeOf));
		lines.push('```');
		lines.push('');
	}

	return lines.join('\n');
}

writeFileSync(OUT_SKILL, renderSkill());

// ── map.json（含原名，不入库）────────────────────────────────
const mapOut = {
	seed: SEED,
	generatedFrom: 'chinook.original.db',
	tables: tables.map((t) => ({
		original: t.name,
		code: codeOf.get(t.name),
		meaning: TABLE_MEANING[t.name] ?? null,
		columns: t.columns.map((c) => ({
			original: c.name,
			code: codeOf.get(c.name),
			meaning: COLUMN_MEANING[c.name] ?? null,
		})),
	})),
	indexes: indexRows.map((idx) => ({ original: idx.name, code: codeOf.get(idx.name) })),
};
writeFileSync(OUT_MAP, JSON.stringify(mapOut, null, 2) + '\n');

// ── 校验：没有原名残留 ──────────────────────────────────────
function checkNoOriginalNames() {
	const problems = [];
	const original = [...allNames].filter((n) => n.length > 1); // 单字符名太容易误伤，跳过
	const tokenRe = new RegExp(`\\b(?:${original.map((n) => n.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|')})\\b`);

	const schemaText = out
		.prepare("SELECT sql FROM sqlite_master WHERE sql IS NOT NULL")
		.all()
		.map((r) => r.sql)
		.join('\n');
	if (tokenRe.test(schemaText)) problems.push('库的 sqlite_master 里还有原名');

	for (const [label, text] of [['sqlite.sql', readFileSync(OUT_SQL, 'utf-8')], ['SKILL.md', readFileSync(OUT_SKILL, 'utf-8')]]) {
		const hit = text.match(tokenRe);
		if (hit) problems.push(`${label} 里还有原名：${hit[0]}`);
	}
	return problems;
}

// ── 校验：行数一致 ──────────────────────────────────────────
function checkRowCounts() {
	const problems = [];
	for (const t of tables) {
		const n = out.prepare(`SELECT COUNT(*) AS n FROM ${quote(codeOf.get(t.name))}`).get().n;
		if (n !== t.rowCount) problems.push(`${t.name} → ${codeOf.get(t.name)} 行数 ${n} ≠ ${t.rowCount}`);
	}
	return problems;
}

src.close();

const nameProblems = checkNoOriginalNames();
const rowProblems = checkRowCounts();
const problems = [...nameProblems, ...rowProblems];
out.close();

console.log(`[obfuscate] seed=${SEED} 表 ${tables.length} / 列 ${allNames.size} 个名字`);
console.log(`[obfuscate] 写出：${OUT_DB}、${OUT_SQL}、${OUT_SKILL}、${OUT_MAP}`);
if (problems.length > 0) {
	console.error('[obfuscate] ✗ 校验未过：');
	for (const p of problems) console.error('  - ' + p);
	process.exit(1);
}
console.log('[obfuscate] ✓ 无原名残留、行数一致');
