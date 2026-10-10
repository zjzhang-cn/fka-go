# mcp_sqlite

Chinook 示例库（`chinook.db`）的只读查询 MCP 服务。工具分三类：

- **表名清单**：`list_tables` 列出所有用户表名，先看有哪些表。
- **按表查询**， `query_<表名>`：按表查询，列过滤 + 分页，**不支持 JOIN**。
- **复杂查询**：`query_sql` 跑任意只读 SELECT/WITH（JOIN / GROUP BY / 聚合 / 子查询 /
  窗口函数）；`describe_schema` 先给出表结构与外键，便于拼复杂 SQL。

表结构从 `sqlite.sql` 解析（文件缺失时退回 `PRAGMA table_info` 反射），所以改动建表脚本
后按表工具清单会跟着变。

## 用法

在 `mcp.json` 里注册（路径可换成绝对路径，避免依赖 cwd）：

```json
{
  "mcpServers": {
    "sqlite": { "command": "node", "args": ["mcp_sqlite/server.mjs"] }
  }
}
```

FKA 启动后工具以 `mcp__sqlite__query_album` 的形式交给模型；MCP 工具一律
`effect: 'external'`，需要 `LLM_TOOL_EFFECTS=read,external` 才可见。

数据库路径可用 `CHINOOK_DB_PATH` 覆盖，默认取本目录的 `chinook.db`。

## 工具参数

### 表名清单 `list_tables`

无参数。返回库里所有用户表的名称（按字母序），用于不确定有哪些表时先看一眼。

### 按表查询 `query_<表名>`

- **列名**：标量为等值过滤（文本含 `%` 等价 `LIKE`），或操作符对象
  `{ "like": "%x%" }` / `{ "gt": 5 }` / `{ "gte": 5 }` / `{ "lt": 5 }` / `{ "lte": 5 }` /
  `{ "in": [1, 2] }`；
- `limit`（默认 20，最大 200）、`offset`；
- `orderBy`（任一列名）、`order`（`asc` / `desc`）；
- `columns`（只返回指定列，默认全部）。

返回一段带行数统计的文本表。

### 复杂查询 `query_sql`

- `sql`：一条 SELECT / WITH 开头的只读查询；
- `params`：按顺序替换 SQL 里 `?` 占位符的值数组（可选，别拼字符串）；
- `limit`：最多返回行数，默认 100，最大 1000；超出时提示截断。

返回带行数的 JSON Lines。

### 结构速查 `describe_schema`

- `table`：只看这张表；不传返回全部。

返回每张表的列（类型 / PK / NOT NULL）与外键。

## 安全约束

- 库以 `node:sqlite` 的 `readOnly` 打开；
- 按表查询的 SQL 全部走参数占位符，表名 / 列名只从解析结果来并做标识符校验；
- `query_sql` 只放行**单条** SELECT / WITH（拒绝多语句与非只读开头），写入由只读连接兜底；
- stdout 是 JSON-RPC 协议通道，日志一律走 `console.error`。
