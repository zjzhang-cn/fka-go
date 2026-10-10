// fka 网页客户端。**无框架、无构建**：原生 fetch + EventSource。
//
// 认证走 cookie（POST /login 种下），因为 EventSource 不能带自定义头。
// 令牌只存在内存里，不写 localStorage——XSS 拿不到它。
(function () {
  "use strict";

  var el = {
    status: document.getElementById("status"),
    conversation: document.getElementById("conversation"),
    token: document.getElementById("token"),
    connect: document.getElementById("connect"),
    log: document.getElementById("log"),
    form: document.getElementById("composer"),
    text: document.getElementById("text"),
    send: document.getElementById("send"),
  };

  var state = { conversation: "", source: null };
  // 当前**推理段**的元素：推理增量往它上面追加；一次工具调用或一条答案 = 这一段
  // 结束，下段另起一块（避免整轮的推理全堆在一个地方）。见 tool/message 处理器。
  var reasoningLine = null;

  // ── 会话 id ────────────────────────────────────────────
  // 存 localStorage：它不是机密，只用来把同一个浏览器认成同一段对话。
  function loadConversation() {
    var stored = localStorage.getItem("fka.conversation");
    if (stored && /^[A-Za-z0-9._-]{1,64}$/.test(stored)) return stored;
    var fresh = "web-" + Math.random().toString(36).slice(2, 10);
    localStorage.setItem("fka.conversation", fresh);
    return fresh;
  }

  // ── 渲染 ───────────────────────────────────────────────
  function scroll() { el.log.scrollTop = el.log.scrollHeight; }

  function addBubble(kind, text) {
    var row = document.createElement("div");
    row.className = "row " + kind;
    var bubble = document.createElement("div");
    bubble.className = "bubble";
    bubble.textContent = text;
    row.appendChild(bubble);
    el.log.appendChild(row);
    scroll();
    return bubble;
  }

  function addNote(kind, text) {
    var line = document.createElement("div");
    line.className = "note " + kind;
    line.textContent = text;
    el.log.appendChild(line);
    scroll();
    return line;
  }

  function addMedia(payload) {
    var box = document.createElement("div");
    box.className = "media";
    var url = payload.url || "";
    if ((payload.mimeType || "").indexOf("image/") === 0) {
      var img = document.createElement("img");
      img.src = url;
      img.alt = payload.name || "图片";
      box.appendChild(img);
    } else {
      var a = document.createElement("a");
      a.href = url;
      a.textContent = "📎 " + (payload.name || "下载文件");
      if (payload.name) a.setAttribute("download", payload.name);
      box.appendChild(a);
    }
    el.log.appendChild(box);
    scroll();
  }

  function setStatus(text, online) {
    el.status.textContent = text;
    el.status.className = "status " + (online ? "online" : "offline");
  }

  // ── 连接 ───────────────────────────────────────────────
  async function connect() {
    var token = el.token.value.trim();
    if (!token) { setStatus("请填令牌", false); return; }

    state.conversation = el.conversation.value.trim();
    if (!/^[A-Za-z0-9._-]{1,64}$/.test(state.conversation)) {
      setStatus("会话 id 不合法", false);
      return;
    }
    localStorage.setItem("fka.conversation", state.conversation);

    el.connect.disabled = true;
    try {
      var res = await fetch("/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ token: token }),
      });
      if (!res.ok) {
        setStatus("令牌不对", false);
        el.connect.disabled = false;
        return;
      }
    } catch (err) {
      setStatus("连不上服务器", false);
      el.connect.disabled = false;
      return;
    }

    if (state.source) state.source.close();
    var source = new EventSource("/events?conversation=" + encodeURIComponent(state.conversation));
    state.source = source;

    source.addEventListener("ready", function () {
      setStatus("已连接", true);
      el.connect.disabled = false;
      el.send.disabled = false;
    });
    source.addEventListener("message", function (ev) {
      // 一段完整答复。**最终答案与主动推送都走这里**（Emitter.Answer 是空的）
      var data = JSON.parse(ev.data);
      addBubble("bot", data.text || "");
      reasoningLine = null;
    });
    source.addEventListener("reasoning", function (ev) {
      var data = JSON.parse(ev.data);
      if (!reasoningLine) reasoningLine = addNote("reasoning", "");
      reasoningLine.textContent += data.text || "";
      // 限高 5 行（见 style.css），滚到底让**最新**的推理可见
      reasoningLine.scrollTop = reasoningLine.scrollHeight;
      scroll();
    });
    source.addEventListener("tool", function (ev) {
      var data = JSON.parse(ev.data);
      addNote("tool", "🔧 " + data.name + " → " + (data.result || "").slice(0, 200));
      // 一次工具调用 = 一段结束：下一段推理另起一块，不跟这一段堆在一起
      reasoningLine = null;
    });
    source.addEventListener("file", function (ev) {
      addMedia(JSON.parse(ev.data));
    });
    source.addEventListener("error", function () {
      // EventSource 会自己重连；连不上时它也会触发这里
      setStatus("连接断开，重连中…", false);
    });
  }

  // ── 发送 ───────────────────────────────────────────────
  async function send(text) {
    var res = await fetch("/messages", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ conversation: state.conversation, text: text }),
    });
    if (!res.ok) {
      var detail = "";
      try { detail = (await res.json()).error || ""; } catch (e) {}
      addNote("error", "发送失败：" + detail);
    }
  }

  el.form.addEventListener("submit", function (ev) {
    ev.preventDefault();
    var text = el.text.value.trim();
    if (!text) return;
    addBubble("user", text);
    el.text.value = "";
    send(text).catch(function () { addNote("error", "发送失败"); });
  });

  el.text.addEventListener("keydown", function (ev) {
    if (ev.key === "Enter" && !ev.shiftKey) {
      ev.preventDefault();
      el.form.requestSubmit();
    }
  });

  el.connect.addEventListener("click", connect);

  // 初始化
  el.conversation.value = loadConversation();
  el.send.disabled = true;
  el.token.focus();
})();
