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

  var state = { conversation: "", source: null, running: false };
  // 当前**推理段**的元素：推理增量往它上面追加；一次工具调用或一条答案 = 这一段
  // 结束，下段另起一块（避免整轮的推理全堆在一个地方）。见 tool/message 处理器。
  var reasoningLine = null;

  // 键用图标，不用文字：发送是一支纸飞机，执行中换成停止方块。
  var ICON_SEND = '<svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M22 2 11 13"/><path d="M22 2 15 22 11 13 2 9z"/></svg>';
  var ICON_STOP = '<svg viewBox="0 0 24 24" width="18" height="18" fill="currentColor"><rect x="6" y="6" width="12" height="12" rx="2"/></svg>';

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

  // 一轮的真实 token 用量（服务端报的，经 usage 事件推来）。**没给总数就省略
  // 「（共 …）」**；全零时服务端根本不推这一帧。
  function usageText(data) {
    var line = "入 " + (data.promptTokens || 0) + " / 出 " + (data.completionTokens || 0);
    if (data.totalTokens) line += "（共 " + data.totalTokens + "）";
    return "用量：" + line;
  }

  // 后缀 → 类别，给「MIME 认不出来」兜底。服务端的 Content-Type 大多已经具体
  // （含 svg、视频、音频），但通配（image/*）、空串、或嗅探认不出的类型要有第二条路。
  var EXT_KIND = {
    png: "image", jpg: "image", jpeg: "image", gif: "image", webp: "image",
    bmp: "image", svg: "image", ico: "image", avif: "image",
    mp4: "video", webm: "video", mov: "video", mkv: "video", avi: "video", ogv: "video",
    mp3: "audio", wav: "audio", ogg: "audio", oga: "audio", m4a: "audio",
    aac: "audio", flac: "audio", opus: "audio", pdf: "pdf",
  };

  // mediaKind 决定用哪种元素渲染：先看 MIME 前缀，看不出来再看后缀。
  function mediaKind(mime, name) {
    mime = (mime || "").toLowerCase();
    if (mime.indexOf("image/") === 0) return "image";
    if (mime.indexOf("video/") === 0) return "video";
    if (mime.indexOf("audio/") === 0) return "audio";
    if (mime.indexOf("application/pdf") === 0) return "pdf";
    var m = /\.([a-z0-9]+)$/.exec((name || "").toLowerCase());
    return (m && EXT_KIND[m[1]]) || "file";
  }

  function addMedia(payload) {
    var box = document.createElement("div");
    box.className = "media";
    var url = payload.url || "";
    var kind = mediaKind(payload.mimeType, payload.name);
    var node;
    if (kind === "image") {
      node = document.createElement("img");
      node.src = url;
      node.alt = payload.name || "图片";
      node.loading = "lazy";
    } else if (kind === "video") {
      node = document.createElement("video");
      node.src = url;
      node.controls = true;
      node.preload = "metadata";
      node.setAttribute("playsinline", "");
    } else if (kind === "audio") {
      node = document.createElement("audio");
      node.src = url;
      node.controls = true;
      node.preload = "metadata";
    } else if (kind === "pdf") {
      node = document.createElement("iframe");
      node.src = url;
      node.className = "pdf";
      node.title = payload.name || "PDF";
    } else {
      // 认不出的类型不做「附件下载」：新标签页打开，交给浏览器自己决定怎么呈现。
      node = document.createElement("a");
      node.href = url;
      node.target = "_blank";
      node.rel = "noopener";
      node.textContent = "📎 " + (payload.name || "打开文件");
    }
    box.appendChild(node);
    el.log.appendChild(box);
    scroll();
  }

  function setStatus(text, online) {
    el.status.textContent = text;
    el.status.className = "status " + (online ? "online" : "offline");
  }

  // 执行中状态：键在「发送」与「停止」之间切。**只认服务端的 turn 通知**——
  // 本地乐观切换会在「这一轮其实没跑起来」（空问题/缺身份被拒）时卡住。
  function setRunning(running) {
    state.running = !!running;
    el.send.innerHTML = state.running ? ICON_STOP : ICON_SEND;
    el.send.title = state.running ? "停止" : "发送";
    el.send.setAttribute("aria-label", state.running ? "停止" : "发送");
    el.send.classList.toggle("stop", state.running);
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
      setRunning(false); // 重连后按「空闲」起步；真在跑会马上收到 turn 通知
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
      // 推理块**不设限高、不自己滚动**——像工具 / 应答块一样整段向下长，留在原地。
      reasoningLine.textContent += data.text || "";
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
    source.addEventListener("usage", function (ev) {
      // 轮末的账目：每个会话只看自己的用量。过程事件（推理/工具）之后到。
      addNote("usage", usageText(JSON.parse(ev.data)));
      reasoningLine = null;
    });
    source.addEventListener("turn", function (ev) {
      // 执行中状态：发送键 ↔ 停止键
      setRunning(JSON.parse(ev.data).running);
    });
    source.addEventListener("error", function () {
      // EventSource 会自己重连；连不上时它也会触发这里
      setStatus("连接断开，重连中…", false);
    });
  }

  // ── 发送 / 停止 ─────────────────────────────────────────
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

  // 喊停当前会话进行中的一轮。键的回切靠服务端的 turn 通知，不在这里改。
  async function stop() {
    try {
      await fetch("/stop", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ conversation: state.conversation }),
      });
    } catch (e) {
      // 停不下就算了：这一轮自己会结束，turn 通知把键切回来
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

  // 一个键两种角色：不在跑就是「发送」，在跑就是「停止」。
  el.send.addEventListener("click", function () {
    if (state.running) { stop(); return; }
    el.form.requestSubmit();
  });

  el.text.addEventListener("keydown", function (ev) {
    if (ev.key === "Enter" && !ev.shiftKey) {
      ev.preventDefault();
      if (state.running) return; // 执行中 Enter 不发也不停，只有点停止键才停
      el.form.requestSubmit();
    }
  });

  el.connect.addEventListener("click", connect);

  // 初始化
  el.conversation.value = loadConversation();
  el.send.disabled = true;
  setRunning(false);
  el.token.focus();
})();
