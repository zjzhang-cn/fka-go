// Package domain 放跨模块共享的**领域枚举**：文档状态、可见性、记忆类型。
//
// 它们原先在 db/schema.ts 里，于是「记忆的契约」与「记忆的呈现」都得 import 具体的
// 数据库 schema——换后端时契约跟着 schema 一起动。这几组取值是**领域词汇**，不是某张表
// 的列定义，所以放在这一层，schema 反过来 import 它们。
package domain

// DocStatus 文档的处理状态。两段式回复与重试都靠它。
type DocStatus string

const (
	// StatusPending 已落盘，尚未解析
	StatusPending DocStatus = "pending"
	// StatusParsing 正在解析。进程若在此状态下挂掉，重启后要靠它识别孤儿任务
	StatusParsing DocStatus = "parsing"
	// StatusReady 解析完成，正文可检索
	StatusReady DocStatus = "ready"
	// StatusFailed 解析失败。error 列里有原因，可按 attempts 重试
	StatusFailed DocStatus = "failed"
)

// Visibility 可见性。只有两级，区别在「谁能询问」。
type Visibility string

const (
	// VisPublic 公开。文档自 2026-09-25 起默认 private，这条值要显式标
	VisPublic Visibility = "public"
	// VisPrivate 私有。只有属主能问到
	VisPrivate Visibility = "private"
)

// MemoryType 家庭记忆的类型。
//
// 四类的区别在**这条记忆说的是什么**，不在存储方式——它们同表同列。
type MemoryType string

const (
	// MemEvent 已发生的家庭事件（「2025 年 3 月全家去了三亚」）
	MemEvent MemoryType = "event"
	// MemReminder 需要关注的时间节点（「冰箱保修还有 30 天」）
	MemReminder MemoryType = "reminder"
	// MemExperience 经验/教训（「上次修空调 800 块，师傅电话是…」）
	MemExperience MemoryType = "experience"
	// MemKnowledge 家庭知识/常识（「孩子鸡蛋过敏」）。默认：用户顺手记一句的多半是这类
	MemKnowledge MemoryType = "knowledge"
)

// ValidMemoryType 判断一个值是否是四类之一。/记忆 命令用它校验用户输入，
// 呈现层用它决定标签——**未知类型原样回显，不映射到兜底标签**。
func ValidMemoryType(t string) bool {
	switch MemoryType(t) {
	case MemEvent, MemReminder, MemExperience, MemKnowledge:
		return true
	}
	return false
}
