// Package domain 是这个 server 自己的**领域词汇**：记忆类型与可见性。
//
// ## 为什么是「自己的」而不是共用一份
//
// 这些取值原先放在一个跨 server 的包里，于是记忆的**契约**与**呈现**都要去 import
// 别人的内部约定——加一类记忆要跟文档的状态枚举商量，删一个 server 还要先确认
// 没人引用它。
//
// 现在它们只被本 server 用。**它是领域词汇，不是某张表的列定义**——所以放在
// 枚举这一层，schema 反过来 import 它。
package domain

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

// Visibility 可见性。只有两级，区别在「谁能询问」。
type Visibility string

const (
	// VisPublic 公开。所有家人都问得到
	VisPublic Visibility = "public"
	// VisPrivate 私有。只有属主能问到
	VisPrivate Visibility = "private"
)

// ValidMemoryType 判断一个值是否是四类之一。
//
// 呈现层用它决定标签——**未知类型原样回显，不映射到兜底标签**：把用户写的
// 「教训」显示成「经验」比显示原文更糟。
func ValidMemoryType(t string) bool {
	switch MemoryType(t) {
	case MemEvent, MemReminder, MemExperience, MemKnowledge:
		return true
	}
	return false
}

// ValidVisibility 判断一个值是不是两级之一。
func ValidVisibility(v string) bool {
	switch Visibility(v) {
	case VisPublic, VisPrivate:
		return true
	}
	return false
}
