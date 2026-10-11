#!/usr/bin/env node
/**
 * 音乐商店示例库的**名称混淆器**：把 `chinook.original.db` 的表名 / 列名 / 索引名换成与
 * 原名无关的稳定短码，写成 `chinook.db`；并**从原始技能 `SKILL.original.md` 变换**出
 * 混淆名版的技能 `SKILL.md`。
 *
 * ## 为什么这么做
 *
 * 目的是**测 Text-to-SQL**：模型若能从预训练里认出 Chinook 的表列名，测的就不是它读
 * schema 写 SQL 的能力，而是它背没背过这份示例库。把名字换成无意义短码，再给一份
 * 「短码 → 业务含义」的字典技能，才测得到真本事。
 *
 * ## 两个原始输入，两个混淆产物
 *
 *   chinook.original.db  →  chinook.db     （改表/列/索引名，数据不动）
 *   SKILL.original.md    →  SKILL.md       （标识符换短码；「名称对照」节用生成器字典重渲染）
 *
 * 技能以原始技能为底**变换**：结构原样保留，标识符换成短码。其中「## 名称对照」一节由
 * **生成器自身的字典**（`TABLE_MEANING` / `COLUMN_MEANING`）渲染成「短码 → 中文含义」，
 * 替换原文那一节——原始技能里也有一份同结构的「英文名 → 中文含义」，自检保证两边条目、
 * 顺序、含义一致，于是**原始技能与目标技能一一对应**。front-matter 的 description 由生成器
 * 补（原文列的是原表名的复数，会泄漏）。
 *
 * ## 四条不变量（每次运行自检）
 *
 * 1. **原名一个都不留**——包括 `sqlite_master.sql` 里那段建表语句、`IFK_AlbumArtistId`
 *    这类**索引名**（它自己就把原列名带出来了）、原文 description 里的复数表名。
 *    `CONSTRAINT [PK_...]` 这类约束名则直接删掉（SQLite 不需要它们）。
 * 2. **数据一个都不动**——值、行数、类型、PK/FK/NOT NULL 全保留；只改名。
 * 3. **可复现**——短码 = `hash(seed + 原名)`，同一 seed 必得同一份库与同一份技能。
 * 4. **对照不丢**——原始技能「名称对照」的条目、顺序、含义必须与生成器字典一致，缺一条就报错。
 *
 * 注意：服务端（server.mjs）**不读 sqlite.sql**，一律反射实际库；本工具也不再产出
 * `sqlite.sql`（结构的可读版本由技能的「名称对照」一节 + `describe_schema` 提供）。
 *
 * ## 用法
 *
 *   node obfuscate.mjs [--seed chinook-obf] [--in chinook.original.db]
 *                      [--out chinook.db]
 *                      [--skill-src SKILL.original.md]
 *                      [--skill ../../skills/music-catalog-analytics/SKILL.md]
 *                      [--map obfuscate.map.json]
 *
 * `--map` 里带**原名**（code ↔ 含义 ↔ 原名），只给工具用，**不要入库**（.gitignore 已忽略）。
 * 技能里只出现业务含义，绝不出现原名。
 */
import { readFileSync, writeFileSync, existsSync, unlinkSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { dirname, resolve } from 'node:path';
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
const OUT_SKILL = resolve(HERE, args.skill ?? '../../skills/music-catalog-analytics/SKILL.md');
const SKILL_SRC = resolve(HERE, args['skill-src'] ?? 'SKILL.original.md');
const OUT_MAP = resolve(HERE, args.map ?? 'obfuscate.map.json');

// ── 语义字典（只在这份脚本里出现原名）────────────────────────
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

// 混淆后技能的 front-matter description。**由生成器写**：原文那份列的是原表名的复数
// （Artists、Albums…），令牌替换匹配不到会残留可识别的 Chinook 名。
const SAFE_DESCRIPTION =
	'音乐商店（Music Store）关系型数据库的查询技能。**本库的表名与列名已被混淆**（如 z_a1b2），' +
	'本技能是唯一权威的名称字典：把混淆名对应到业务含义、表间关系与常用 SQL 模板。' +
	'适用：跨表联合查询（热销流派、客户消费排行、员工业绩）、资产多维清点（按流派/格式/年度统计曲目）、' +
	'异常排查（无销量的孤儿曲目、未分配客服的客户）。**不要凭记忆臆造表名或列名；不确定就用 ' +
	'describe_schema 核对，一切以本技能与库结构为准。**';

// 表名的**复数**（原文 description 里出现过）。单独列出来一并守泄漏，防回归。
const PLURAL_ALIASES = [
	'Artists', 'Albums', 'Tracks', 'Genres', 'MediaTypes', 'Playlists', 'PlaylistTracks',
	'Customers', 'Invoices', 'InvoiceLines', 'Employees', 'Invoice_Items',
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

function replaceTokens(text, codeOf) {
	return text.replace(TOKEN, (token) => codeOf.get(token) ?? token);
}

function obfuscateSql(sql, codeOf) {
	return replaceTokens(stripConstraintNames(sql), codeOf);
}

const quote = (ident) => `"${String(ident).replace(/"/g, '""')}"`;

// ── 读原库结构 ──────────────────────────────────────────────
const src = new DatabaseSync(IN_DB, { readOnly: true });

const tables = src
	.prepare("SELECT name, sql FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	.all()
	.map(({ name, sql }) => ({
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
	.all();

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

for (const t of tables) {
	const ddl = obfuscateSql(t.sql, codeOf);
	out.exec(ddl.endsWith(';') ? ddl : ddl + ';');
}
for (const idx of indexRows) {
	const ddl = obfuscateSql(idx.sql, codeOf);
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

// ── 渲染 SKILL.md：从 SKILL.original.md 变换而来 ──────────────
//
// 技能以**原始技能**为底，生成器做四件事：
//   1. 换掉 front-matter 的 description（原文列的是原表名的复数，见 SAFE_DESCRIPTION）。
//   2. 删掉过时的「按表查询」条目并改掉对应的「选择」一句。
//   3. 用**生成器自身的字典**渲染「## 名称对照」一节（短码 → 中文含义），替换原文同名节。
//   4. 全篇标识符 → 短码。
// 于是目标技能与原始技能**结构、内容一一对应**（同节同序，只是英文名换成短码）。
function renderSkill() {
	if (!existsSync(SKILL_SRC)) {
		throw new Error(`找不到原始技能 ${SKILL_SRC}（可用 --skill-src 指定）`);
	}
	let text = readFileSync(SKILL_SRC, 'utf-8');

	text = text.replace(/^description:.*$/m, `description: ${SAFE_DESCRIPTION}`);
	text = text.replace(/- `query_<表名>`[\s\S]*?不支持 JOIN\*\*。\n/, '');
	text = text.replace(
		/^\*\*选择\*\*：.*$/m,
		'**选择**：先 `describe_schema` 看结构；要数据一律用 `query_sql`（单表就直接 SELECT 那张表）。'
	);
	text = replaceMeaningSection(text, renderMeaningSection());
	text = replaceTokens(text, codeOf);

	return text.replace(/\s*$/, '') + '\n';
}

// renderMeaningSection 用**生成器自身的字典**渲染「## 名称对照」一节（短码 → 中文含义）。
// 顺序取自字典本身，与原始技能那一节一致。
function renderMeaningSection() {
	const lines = [
		'## 名称对照',
		'',
		'英文表名 / 字段名 → 中文含义。混淆时这一节的英文名整体换成短码，含义列原样保留，',
		'所以目标技能的同一节与这里一一对应。',
		'',
		'表名：',
		'',
	];
	for (const name of Object.keys(TABLE_MEANING)) lines.push(`- ${codeOf.get(name)} → ${TABLE_MEANING[name]}`);
	lines.push('', '字段名：', '');
	for (const name of Object.keys(COLUMN_MEANING)) lines.push(`- ${codeOf.get(name)} → ${COLUMN_MEANING[name]}`);
	return lines.join('\n') + '\n';
}

// replaceMeaningSection 把原文的「## 名称对照」一节换成生成器渲染的那份（到下一个 `## ` 为止）。
function replaceMeaningSection(text, rendered) {
	const header = /^## 名称对照\s*$/m;
	const at = text.search(header);
	if (at < 0) throw new Error(`原始技能里找不到「## 名称对照」一节：${SKILL_SRC}`);
	const before = text.slice(0, at);
	const after = text.slice(at).split(header)[1] ?? '';
	const nextAt = after.search(/^## /m);
	const tail = nextAt >= 0 ? after.slice(nextAt) : '';
	return before + rendered + '\n' + tail;
}

// parseMeaningSection 从原始技能的「## 名称对照」一节取 `英文名 → 中文含义`（保持顺序）。
function parseMeaningSection(text) {
	const section = (text.split(/^## 名称对照\s*$/m)[1] ?? '').split(/^## /m)[0];
	const map = new Map();
	for (const line of section.split('\n')) {
		const hit = line.match(/^\s*-\s*(\S+)\s*→\s*(.+?)\s*$/);
		if (hit) map.set(hit[1], hit[2]);
	}
	return map;
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
	// 认得出的原名：库里的表/列/索引名，外加原文 description 里出现过的复数表名。
	const names = [...new Set([...allNames, ...PLURAL_ALIASES])].filter((n) => n.length > 1);
	const tokenRe = new RegExp(`\\b(?:${names.map((n) => n.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|')})\\b`);

	const schemaText = out
		.prepare("SELECT sql FROM sqlite_master WHERE sql IS NOT NULL")
		.all()
		.map((r) => r.sql)
		.join('\n');
	if (tokenRe.test(schemaText)) problems.push('库的 sqlite_master 里还有原名');

	const hit = readFileSync(OUT_SKILL, 'utf-8').match(tokenRe);
	if (hit) problems.push(`SKILL.md 里还有原名：${hit[0]}`);

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

// ── 校验：原始技能与生成器字典一一对应 ──────────────────────
// 生成器用自己的字典渲染目标技能的「名称对照」；原始技能也有一份同结构的英文版。这条钉住
// 两边条目、顺序、含义一致——否则原始技能与目标技能就会「对不上」。
function checkMeaningCorresponds() {
	const problems = [];
	const parsed = parseMeaningSection(readFileSync(SKILL_SRC, 'utf-8'));
	const expected = [...Object.keys(TABLE_MEANING), ...Object.keys(COLUMN_MEANING)];
	const got = [...parsed.keys()];
	if (got.length !== expected.length || got.some((name, i) => name !== expected[i])) {
		problems.push('原始技能「名称对照」的条目或顺序与生成器字典不一致');
		return problems;
	}
	for (const name of expected) {
		const meaning = TABLE_MEANING[name] ?? COLUMN_MEANING[name];
		if (parsed.get(name) !== meaning) problems.push(`「名称对照」里 ${name} 的含义与生成器字典不一致`);
	}
	return problems;
}

src.close();

const problems = [...checkNoOriginalNames(), ...checkMeaningCorresponds(), ...checkRowCounts()];
out.close();

console.log(`[obfuscate] seed=${SEED} 表 ${tables.length} / 名字 ${allNames.size} 个`);
console.log(`[obfuscate] 写出：${OUT_DB}、${OUT_SKILL}、${OUT_MAP}`);
if (problems.length > 0) {
	console.error('[obfuscate] ✗ 校验未过：');
	for (const p of problems) console.error('  - ' + p);
	process.exit(1);
}
console.log('[obfuscate] ✓ 无原名残留、行数一致、原始/目标对照一致');
