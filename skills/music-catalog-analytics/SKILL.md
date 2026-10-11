---
name: music-catalog-analytics
description: 音乐商店（Music Store）关系型数据库的查询技能。**本库的表名与列名已被混淆**（如 z_a1b2），本技能是唯一权威的名称字典：把混淆名对应到业务含义、表间关系与常用 SQL 模板。适用：跨表联合查询（热销流派、客户消费排行、员工业绩）、资产多维清点（按流派/格式/年度统计曲目）、异常排查（无销量的孤儿曲目、未分配客服的客户）。**不要凭记忆臆造表名或列名；不确定就用 describe_schema 核对，一切以本技能与库结构为准。**
---

# 音乐商店数据库查询指南

SQLite 音乐商店示例库。mcp工具（ `mcp__sqlite__*` ）需 `LLM_TOOL_EFFECTS=read,external`：

- `list_tables`：列出全部表名（不确定有哪些表时先看它）。
- `describe_schema`：查表结构（列 / PK / NOT NULL / 外键），可传 `table` 只看一张表。
- `query_sql`：一条只读 SELECT/WITH，支持 JOIN / GROUP BY / 聚合 / 子查询 / 窗口 / 递归 CTE；
  值用 `?` 占位符放进 `params`。

**选择**：先 `describe_schema` 看结构；要数据一律用 `query_sql`（单表就直接 SELECT 那张表）。

## 执行流程（照做，少走弯路）

1. 想清楚要哪几个表、哪些列、怎么过滤/分组/排序。
2. 字段或外键不确定就先 `describe_schema`（可只传一张表），**不要臆造表名或列名**。
3. 跨表用 `query_sql`：每张表起别名，列名带别名前缀（`z_f77cd2`、`z_aafe4d` 在多表重名）。
4. 聚合列用 `AS` 起名（返回 JSON 的键就是别名）；加 `LIMIT` 防刷屏。
5. 报错就按错误里的表名/列名/语法改正后重试；只允许单条 SELECT/WITH。

## 表结构

记法：`PK` 主键、`FK→表` 外键、未标即可空；ID/数量为 INTEGER，文本 NVARCHAR，金额
NUMERIC(10,2)，标 † 为 DATETIME（文本日期）。

媒体与音乐：

- **z_f4a38f**：z_2f7d91 PK；z_f77cd2。
- **z_7c9787**：z_390399 PK；z_e6d303；z_2f7d91 FK→z_f4a38f。
- **z_69bb8b**：z_ccacba PK；z_f77cd2。
- **z_d3a8b1**：z_8c15c7 PK；z_f77cd2。
- **z_fa92c4**：z_03ab46 PK；z_f77cd2；z_390399 FK→z_7c9787；z_8c15c7 FK→z_d3a8b1；z_ccacba FK→z_69bb8b；
  z_95237a；z_a4e40b（毫秒）；z_0b3164；z_aafe4d（售价）。

播放列表：

- **z_30d9c3**：z_ea1b0b PK；z_f77cd2。
- **z_7a1bbe**（多对多中间表）：z_ea1b0b PK·FK→z_30d9c3；z_03ab46 PK·FK→z_fa92c4。

客户与销售：

- **z_2af565**：z_8c8908 PK；z_742f3a；z_1aad66；z_dd2680；z_095c3b；z_c89d56；z_6e134e；z_44584d；
  z_a2c90a；z_e2c0e1；z_1c61a7；z_bfe382；z_a8691f FK→z_fbb450。
- **z_3ec2de**：z_a24fb2 PK；z_8c8908 FK→z_2af565；z_605bad†；z_ead0f8；z_3c9709；
  z_ff3acc；z_47e659；z_4a113e；z_6c577b（订单总额）。
- **z_3427e1**：z_9d0b0f PK；z_a24fb2 FK→z_3ec2de；z_03ab46 FK→z_fa92c4；z_aafe4d（成交单价）；
  z_9238bb。

员工：

- **z_fbb450**：z_05c212 PK；z_1aad66；z_742f3a；z_e6d303；z_4e96af FK→z_fbb450（自关联，上级）；
  z_5e1000†；z_c8bf4f†；z_095c3b；z_c89d56；z_6e134e；z_44584d；z_a2c90a；z_e2c0e1；z_1c61a7；z_bfe382。

## 名称对照

英文表名 / 字段名 → 中文含义。混淆时这一节的英文名整体换成短码，含义列原样保留，
所以目标技能的同一节与这里一一对应。

表名：

- z_f4a38f → 歌手 / 表演者
- z_7c9787 → 专辑
- z_fa92c4 → 曲目（歌曲）
- z_69bb8b → 音乐流派
- z_d3a8b1 → 媒体格式（编码 / 载体）
- z_30d9c3 → 播放列表（歌单）
- z_7a1bbe → 歌单与曲目的多对多关联表
- z_2af565 → 客户
- z_3ec2de → 发票（订单）
- z_3427e1 → 发票明细行
- z_fbb450 → 员工

字段名：

- z_095c3b → 街道地址
- z_390399 → 专辑 ID
- z_2f7d91 → 歌手 ID
- z_ead0f8 → 账单街道地址
- z_3c9709 → 账单城市
- z_47e659 → 账单国家
- z_4a113e → 账单邮编
- z_ff3acc → 账单州 / 省
- z_5e1000 → 出生日期
- z_0b3164 → 文件字节数
- z_c89d56 → 城市
- z_dd2680 → 公司
- z_95237a → 作曲者
- z_44584d → 国家
- z_8c8908 → 客户 ID
- z_bfe382 → 邮箱
- z_05c212 → 员工 ID
- z_1c61a7 → 传真
- z_742f3a → 名
- z_ccacba → 流派 ID
- z_c8bf4f → 入职日期
- z_605bad → 订单日期
- z_a24fb2 → 发票 ID
- z_9d0b0f → 发票明细行 ID
- z_1aad66 → 姓
- z_8c15c7 → 媒体格式 ID
- z_a4e40b → 时长（毫秒）
- z_f77cd2 → 名称（含义随所在表而定：歌手名 / 专辑名 / 曲名 / 流派名 / 格式名 / 歌单名）
- z_e2c0e1 → 电话
- z_ea1b0b → 歌单 ID
- z_a2c90a → 邮编
- z_9238bb → 数量
- z_4e96af → 上级员工 ID
- z_6e134e → 州 / 省
- z_a8691f → 客服代表（员工）ID
- z_e6d303 → 头衔 / 标题（员工表为职务，专辑表为专辑名）
- z_6c577b → 订单总额
- z_03ab46 → 曲目 ID
- z_aafe4d → 单价

## 表间关系

| 子表.外键 | → 父表.主键 | 关系 | 场景 |
|---|---|---|---|
| z_7c9787.z_2f7d91 | z_f4a38f.z_2f7d91 | N:1 | 专辑属某歌手 |
| z_fa92c4.z_390399 | z_7c9787.z_390399 | N:1 | 歌曲属某专辑 |
| z_fa92c4.z_ccacba | z_69bb8b.z_ccacba | N:1 | 歌曲属某流派 |
| z_fa92c4.z_8c15c7 | z_d3a8b1.z_8c15c7 | N:1 | 歌曲属某格式 |
| z_7a1bbe.z_ea1b0b | z_30d9c3.z_ea1b0b | N:1 | 歌单↔歌曲（多对多） |
| z_7a1bbe.z_03ab46 | z_fa92c4.z_03ab46 | N:1 | 同上 |
| z_3ec2de.z_8c8908 | z_2af565.z_8c8908 | N:1 | 客户的发票 |
| z_3427e1.z_a24fb2 | z_3ec2de.z_a24fb2 | N:1 | 发票的明细 |
| z_3427e1.z_03ab46 | z_fa92c4.z_03ab46 | N:1 | 明细里的歌曲 |
| z_2af565.z_a8691f | z_fbb450.z_05c212 | N:1 | 客户的客服 |
| z_fbb450.z_4e96af | z_fbb450.z_05c212 | N:1 | 员工上级（自关联） |

## SQL 编写规则（提准）

- **金额**：实际成交额 = `SUM(z_3427e1.z_aafe4d * z_3427e1.z_9238bb)`；`z_3ec2de.z_6c577b` 已是
  订单总额。二者按问题选一个，**不要重复相加**。
- **重名列**：`z_f77cd2`（z_f4a38f/z_7c9787/z_fa92c4/z_69bb8b/z_d3a8b1/z_30d9c3）、`z_aafe4d`
  （z_fa92c4/z_3427e1）必须带表别名。
- **时长**：`z_a4e40b` 是毫秒，展示秒用 `ROUND(z_a4e40b/1000.0, 1)`。
- **日期**：`z_605bad` 是文本 `'YYYY-MM-DD 00:00:00'`；按年/月用 `strftime('%Y-%m', z_605bad)`。
- **文本拼接**：SQLite 用 `||`，如 `z_742f3a || ' ' || z_1aad66 AS name`。
- **NULL 外键**：`z_fa92c4.z_390399/z_ccacba`、`z_2af565.z_a8691f`、`z_fbb450.z_4e96af` 可为空；
  `JOIN` 会丢掉这些行，要保留就 `LEFT JOIN`，判空用 `IS NULL` 不要用 `= NULL`。
- **计数去重**：歌单有同名项，计数按 `z_ea1b0b` 而非 `z_f77cd2`；可能重复的行用
  `COUNT(DISTINCT ...)`。
- **参数**：值用 `?` 占位符按顺序放进 `params`，不要拼字符串。`query_sql` 默认最多 100 行
  （可传 `limit`，上限 1000），不够就聚合或分页。

## 常用 SQL 模板（改条件即用）

各流派销售额 Top5：

```sql
SELECT g.z_f77cd2 AS genre, ROUND(SUM(il.z_aafe4d*il.z_9238bb),2) AS revenue
FROM z_3427e1 il JOIN z_fa92c4 t ON t.z_03ab46=il.z_03ab46 JOIN z_69bb8b g ON g.z_ccacba=t.z_ccacba
GROUP BY g.z_ccacba ORDER BY revenue DESC LIMIT 5
```

艺人歌曲数 Top10：

```sql
SELECT ar.z_f77cd2 AS artist, COUNT(t.z_03ab46) AS tracks
FROM z_f4a38f ar JOIN z_7c9787 al ON al.z_2f7d91=ar.z_2f7d91 JOIN z_fa92c4 t ON t.z_390399=al.z_390399
GROUP BY ar.z_2f7d91 ORDER BY tracks DESC LIMIT 10
```

客户消费额 Top5：

```sql
SELECT c.z_8c8908, c.z_742f3a||' '||c.z_1aad66 AS name, ROUND(SUM(i.z_6c577b),2) AS total
FROM z_2af565 c JOIN z_3ec2de i ON i.z_8c8908=c.z_8c8908
GROUP BY c.z_8c8908 ORDER BY total DESC LIMIT 5
```

按月销售额：

```sql
SELECT strftime('%Y-%m', z_605bad) AS ym, ROUND(SUM(z_6c577b),2) AS total
FROM z_3ec2de GROUP BY ym ORDER BY ym
```

每张专辑时长最长的曲目：

```sql
WITH r AS (
  SELECT al.z_e6d303 AS album, t.z_f77cd2 AS track, t.z_a4e40b AS ms,
         ROW_NUMBER() OVER (PARTITION BY al.z_390399 ORDER BY t.z_a4e40b DESC) AS rn
  FROM z_fa92c4 t JOIN z_7c9787 al ON al.z_390399=t.z_390399)
SELECT album, track, ms FROM r WHERE rn=1 ORDER BY ms DESC LIMIT 10
```

员工上下级层级：

```sql
WITH RECURSIVE org(id,name,mgr,lvl) AS (
  SELECT z_05c212, z_742f3a||' '||z_1aad66, z_4e96af, 0 FROM z_fbb450 WHERE z_4e96af IS NULL
  UNION ALL SELECT e.z_05c212, e.z_742f3a||' '||e.z_1aad66, e.z_4e96af, org.lvl+1
  FROM z_fbb450 e JOIN org ON e.z_4e96af=org.id)
SELECT lvl, mgr, name FROM org ORDER BY lvl, mgr
```

歌单曲目数 Top5：

```sql
SELECT p.z_ea1b0b, p.z_f77cd2 AS playlist, COUNT(pt.z_03ab46) AS tracks
FROM z_30d9c3 p JOIN z_7a1bbe pt ON pt.z_ea1b0b=p.z_ea1b0b
GROUP BY p.z_ea1b0b ORDER BY tracks DESC LIMIT 5
```
