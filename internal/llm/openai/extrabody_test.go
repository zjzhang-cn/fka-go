// 本文件钉「请求体外层的额外字段」。
//
// 这些字段**不是 OpenAI 规范的一部分**（`enable_thinking` 是推理模型那类实现的扩展），
// 所以它们不能写死在客户端构建里——那等于「每一个兼容端点都被塞上这一家的开关」，
// 而有些实现会对不认识的字段回 400。
package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestExtraBody默认注入enable_thinking 与旧行为一字不差——只是它现在**可以换掉**。
func TestExtraBody默认注入enable_thinking(t *testing.T) {
	setLLMEnv(t)
	t.Setenv(ExtraBodyEnv, "")

	cfg, ok := ReadConfig()
	if !ok {
		t.Fatal("key 与 model 都给了，该配置成功")
	}
	if cfg.ExtraBody["enable_thinking"] != true {
		t.Errorf("默认该注入 enable_thinking=true（本仓库的产品决定），实际 %v", cfg.ExtraBody)
	}
}

// TestExtraBody整份替换 换一家厂商的扩展时，默认那份**不该还留着**。
func TestExtraBody整份替换(t *testing.T) {
	setLLMEnv(t)
	t.Setenv(ExtraBodyEnv, `{"reasoning_effort":"high"}`)

	cfg, _ := ReadConfig()
	if _, still := cfg.ExtraBody["enable_thinking"]; still {
		t.Error("该是整份替换，不该还留着默认的 enable_thinking")
	}
	if cfg.ExtraBody["reasoning_effort"] != "high" {
		t.Errorf("替换没生效：%v", cfg.ExtraBody)
	}
}

// TestExtraBody空对象等于什么都不并 有些部署就该原样发标准请求体。
func TestExtraBody空对象等于什么都不并(t *testing.T) {
	setLLMEnv(t)
	t.Setenv(ExtraBodyEnv, `{}`)

	cfg, _ := ReadConfig()
	if len(cfg.ExtraBody) != 0 {
		t.Errorf("`{}` 该表示什么都不并，实际 %v", cfg.ExtraBody)
	}
}

// TestExtraBody格式错退回默认 一个笔误不该让服务起不来，但也不该悄悄少发一个字段
// ——那会表现成「模型行为变了」。
func TestExtraBody格式错退回默认(t *testing.T) {
	setLLMEnv(t)
	t.Setenv(ExtraBodyEnv, `{这不是 JSON`)

	cfg, _ := ReadConfig()
	if cfg.ExtraBody["enable_thinking"] != true {
		t.Errorf("格式错该退回默认值，实际 %v", cfg.ExtraBody)
	}
}

// Test注入器把额外字段并进请求体 上面几条验的是「读到什么」，这条验「真的发出去了」。
func Test注入器把额外字段并进请求体(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got = string(raw)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	injector := &bodyInjector{
		next:   server.Client(),
		extras: map[string]any{"enable_thinking": true},
		apiKey: "sk-test",
	}

	request, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := injector.Do(request)
	if err != nil {
		t.Fatalf("注入后请求失败：%v", err)
	}
	defer func() { _ = response.Body.Close() }()

	// 外层键被并入，原来的键一个不动
	if !strings.Contains(got, `"enable_thinking":true`) {
		t.Errorf("额外字段没并进去：%s", got)
	}
	if !strings.Contains(got, `"model":"m"`) {
		t.Errorf("原有字段被弄丢了：%s", got)
	}
	// 重排请求体后 ContentLength 必须跟着改，否则服务端读到截断的 body
	if request.ContentLength <= 0 {
		t.Errorf("ContentLength 没更新：%d", request.ContentLength)
	}
}

// setLLMEnv 给一组能把 ReadConfig 喂饱的环境变量。
func setLLMEnv(t *testing.T) {
	t.Helper()

	t.Setenv("LLM_API_KEY", "sk-test")
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("LLM_BASE_URL", "")
}
