package main

import "testing"

// Test用量行_渲染与缺项 usageLine 是纯函数，ask 与 chat 共用。三种情形：
// 三个数都给了、没给总数、一个数都没有（不打）。
func Test用量行_渲染与缺项(t *testing.T) {
	for name, tc := range map[string]struct {
		in, out, total int
		want           string
	}{
		"都给了":  {10, 5, 15, "入 10 / 出 5（共 15）"},
		"没给总数": {10, 5, 0, "入 10 / 出 5"},
		"全零不打": {0, 0, 0, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := usageLine(tc.in, tc.out, tc.total); got != tc.want {
				t.Errorf("usageLine(%d, %d, %d) = %q，想要 %q", tc.in, tc.out, tc.total, got, tc.want)
			}
		})
	}
}
