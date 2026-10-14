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
    attach: document.getElementById("attach"),
    file: document.getElementById("file"),
    picker: document.getElementById("picker"),
    dropzone: document.getElementById("dropzone"),
  };

  var state = { conversation: "", source: null, running: false };
  // resources：本会话可 `#引用` 的媒体清单（服务端给）。null = 还没拉过（打开 `#`
  // 选择器时惰性拉取）；换会话、收到新文件、或自己发过文件后置 null，下次重拉。
  state.resources = null;
  state.resourcesLoading = false;
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
  // 「（共 …）」**；缓存命中是输入的一部分、贴在「入」后面；全零时服务端根本
  // 不推这一帧。
  function usageText(data) {
    var line = "入 " + (data.promptTokens || 0);
    if (data.cachedTokens) line += "（缓存命中 " + data.cachedTokens + "）";
    line += " / 出 " + (data.completionTokens || 0);
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

  // 用户自己发的文件：本地立刻回显。图片画出来，其它显示成一枚文件名标签。
  // 服务端收到后不会把这些再推回来（它只回文本/答复），所以这里必须自己画。
  function addUserFile(file) {
    var row = document.createElement("div");
    row.className = "row user";
    var bubble = document.createElement("div");
    bubble.className = "bubble";
    if (file.type && file.type.indexOf("image/") === 0) {
      var img = document.createElement("img");
      img.src = URL.createObjectURL(file);
      img.alt = file.name || "图片";
      bubble.appendChild(img);
    } else {
      var chip = document.createElement("span");
      chip.className = "filechip";
      chip.textContent = "📎 " + (file.name || "文件");
      bubble.appendChild(chip);
    }
    row.appendChild(bubble);
    el.log.appendChild(row);
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

  // ── `#` 引用选择器 ─────────────────────────────────────
  // 输入框里打 `#` 弹出**本会话媒体清单**（服务端 `GET /resources`），选中插入 `#名字`；
  // 名字含空格/标点时写成 `#"名字"`——与服务端的解析规则同构（见 resource.go）。
  var picker = { open: false, items: [], index: 0, token: null };

  // refTokenAt 看光标前那一段是不是个 `#引用`：从光标往前找词首的 `#`，到空白为止。
  function refTokenAt(value, pos) {
    var start = pos;
    while (start > 0 && !/\s/.test(value[start - 1])) start--;
    if (start < pos && value[start] === "#") {
      return { start: start, end: pos, query: value.slice(start + 1, pos) };
    }
    return null;
  }

  // needsQuote 名字里有空白或句读标点时要用引号包起来，否则解析会在那里断开。
  function needsQuote(name) { return /[\s，。！？、；：「」（）【】《》]/.test(name); }

  async function loadResources() {
    var list = [];
    if (state.conversation) {
      try {
        var res = await fetch("/resources?conversation=" + encodeURIComponent(state.conversation));
        if (res.ok) list = (await res.json()).resources || [];
      } catch (e) {
        list = [];
      }
    }
    state.resources = list;
  }

  // invalidateResources 让 `#引用` 清单下次打开时重拉（换会话、来新文件、自己发过文件）。
  function invalidateResources() { state.resources = null; state.resourcesLoading = false; }

  function closePicker() {
    picker.open = false;
    picker.items = [];
    picker.token = null;
    el.picker.hidden = true;
    el.picker.innerHTML = "";
  }

  function updatePicker() {
    var tok = refTokenAt(el.text.value, el.text.selectionStart);
    if (!tok) { closePicker(); return; }
    picker.token = tok;
    if (state.resources === null) {
      if (!state.resourcesLoading) {
        state.resourcesLoading = true;
        loadResources().then(function () {
          state.resourcesLoading = false;
          if (refTokenAt(el.text.value, el.text.selectionStart)) renderPicker();
        }, function () {
          state.resourcesLoading = false;
          closePicker();
        });
      }
      return; // 加载中先不渲染，免得闪一下空清单
    }
    renderPicker();
  }

  function renderPicker() {
    var q = (picker.token ? picker.token.query : "").toLowerCase();
    picker.items = (state.resources || []).filter(function (r) {
      return !q || (r.name || "").toLowerCase().indexOf(q) >= 0;
    });
    picker.index = 0;
    el.picker.innerHTML = "";
    if (!picker.items.length) {
      var empty = document.createElement("div");
      empty.className = "picker-empty";
      empty.textContent = (state.resources && state.resources.length)
        ? "没有匹配的文件" : "本会话还没有可引用的文件";
      el.picker.appendChild(empty);
      picker.open = false; // 无可选项：不拦 Enter
    } else {
      picker.items.forEach(function (r, i) {
        var row = document.createElement("div");
        row.className = "picker-item" + (i === picker.index ? " active" : "");
        var name = document.createElement("span");
        name.className = "picker-name";
        name.textContent = r.name || "";
        var kind = document.createElement("span");
        kind.className = "picker-kind";
        kind.textContent = r.kind === "image" ? "图片" : "文件";
        row.appendChild(name);
        row.appendChild(kind);
        row.addEventListener("mousedown", function (ev) { ev.preventDefault(); pickItem(i); });
        el.picker.appendChild(row);
      });
      picker.open = true;
    }
    placePicker();
    el.picker.hidden = false;
  }

  // placePicker 摆在输入框正上方（固定定位，跟着输入框走）。
  function placePicker() {
    var rect = el.text.getBoundingClientRect();
    el.picker.style.left = rect.left + "px";
    el.picker.style.width = rect.width + "px";
    el.picker.style.bottom = (window.innerHeight - rect.top + 6) + "px";
  }

  function movePicker(delta) {
    if (!picker.items.length) return;
    picker.index = (picker.index + delta + picker.items.length) % picker.items.length;
    var rows = el.picker.querySelectorAll(".picker-item");
    for (var i = 0; i < rows.length; i++) rows[i].classList.toggle("active", i === picker.index);
  }

  function pickItem(i) {
    var r = picker.items[i];
    if (!r) return;
    var tok = refTokenAt(el.text.value, el.text.selectionStart) || picker.token;
    if (!tok) return;
    var name = r.name || "";
    var insert = needsQuote(name) ? '#"' + name + '"' : "#" + name;
    var v = el.text.value;
    el.text.value = v.slice(0, tok.start) + insert + v.slice(tok.end);
    var caret = tok.start + insert.length;
    closePicker();
    el.text.focus();
    el.text.setSelectionRange(caret, caret);
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
    invalidateResources(); // 换会话：`#引用` 清单作废

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
      invalidateResources(); // 来了新文件
    });
    source.addEventListener("usage", function (ev) {
      // 轮末的账目：每个会话只看自己的用量。过程事件（推理/工具）之后到。
      addNote("usage", usageText(JSON.parse(ev.data)));
      reasoningLine = null;
    });
    source.addEventListener("stored", function (ev) {
      // 文件已落沙盒：服务端只给**结构化路径**，这里自己拼成一行系统提示（不是聊天气泡）。
      var paths = JSON.parse(ev.data).paths || [];
      var head = paths.length ? "已存入沙盒：" + paths.join("、") + "。" : "文件已存入沙盒。";
      addNote("stored", head + "请说要我拿它做什么。");
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

  // 发文件：走 multipart（字段 conversation/text + 若干 `file` 文件）。文本若也填了
  // 就一起带上。与服务端 `POST /messages` 的多段分支对应。**任意类型**都发——图片、
  // 文本、二进制由服务端与消息层分类。
  async function sendFiles(files) {
    if (!state.conversation) { addNote("error", "先连接再发文件"); return; }
    var picked = Array.prototype.slice.call(files).filter(function (f) { return !!f; });
    if (!picked.length) return;

    picked.forEach(addUserFile);
    var text = el.text.value.trim();
    el.text.value = "";

    var form = new FormData();
    form.append("conversation", state.conversation);
    if (text) form.append("text", text);
    picked.forEach(function (f) { form.append("file", f); });

    try {
      // **不手动设 Content-Type**：浏览器要给 multipart 补 boundary
      var res = await fetch("/messages", { method: "POST", body: form });
      if (!res.ok) {
        var detail = "";
        try { detail = (await res.json()).error || ""; } catch (e) {}
        addNote("error", "发送失败：" + detail);
      }
    } catch (err) {
      addNote("error", "发送失败");
    }
    invalidateResources(); // 自己发过文件
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
    closePicker();
    send(text).catch(function () { addNote("error", "发送失败"); });
  });

  // 一个键两种角色：不在跑就是「发送」，在跑就是「停止」。
  el.send.addEventListener("click", function () {
    if (state.running) { stop(); return; }
    el.form.requestSubmit();
  });

  el.text.addEventListener("keydown", function (ev) {
    // `#` 选择器开着时先归它：上下选、Enter/Tab 选中、Esc 关。
    if (picker.open && picker.items.length) {
      if (ev.key === "ArrowDown") { ev.preventDefault(); movePicker(1); return; }
      if (ev.key === "ArrowUp") { ev.preventDefault(); movePicker(-1); return; }
      if (ev.key === "Enter" || ev.key === "Tab") { ev.preventDefault(); pickItem(picker.index); return; }
      if (ev.key === "Escape") { ev.preventDefault(); closePicker(); return; }
    }
    if (ev.key === "Enter" && !ev.shiftKey) {
      ev.preventDefault();
      if (state.running) return; // 执行中 Enter 不发也不停，只有点停止键才停
      el.form.requestSubmit();
    }
  });

  el.text.addEventListener("input", updatePicker);
  el.text.addEventListener("blur", function () { setTimeout(closePicker, 120); });

  el.connect.addEventListener("click", connect);

  // 选文件 / 粘贴：立刻作为一条带文件的消息发出（文本若填了也一起带）。**选文件靠原生
  // `<label for>` 触发**（不靠脚本点隐藏 input——Safari 会忽略），这里只在执行中
  // 拦一下（preventDefault 阻止弹选择框）。粘贴走 paste 事件（多为截图）。
  el.attach.addEventListener("click", function (ev) {
    if (state.running) {
      ev.preventDefault();
      addNote("error", "正在回答，等这一轮结束再发");
    }
  });
  el.file.addEventListener("change", function () {
    sendFiles(el.file.files).catch(function () { addNote("error", "发送失败"); });
    el.file.value = "";
  });
  document.addEventListener("paste", function (ev) {
    var items = (ev.clipboardData && ev.clipboardData.items) || [];
    var files = [];
    for (var i = 0; i < items.length; i++) {
      if (items[i].kind === "file") {
        var f = items[i].getAsFile();
        if (f) files.push(f);
      }
    }
    if (files.length) sendFiles(files).catch(function () { addNote("error", "发送失败"); });
  });

  // 拖拽文件进页面：整页都是放置区，拖进来时盖一层提示，松开即作为一条带文件的消息发出。
  // **只认带文件的拖拽**（拖文字、拖链接不拦）；dragenter/dragleave 会在子元素间反复触发，
  // 用计数抵消，计数归零才收起遮罩。
  function draggingFiles(ev) {
    var types = (ev.dataTransfer && ev.dataTransfer.types) || [];
    for (var i = 0; i < types.length; i++) {
      if (types[i] === "Files") return true;
    }
    return false;
  }
  var dragDepth = 0;
  window.addEventListener("dragenter", function (ev) {
    if (!draggingFiles(ev)) return;
    ev.preventDefault();
    dragDepth++;
    el.dropzone.hidden = false;
  });
  window.addEventListener("dragover", function (ev) {
    if (!draggingFiles(ev)) return;
    ev.preventDefault(); // 必须：不拦的话浏览器不把这里当放置区
    if (ev.dataTransfer) ev.dataTransfer.dropEffect = "copy";
  });
  window.addEventListener("dragleave", function (ev) {
    if (!draggingFiles(ev)) return;
    dragDepth = Math.max(0, dragDepth - 1);
    if (dragDepth === 0) el.dropzone.hidden = true;
  });
  window.addEventListener("drop", function (ev) {
    if (!draggingFiles(ev)) return;
    ev.preventDefault(); // 别让浏览器直接打开这个文件
    dragDepth = 0;
    el.dropzone.hidden = true;
    var files = Array.prototype.slice.call((ev.dataTransfer && ev.dataTransfer.files) || []);
    if (!files.length) return;
    if (state.running) { addNote("error", "正在回答，等这一轮结束再发"); return; }
    if (!state.conversation) { addNote("error", "先连接再发文件"); return; }
    sendFiles(files).catch(function () { addNote("error", "发送失败"); });
  });

  // 初始化
  el.conversation.value = loadConversation();
  el.send.disabled = true;
  setRunning(false);
  el.token.focus();
})();
