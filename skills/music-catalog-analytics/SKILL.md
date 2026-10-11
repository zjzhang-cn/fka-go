---
name: music-catalog-analytics
description: 本技能专注于音乐商店（Music Store）关系型数据库的架构理解与复杂数据查询。包含媒体资产、销售发票、客户关系及员工组织四大核心业务板块。 **适用场景**：支持的表关系模型, 涵盖 Artists、Albums、Tracks、Genres、Customers、Invoices、Invoice_Items 等表的 1:N 和 N:M 关联逻辑。**核心功能**：跨表联合查询（如分析热销音乐流派、客户消费排行、员工业绩考核）。资产多维清点（如按流派、媒体类型、年度统计曲库分布）。异常数据排查（如查找无销售记录的孤儿单曲、未分配服务代表的客户）。
---

# 音乐商店数据库查询指南

SQLite 音乐商店示例库。mcp工具（ `mcp__sqlite__*` ）需 `LLM_TOOL_EFFECTS=read,external`：

- `list_tables`：列出全部表名（不确定有哪些表时先看它）。
- `describe_schema`：查表结构（列 / PK / NOT NULL / 外键），可传 `table` 只看一张表。
- `query_<表名>`：一表一个——`query_album` `query_artist` `query_customer` `query_employee`
  `query_genre` `query_invoice` `query_invoiceline` `query_mediatype` `query_playlist`
  `query_playlisttrack` `query_track`；列过滤 + 分页，**不支持 JOIN**。
- `query_sql`：一条只读 SELECT/WITH，支持 JOIN / GROUP BY / 聚合 / 子查询 / 窗口 / 递归 CTE；
  值用 `?` 占位符放进 `params`。

**选择**：只罗列单表数据 → `query_<表名>`；跨表、聚合、排行、层级 → `query_sql`。

## 执行流程（照做，少走弯路）

1. 想清楚要哪几个表、哪些列、怎么过滤/分组/排序。
2. 字段或外键不确定就先 `describe_schema`（可只传一张表），**不要臆造表名或列名**。
3. 跨表用 `query_sql`：每张表起别名，列名带别名前缀（`Name`、`UnitPrice` 在多表重名）。
4. 聚合列用 `AS` 起名（返回 JSON 的键就是别名）；加 `LIMIT` 防刷屏。
5. 报错就按错误里的表名/列名/语法改正后重试；只允许单条 SELECT/WITH。

## 表结构

记法：`PK` 主键、`FK→表` 外键、未标即可空；ID/数量为 INTEGER，文本 NVARCHAR，金额
NUMERIC(10,2)，标 † 为 DATETIME（文本日期）。

媒体与音乐：

- **Artist**：ArtistId PK；Name。
- **Album**：AlbumId PK；Title；ArtistId FK→Artist。
- **Genre**：GenreId PK；Name。
- **MediaType**：MediaTypeId PK；Name。
- **Track**：TrackId PK；Name；AlbumId FK→Album；MediaTypeId FK→MediaType；GenreId FK→Genre；
  Composer；Milliseconds（毫秒）；Bytes；UnitPrice（售价）。

播放列表：

- **Playlist**：PlaylistId PK；Name。
- **PlaylistTrack**（多对多中间表）：PlaylistId PK·FK→Playlist；TrackId PK·FK→Track。

客户与销售：

- **Customer**：CustomerId PK；FirstName；LastName；Company；Address；City；State；Country；
  PostalCode；Phone；Fax；Email；SupportRepId FK→Employee。
- **Invoice**：InvoiceId PK；CustomerId FK→Customer；InvoiceDate†；BillingAddress；BillingCity；
  BillingState；BillingCountry；BillingPostalCode；Total（订单总额）。
- **InvoiceLine**：InvoiceLineId PK；InvoiceId FK→Invoice；TrackId FK→Track；UnitPrice（成交单价）；
  Quantity。

员工：

- **Employee**：EmployeeId PK；LastName；FirstName；Title；ReportsTo FK→Employee（自关联，上级）；
  BirthDate†；HireDate†；Address；City；State；Country；PostalCode；Phone；Fax；Email。

## 表间关系

| 子表.外键 | → 父表.主键 | 关系 | 场景 |
|---|---|---|---|
| Album.ArtistId | Artist.ArtistId | N:1 | 专辑属某歌手 |
| Track.AlbumId | Album.AlbumId | N:1 | 歌曲属某专辑 |
| Track.GenreId | Genre.GenreId | N:1 | 歌曲属某流派 |
| Track.MediaTypeId | MediaType.MediaTypeId | N:1 | 歌曲属某格式 |
| PlaylistTrack.PlaylistId | Playlist.PlaylistId | N:1 | 歌单↔歌曲（多对多） |
| PlaylistTrack.TrackId | Track.TrackId | N:1 | 同上 |
| Invoice.CustomerId | Customer.CustomerId | N:1 | 客户的发票 |
| InvoiceLine.InvoiceId | Invoice.InvoiceId | N:1 | 发票的明细 |
| InvoiceLine.TrackId | Track.TrackId | N:1 | 明细里的歌曲 |
| Customer.SupportRepId | Employee.EmployeeId | N:1 | 客户的客服 |
| Employee.ReportsTo | Employee.EmployeeId | N:1 | 员工上级（自关联） |

## SQL 编写规则（提准）

- **金额**：实际成交额 = `SUM(InvoiceLine.UnitPrice * InvoiceLine.Quantity)`；`Invoice.Total` 已是
  订单总额。二者按问题选一个，**不要重复相加**。
- **重名列**：`Name`（Artist/Album/Track/Genre/MediaType/Playlist）、`UnitPrice`
  （Track/InvoiceLine）必须带表别名。
- **时长**：`Milliseconds` 是毫秒，展示秒用 `ROUND(Milliseconds/1000.0, 1)`。
- **日期**：`InvoiceDate` 是文本 `'YYYY-MM-DD 00:00:00'`；按年/月用 `strftime('%Y-%m', InvoiceDate)`。
- **文本拼接**：SQLite 用 `||`，如 `FirstName || ' ' || LastName AS name`。
- **NULL 外键**：`Track.AlbumId/GenreId`、`Customer.SupportRepId`、`Employee.ReportsTo` 可为空；
  `JOIN` 会丢掉这些行，要保留就 `LEFT JOIN`，判空用 `IS NULL` 不要用 `= NULL`。
- **计数去重**：歌单有同名项，计数按 `PlaylistId` 而非 `Name`；可能重复的行用
  `COUNT(DISTINCT ...)`。
- **参数**：值用 `?` 占位符按顺序放进 `params`，不要拼字符串。`query_sql` 默认最多 100 行
  （可传 `limit`，上限 1000），不够就聚合或分页。

## 常用 SQL 模板（改条件即用）

各流派销售额 Top5：

```sql
SELECT g.Name AS genre, ROUND(SUM(il.UnitPrice*il.Quantity),2) AS revenue
FROM InvoiceLine il JOIN Track t ON t.TrackId=il.TrackId JOIN Genre g ON g.GenreId=t.GenreId
GROUP BY g.GenreId ORDER BY revenue DESC LIMIT 5
```

艺人歌曲数 Top10：

```sql
SELECT ar.Name AS artist, COUNT(t.TrackId) AS tracks
FROM Artist ar JOIN Album al ON al.ArtistId=ar.ArtistId JOIN Track t ON t.AlbumId=al.AlbumId
GROUP BY ar.ArtistId ORDER BY tracks DESC LIMIT 10
```

客户消费额 Top5：

```sql
SELECT c.CustomerId, c.FirstName||' '||c.LastName AS name, ROUND(SUM(i.Total),2) AS total
FROM Customer c JOIN Invoice i ON i.CustomerId=c.CustomerId
GROUP BY c.CustomerId ORDER BY total DESC LIMIT 5
```

按月销售额：

```sql
SELECT strftime('%Y-%m', InvoiceDate) AS ym, ROUND(SUM(Total),2) AS total
FROM Invoice GROUP BY ym ORDER BY ym
```

每张专辑时长最长的曲目：

```sql
WITH r AS (
  SELECT al.Title AS album, t.Name AS track, t.Milliseconds AS ms,
         ROW_NUMBER() OVER (PARTITION BY al.AlbumId ORDER BY t.Milliseconds DESC) AS rn
  FROM Track t JOIN Album al ON al.AlbumId=t.AlbumId)
SELECT album, track, ms FROM r WHERE rn=1 ORDER BY ms DESC LIMIT 10
```

员工上下级层级：

```sql
WITH RECURSIVE org(id,name,mgr,lvl) AS (
  SELECT EmployeeId, FirstName||' '||LastName, ReportsTo, 0 FROM Employee WHERE ReportsTo IS NULL
  UNION ALL SELECT e.EmployeeId, e.FirstName||' '||e.LastName, e.ReportsTo, org.lvl+1
  FROM Employee e JOIN org ON e.ReportsTo=org.id)
SELECT lvl, mgr, name FROM org ORDER BY lvl, mgr
```

歌单曲目数 Top5：

```sql
SELECT p.PlaylistId, p.Name AS playlist, COUNT(pt.TrackId) AS tracks
FROM Playlist p JOIN PlaylistTrack pt ON pt.PlaylistId=p.PlaylistId
GROUP BY p.PlaylistId ORDER BY tracks DESC LIMIT 5
```
