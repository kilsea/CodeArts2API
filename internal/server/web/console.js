(function(){
  "use strict";
  var $ = function(s){ return document.querySelector(s); };
  var state = { data:null };
  // 服务端是否设置了管理密码（由 /admin/api/state 同步）。
  // 未设置时管理接口走「本机直连」兜底，右上角不能显示「未解锁」——
  // 那会让人以为鉴权坏了（实际全部可用）。
  var serverHasPw = false;
  // 设置表单是否被用户改过：改过之后 refresh() 不得用服务端值覆盖。
  var settingsDirty = false;

  function toast(msg, kind){
    var t = $("#toast");
    t.textContent = msg;
    t.className = "toast show " + (kind||"");
    // 错误必须能被读到：切到 alert 且不自动消失（点击关闭）。
    t.setAttribute("role", kind === "err" ? "alert" : "status");
    t.setAttribute("aria-live", kind === "err" ? "assertive" : "polite");
    clearTimeout(toast._t);
    if (kind !== "err") {
      toast._t = setTimeout(function(){ t.className = "toast"; }, 4600);
    }
  }
  function pw(){ return $("#pw").value.trim(); }

  // 管理密码只放内存 + sessionStorage（当前标签页），绝不再写 localStorage：
  // 明文密码落在 Local Storage 里，任何同源脚本或本机进程都能直接读走，
  // 而它等价于「导出完整登录态」的权限。
  var PW_KEY = "codearts2api_pw";
  var pwMigrated = false;
  function loadPw(){
    var v = "";
    try {
      if (localStorage.getItem(PW_KEY) !== null) {
        localStorage.removeItem(PW_KEY);
        pwMigrated = true;
      }
      v = sessionStorage.getItem(PW_KEY) || "";
    } catch(e){}
    return v;
  }
  function storePw(v){
    try {
      if (v) sessionStorage.setItem(PW_KEY, v);
      else sessionStorage.removeItem(PW_KEY);
    } catch(e){}
  }
  function clearStoredPw(){
    try { localStorage.removeItem(PW_KEY); sessionStorage.removeItem(PW_KEY); } catch(e){}
  }

  function api(path, opts){
    opts = opts || {};
    var h = { "Content-Type":"application/json" };
    if (pw()) h["X-Admin-Password"] = pw();
    return fetch(path, {
      method: opts.method || "GET",
      headers: h,
      body: opts.body ? JSON.stringify(opts.body) : undefined
    }).then(function(r){
      return r.text().then(function(txt){
        var j = null;
        try { j = JSON.parse(txt); } catch(e){}
        if (!r.ok) {
          var m = (j && j.error && j.error.message) || txt || ("HTTP " + r.status);
          var err = new Error(m); err.status = r.status; throw err;
        }
        return j;
      });
    });
  }

  // --- DOM 构建助手（全程 textContent，绝不拼接 HTML） ---
  function mk(tag, cls, text){
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = String(text);
    return n;
  }
  function cell(text, cls){ return mk("td", cls, text); }
  function row(cells){
    var r = mk("tr");
    for (var i = 0; i < cells.length; i++) r.appendChild(cells[i]);
    return r;
  }
  function pill(text, cls){ return mk("span", "badge " + (cls||""), text); }
  function emptyRow(cols, text){
    var c = cell(text, "empty");
    c.colSpan = cols;
    var r = mk("tr");
    r.appendChild(c);
    return r;
  }
  function btn(label, cls, fn){
    var b = mk("button", "btn " + (cls||""), label);
    b.type = "button";
    b.onclick = busyable(b, fn);
    return b;
  }

  // --- 行内「更多」菜单 ---
  //
  // 账号行的次要操作收进这里。为什么不用绝对定位的下拉：表格容器是
  // overflow-x:auto，会直接把下拉裁掉（还会撑出内部滚动条）。所以菜单挂在
  // body 上、用 fixed 定位，坐标按触发按钮实时算；滚动、缩放、点空白、Esc 都关。
  var SVGNS = "http://www.w3.org/2000/svg";
  var rowMenuEl = null, rowMenuAnchor = null;

  function svgIcon(ds){
    var s = document.createElementNS(SVGNS, "svg");
    s.setAttribute("class", "ico");
    s.setAttribute("viewBox", "0 0 24 24");
    s.setAttribute("aria-hidden", "true");
    ds.forEach(function(d){
      var p = document.createElementNS(SVGNS, "path");
      p.setAttribute("d", d);
      s.appendChild(p);
    });
    return s;
  }

  function moreBtn(buildItems){
    var b = mk("button", "btn xs menu-btn");
    b.type = "button";
    b.title = "更多操作";
    b.setAttribute("aria-label", "更多操作");
    b.setAttribute("aria-haspopup", "menu");
    b.setAttribute("aria-expanded", "false");
    b.appendChild(svgIcon(["M5 12h.01", "M12 12h.01", "M19 12h.01"]));
    b.onclick = function(ev){
      ev.stopPropagation();
      if (rowMenuEl && rowMenuAnchor === b) { closeRowMenu(true); return; }
      closeRowMenu(false);
      openRowMenu(b, buildItems());
    };
    return b;
  }

  function openRowMenu(anchor, items){
    var el = mk("div", "row-menu");
    el.setAttribute("role", "menu");
    items.forEach(function(it){
      if (it.sep) { el.appendChild(mk("div", "sep")); return; }
      var b = mk("button", "mi" + (it.danger ? " danger" : ""), it.label);
      b.type = "button";
      b.setAttribute("role", "menuitem");
      b.onclick = function(){ closeRowMenu(false); it.fn(); };
      el.appendChild(b);
    });
    // 先挂上再量尺寸，才能决定向上还是向下展开。
    el.style.visibility = "hidden";
    document.body.appendChild(el);
    var r = anchor.getBoundingClientRect();
    var w = el.offsetWidth, h = el.offsetHeight;
    var vw = window.innerWidth, vh = window.innerHeight;
    var left = Math.min(Math.max(8, r.right - w), Math.max(8, vw - w - 8));
    var top = r.bottom + 6;
    if (top + h > vh - 8) top = Math.max(8, r.top - h - 6);
    el.style.left = left + "px";
    el.style.top = top + "px";
    el.style.visibility = "";
    rowMenuEl = el;
    rowMenuAnchor = anchor;
    anchor.setAttribute("aria-expanded", "true");
    document.addEventListener("mousedown", onMenuDocDown, true);
    document.addEventListener("keydown", onMenuKey, true);
    window.addEventListener("scroll", closeRowMenu, true);
    window.addEventListener("resize", closeRowMenu);
    var first = el.querySelector("button");
    if (first) first.focus();
  }

  function onMenuDocDown(ev){
    if (!rowMenuEl) return;
    if (rowMenuEl.contains(ev.target)) return;
    if (rowMenuAnchor && (ev.target === rowMenuAnchor || rowMenuAnchor.contains(ev.target))) return;
    closeRowMenu(false);
  }

  function onMenuKey(ev){
    if (ev.key === "Escape") closeRowMenu(true);
  }

  // closeRowMenu 同时作为 scroll / resize 监听器：那时第一个参数不是布尔值。
  function closeRowMenu(restoreFocus){
    if (!rowMenuEl) return;
    var anchor = rowMenuAnchor;
    rowMenuEl.remove();
    rowMenuEl = null;
    rowMenuAnchor = null;
    if (anchor) {
      anchor.setAttribute("aria-expanded", "false");
    }
    document.removeEventListener("mousedown", onMenuDocDown, true);
    document.removeEventListener("keydown", onMenuKey, true);
    window.removeEventListener("scroll", closeRowMenu, true);
    window.removeEventListener("resize", closeRowMenu);
    if (restoreFocus === true && anchor) anchor.focus();
  }

  // --- 就地更新助手 -------------------------------------------------------
  // 自动刷新每 5 秒跑一次，绝大多数值跟上一次完全一样。无条件给 textContent /
  // value 赋值会让浏览器先删旧的文本节点再插一个新的：视觉上是「页面自己闪一下」，
  // 还会打断选中、把正在编辑的输入框光标顶到末尾。渲染函数一律走这三个守卫，
  // 只有值真的变了才碰 DOM。
  function setText(el, v){
    if (!el) return;
    var s = (v === undefined || v === null) ? "" : String(v);
    if (el.textContent !== s) el.textContent = s;
  }
  function setValue(el, v){
    if (!el) return;
    var s = (v === undefined || v === null) ? "" : String(v);
    if (el.value !== s) el.value = s;
  }
  function setChecked(el, v){
    if (el && el.checked !== !!v) el.checked = !!v;
  }
  function setAttr(el, name, v){
    if (!el) return;
    var s = (v === undefined || v === null) ? "" : String(v);
    if (el.getAttribute(name) !== s) el.setAttribute(name, s);
  }
  function setClass(el, cls){
    if (el && el.className !== cls) el.className = cls;
  }

  // busyable 在异步操作期间禁用按钮，避免双击造成重复提交
  // （重复签到 / 重复创建密钥 / 重复删除）。
  function busyable(el, fn){
    return function(ev){
      if (el && el.disabled) return;
      var r = fn.call(this, ev);
      if (el && r && typeof r.then === "function") {
        el.disabled = true;
        el.setAttribute("aria-busy", "true");
        r.then(function(){}, function(){}).then(function(){
          el.disabled = false;
          el.removeAttribute("aria-busy");
        });
      }
      return r;
    };
  }
  // on 给静态按钮绑定处理器并自动加 in-flight 保护。
  function on(id, fn){
    var el = $(id);
    el.onclick = busyable(el, fn);
    return el;
  }

  function legacyCopy(text){
    var ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.className = "sr-only";
    document.body.appendChild(ta);
    ta.select();
    var ok = false;
    try { ok = document.execCommand("copy"); } catch(e){ ok = false; }
    document.body.removeChild(ta);
    return ok;
  }
  // copyText 优先用异步剪贴板 API（127.0.0.1 属于安全上下文），
  // 失败再回退到已废弃的 execCommand —— 内网用 http://<IP>:<端口> 访问控制台时
  // navigator.clipboard 在非安全上下文里根本不存在，只靠它等于按钮失灵。
  // 返回 Promise<boolean>，调用方需要知道成没成（比如「只显示一次」的密钥）。
  function copyText(text, okMsg){
    var done = function(ok){
      toast(ok ? (okMsg || "已复制") : "复制失败，请手动选择后按 Ctrl+C", ok ? "ok" : "err");
      return ok;
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text).then(
        function(){ return done(true); },
        function(){ return done(legacyCopy(text)); });
    }
    return Promise.resolve(done(legacyCopy(text)));
  }

  function fmtTime(s){
    if (!s) return "—";
    var d = new Date(s * 1000);
    var p = function(n){ return (n<10?"0":"")+n; };
    return d.getFullYear()+"-"+p(d.getMonth()+1)+"-"+p(d.getDate())+" "+p(d.getHours())+":"+p(d.getMinutes())+":"+p(d.getSeconds());
  }
  function fmtDur(sec){
    if (sec === undefined || sec === null) return "—";
    if (sec <= 0) return "已过期";
    var h = Math.floor(sec/3600), m = Math.floor((sec%3600)/60);
    if (h > 0) return h + " 小时 " + m + " 分";
    return m + " 分 " + (sec%60) + " 秒";
  }
  // fmtCredits 把积分格式化成两位小数；0 / 空值显示「—」。
  //
  // 上游账单本身就是两位小数，这里不再做四舍五入以外的处理；
  // 估算值在日志表里会带「≈」前缀（见 logRow）。
  function fmtCredits(v){
    var n = Number(v) || 0;
    if (n <= 0) return "—";
    return n.toFixed(2);
  }

  function fmtNum(n){
    if (n === undefined || n === null) return "—";
    return String(n).replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  }
  // fmtShort 给 KPI 卡片用的紧凑时间：10-16 08:19（完整值放在 title 里）
  function fmtShort(s){
    if (!s) return "—";
    var d = new Date(s * 1000);
    var p = function(n){ return (n < 10 ? "0" : "") + n; };
    return (d.getMonth() + 1) + "-" + p(d.getDate()) + " " + p(d.getHours()) + ":" + p(d.getMinutes());
  }
  // syncEvery 把「定时同步间隔（秒）」说成人话：6h → 6 小时。
  function syncEvery(sec){
    if (sec >= 3600) {
      var h = sec / 3600;
      return (h % 1 ? h.toFixed(1) : String(h)) + " 小时";
    }
    if (sec >= 60) return Math.round(sec / 60) + " 分钟";
    return sec + " 秒";
  }

  function setHealth(cls, text){
    var el = $("#health");
    // 侧栏状态徽标每 20 秒刷一次：结构一次建好，之后只改类名和文字。
    if (!el.children.length) {
      el.appendChild(mk("i", "dot"));
      el.appendChild(mk("span"));
    }
    setClass(el, "badge " + cls);
    setAttr(el, "title", text);
    setText(el.children[1], text);
  }

  // renderUptime 在侧栏底部显示「服务已运行多久」。
  //
  // 放在侧栏而不是概览：它是环境信息（和构建号、健康状态同类），
  // 不该在首屏占一格 KPI。刷新时顺手更新，跨天/跨小时自动换单位。
  function renderUptime(sec){
    sec = Math.floor(Number(sec) || 0);
    if (!(sec > 0)) { setText($("#uptime"), ""); return; }
    var days = Math.floor(sec / 86400);
    var hours = Math.floor((sec % 86400) / 3600);
    var mins = Math.floor((sec % 3600) / 60);
    var txt = days > 0 ? days + " 天 " + hours + " 小时"
      : hours > 0 ? hours + " 小时 " + mins + " 分"
      : mins > 0 ? mins + " 分"
      : sec + " 秒";
    setText($("#uptime"), "已运行 " + txt);
    setAttr($("#uptime"), "title", "本次进程启动至今，重启即归零");
  }

  function loadHealth(){
    fetch("/health").then(function(r){ return r.json(); }).then(function(h){
      var ready = h.ready || 0, total = h.accounts || 0;
      var ok = total > 0 && ready > 0;
      setHealth(ok ? "ok" : (total === 0 ? "warn" : "err"),
        "账号 "+ready+"/"+total+" 就绪 · 模型 "+(h.models||0));
    }).catch(function(){ setHealth("err", "不可达"); });
  }

  // KPI 卡的标题是固定的，只有数值和副标题会变：复用已有卡片、只就地改文本。
  // 整块重建会在每次有流量时闪一下，还会打断鼠标悬停。
  function renderKpis(kpis){ renderKpisInto("#kpis", kpis); }

  // KPI_MIN 是单张 KPI 卡的最小舒适宽度（再窄数字和标题就挤了）。
  var KPI_MIN = 180;

  // layoutKpis 按「容器宽度 + 卡片数量」算列数，写进 --cols。
  //
  // 目标是两条同时成立：
  //   1. 每张卡不至于宽得离谱（上限由 KPI_MIN 决定）；
  //   2. 每一行都铺满 —— 先算最多几列，再把列数压到「每行数量尽量均匀」，
  //      例如 7 张卡在 5 列上限下排成 4 + 3，而不是 5 + 2。
  function layoutKpis(box){
    if (!box || !box.children.length) return;
    var n = box.children.length;
    var gap = 12;
    var w = box.clientWidth || 0;
    if (!w) return;
    var maxCols = Math.max(1, Math.floor((w + gap) / (KPI_MIN + gap)));
    if (maxCols > n) maxCols = n;
    var rows = Math.ceil(n / maxCols);
    var cols = Math.ceil(n / rows);
    box.style.setProperty("--cols", cols);
  }

  // renderKpisInto 把 KPI 列表画进指定容器（概览与用量页各一组）。
  function renderKpisInto(sel, kpis){
    var box = $(sel);
    if (box.children.length !== kpis.length) {
      box.textContent = "";
      kpis.forEach(function(){
        var c = mk("div", "kpi");
        c.appendChild(mk("div", "k"));
        c.appendChild(mk("div", "v"));
        c.appendChild(mk("div", "sub"));
        box.appendChild(c);
      });
    }
    kpis.forEach(function(item, i){
      var c = box.children[i];
      var sub = c.children[2];
      setText(c.children[0], item.k);
      setText(c.children[1], item.v);
      setText(sub, item.sub || "");
      if (sub.hidden !== !item.sub) sub.hidden = !item.sub;
      setAttr(c, "title", item.title || "");
    });
    layoutKpis(box);
  }

  // 窗口尺寸变了要重算列数（KPI 卡的宽度是百分比，列数必须跟着容器走）。
  var kpiResizeTimer = null;
  window.addEventListener("resize", function(){
    if (kpiResizeTimer) clearTimeout(kpiResizeTimer);
    kpiResizeTimer = setTimeout(function(){
      layoutKpis($("#kpis"));
      layoutKpis($("#usageKpis"));
    }, 120);
  });

  // 余额图是像素坐标画的（圆点不能被拉伸），窗口变宽要按新宽度重绘。
  var balResizeTimer = null;
  window.addEventListener("resize", function(){
    if (balResizeTimer) clearTimeout(balResizeTimer);
    balResizeTimer = setTimeout(function(){
      var tab = $("#tab-balance");
      if (balLastData && tab && tab.classList.contains("on")) renderBalance(balLastData);
    }, 150);
  });

  // 概览页拆成两路：KPI 跟着请求计数变，模型表只在上游清单变时重建。
  // 以前两者共用一个指纹，发一次请求就把整张模型表重建一遍（12 行 DOM）。
  function renderKpiCards(d){
    var s = d.stats || {};
    var accts = d.accounts || [];
    var ready = accts.filter(function(a){ return a.enabled && a.status === "ok"; }).length;
    var soonest = null;
    accts.forEach(function(a){
      if (a.expires_at && (soonest === null || a.expires_at < soonest)) soonest = a.expires_at;
    });
    // 概览只放总体口径：明细（按小时 / 按模型 / 按账号 / 每日）在「用量」页。
    // 密钥数、今日领取、模型数、启动时间都不在这里 —— 它们在各自的页签/侧栏里，
    // 首屏放太多一格一格的数字反而不好读。
    renderKpis([
      { k:"总请求", v: fmtNum(s.total_requests || 0) },
      { k:"失败请求", v: fmtNum(s.failed_requests || 0) },
      { k:"累计输入 token", v: fmtNum(s.total_in_tokens || 0) },
      { k:"累计输出 token", v: fmtNum(s.total_out_tokens || 0) },
      { k:"累计积分", v: fmtCredits(s.total_credits || 0),
        sub: "详见「用量」页", title: "逐请求上游账单口径（查不到时本地估算）" },
      { k:"账号（就绪/总）", v: ready + " / " + accts.length,
        sub: soonest ? "最早过期 " + fmtShort(soonest) : "", title: soonest ? fmtTime(soonest) : "" }
    ]);
  }

  function renderModels(d){
    var tb = $("#models");
    tb.textContent = "";
    var models = d.models || [];
    // 每次渲染重建元数据索引：上游清单可能删掉了模型，留着旧条目会让
    // 「测活全部」去探测已经不存在的模型，并让单行重绘拿到 undefined。
    lastModels = {};
    if (!models.length) { tb.appendChild(emptyRow(7, "无")); return; }
    models.forEach(function(m){
      lastModels[m.id] = m;
      tb.appendChild(modelRow(m));
    });
    // 清单由后台定时同步：把「上次同步 + 周期」写出来，免得以为列表不动就是坏了。
    var hint = "";
    if (d.upstream_error) {
      hint = "最近一次上游刷新失败：" + d.upstream_error;
    } else if (d.models_loaded_at) {
      hint = "清单同步于 " + fmtShort(d.models_loaded_at);
      if (d.models_sync_seconds > 0) hint += "，每 " + syncEvery(d.models_sync_seconds) + "自动同步";
    }
    setText($("#modelHint"), hint);
  }

  // 每个模型的测活状态：id -> {state:"idle|busy|ok|err", err}
  var modelProbeState = {};
  // 最近一次渲染的模型元数据，供单行重绘时复用
  var lastModels = {};

  function modelRow(m){
    var actionCell = cell("", "actions-cell");
    // 别名只是名字映射，指向的仍是上游真实模型，单独测活没有意义。
    if (m.source === "alias") {
      actionCell.appendChild(mk("span", "hint m-0", "别名"));
      return row([
        cell(m.id, "mono"), cell(m.name),
        cell(m.credit || "—", "mono"),
        cell(m.context ? fmtNum(m.context) : "—", "mono"),
        cell(m.max_output ? fmtNum(m.max_output) : "—", "mono"),
        cell(m.description, "muted"),
        actionCell
      ]);
    }

    var btn = mk("button", "btn xs", "测活");
    var st = modelProbeState[m.id] || { state: "idle" };
    if (st.state === "busy") {
      btn.disabled = true;
      btn.textContent = "测活中…";
    } else if (st.state === "ok" || st.state === "err") {
      btn.textContent = "重测";
    }
    btn.onclick = function(){ probeOne(m.id, m.name || m.id); };

    actionCell.appendChild(btn);
    if (st.state === "ok") {
      actionCell.appendChild(document.createTextNode(" "));
      actionCell.appendChild(pill("可用", "ok"));
    } else if (st.state === "err") {
      actionCell.appendChild(document.createTextNode(" "));
      var bad = pill("不可用", "err");
      bad.title = st.err || "";
      actionCell.appendChild(bad);
    }

    var r = row([
      cell(m.id, "mono"), cell(m.name),
      cell(m.credit || "—", "mono"),
      cell(m.context ? fmtNum(m.context) : "—", "mono"),
      cell(m.max_output ? fmtNum(m.max_output) : "—", "mono"),
      cell(m.description, "muted"),
      actionCell
    ]);
    if (st.state === "err" && st.err) r.title = st.err;
    return r;
  }

  // probeOne 对单个模型测活，只重绘那一行。
  function probeOne(modelID, label){
    if (modelProbeState[modelID] && modelProbeState[modelID].state === "busy") return;
    modelProbeState[modelID] = { state: "busy" };
    redrawModelRow(modelID);
    $("#modelHint").textContent = "正在测活 " + label + "…";

    return api("/admin/api/models/probe", {method:"POST", body:{models:[modelID]}}).then(function(r){
      var row0 = (r.results || [])[0] || {};
      modelProbeState[modelID] = row0.ok
        ? { state: "ok", account: row0.account }
        : { state: "err", err: row0.error || "未知错误", account: row0.account };
      redrawModelRow(modelID);
      $("#modelHint").textContent = row0.ok
        ? (label + " 可用" + (row0.account ? "（账号 " + row0.account + "）" : ""))
        : (label + " 不可用：" + (row0.error || "未知错误"));
    }).catch(function(e){
      modelProbeState[modelID] = { state: "err", err: (e && e.message) || "请求失败" };
      redrawModelRow(modelID);
      $("#modelHint").textContent = label + " 测活失败：" + ((e && e.message) || "未知错误");
    });
  }

  // redrawModelRow 只替换指定模型那一行，避免整表重绘打断其他行。
  function redrawModelRow(modelID){
    var meta = lastModels[modelID];
    if (!meta) return; // 清单已刷新、该模型不在了，无事可做
    var tb = $("#models");
    var rows = tb.children;
    for (var i = 0; i < rows.length; i++) {
      var first = rows[i].firstChild;
      if (first && first.textContent === modelID) {
        tb.replaceChild(modelRow(meta), rows[i]);
        return;
      }
    }
  }

  // 每个账号的积分额度：id -> {loading, ok, credit, error}
  var creditState = {};
  var creditLoading = null;
  // 上次自动查询额度的时间：账号页开着时每 5 秒一轮，不能每轮都打上游。
  var creditFetchedAt = 0;
  var CREDIT_REFRESH_EVERY = 5 * 60 * 1000;

  // loadCredits 查询账号积分额度（上游统计插件）。
  // 默认走服务端 60 秒缓存；force=true 时带 ?fresh=1 强制回源。
  function loadCredits(force){
    if (creditLoading) return creditLoading;
    var accts = (state.data && state.data.accounts) || [];
    if (!accts.length) return Promise.resolve();
    // 自动刷新 5 分钟一次就够：服务端虽然还有 60 秒缓存，但账号页开一整天
    // 就是上千次上游请求（每次额度查询都是真实的 snap-manager 调用）。
    // 手动「刷新」按钮走 force，立即回源。
    var now = Date.now();
    if (!force && creditFetchedAt && now - creditFetchedAt < CREDIT_REFRESH_EVERY) {
      return Promise.resolve();
    }
    creditFetchedAt = now;
    // 已经有了旧值就不要退回去显示「查询中…」： 5 秒一轮的自动刷新如果每次都
    // 清空再填，账号表会一直闪。只有第一次（还没有任何额度快照时）才置 loading。
    var seeded = false;
    accts.forEach(function(a){
      if (!creditState[a.id]) { creditState[a.id] = { loading: true }; seeded = true; }
    });
    if (seeded) updateCreditCells();
    var url = "/admin/api/credits" + (force ? "?fresh=1" : "");
    creditLoading = api(url).then(function(d){
      (d.accounts || []).forEach(function(x){
        creditState[x.id] = x.ok
          ? { loading:false, ok:true, credit:x.credit }
          : { loading:false, ok:false, error:x.error };
      });
      creditFetchedAt = Date.now();
      updateCreditCells();
    }).catch(function(e){
      // 失败别等满 5 分钟：置成「快到期」，下一轮（约 1 分钟后）再试一次。
      creditFetchedAt = Date.now() - CREDIT_REFRESH_EVERY + 60 * 1000;
      var msg = (e && e.message) || "查询失败";
      accts.forEach(function(a){ creditState[a.id] = { loading:false, ok:false, error:msg }; });
      updateCreditCells();
      toast("查询积分额度失败：" + msg, "err");
    }).then(function(){ creditLoading = null; });
    return creditLoading;
  }

  // usageByAccount 是「今日各账号消耗积分」（来自 /admin/api/usage?days=1）。
  // 账号页靠它在额度格子里补一句「今日消耗」，与快照的累计已用互补。
  var usageByAccount = {};

  function loadUsageForAccounts(){
    return api("/admin/api/usage?days=1").then(function(d){
      var m = {};
      (d.by_account || []).forEach(function(r){
        if (r.account_id) m[r.account_id] = (m[r.account_id] || 0) + (r.credits || 0);
      });
      usageByAccount = m;
      updateCreditCells();
    }).catch(function(){ /* 静默：额度本身更重要 */ });
  }

  // updateCreditCells 只重写账号表里那一个「积分额度」单元格。
  // 以前这里是 renderAccounts(整表重建) —— 自动刷新每 5 秒跑两遍，
  // 表格会闪、滚动位置和 hover 也会被刷掉。
  function updateCreditCells(){
    var cells = document.querySelectorAll("#accounts td[data-credit]");
    for (var i = 0; i < cells.length; i++) {
      var acct = findAccount(cells[i].getAttribute("data-credit"));
      if (acct) fillCredit(cells[i], acct);
    }
  }
  function findAccount(id){
    var list = (state.data && state.data.accounts) || [];
    for (var i = 0; i < list.length; i++) if (list[i].id === id) return list[i];
    return null;
  }

  // creditCell 渲染一个账号的积分额度：剩余/总量 + 进度条 + 重置日期。
  // 结构一次建好（值 / 进度条 / 明细），后续刷新只改文本和宽度，不重建节点。
  function creditCell(a){
    var c = cell("", "cell-stack");
    c.setAttribute("data-credit", a.id);
    c.appendChild(mk("span"));
    var bar = mk("div", "bar credit-bar");
    bar.appendChild(mk("i"));
    c.appendChild(bar);
    c.appendChild(mk("span", "sub"));
    fillCredit(c, a);
    return c;
  }

  function fillCredit(c, a){
    var val = c.children[0], bar = c.children[1], sub = c.children[2];
    var st = creditState[a.id];
    // 出错/无套餐/加载中：只有一行字，进度条和明细收起。
    function plain(cls, text, title){
      setClass(val, cls);
      setText(val, text);
      setAttr(val, "title", title || "");
      bar.hidden = true;
      sub.hidden = true;
    }
    if (!st || st.loading) { plain("muted", st ? "查询中…" : "—"); return; }
    if (!st.ok) { plain("muted", "查询失败", st.error); return; }
    var info = st.credit || {};
    if (!info.available) { plain("muted", "无积分套餐", info.reason); return; }

    var total = Number(info.total) || 0;
    var remain = Number(info.remaining) || 0;
    var used = Number(info.used) || 0;
    var pct = total > 0 ? Math.max(0, Math.min(100, (remain / total) * 100)) : 0;
    setClass(val, "mono");
    setText(val, fmtNum(remain) + " / " + fmtNum(total));
    setAttr(val, "title", "");
    bar.hidden = false;
    setClass(bar, "bar credit-bar" + (pct <= 20 ? " err" : (pct <= 50 ? " warn" : "")));
    bar.children[0].style.width = pct.toFixed(1) + "%";

    var bits = ["已用 " + fmtNum(used)];
    if (usageByAccount[a.id] > 0) bits.push("今日 " + fmtCredits(usageByAccount[a.id]) + " 积分");
    if (info.expiring) bits.push("即将到期 " + fmtNum(info.expiring));
    if (info.reset_date) bits.push("重置 " + info.reset_date);
    if (info.status && info.status !== "active") bits.push(info.status);
    sub.hidden = false;
    setText(sub, bits.join(" · "));
    setAttr(sub, "title", (info.package_name || "") + (info.spec_code ? (" / " + info.spec_code) : ""));
  }

  function renderAccounts(d){
    // 整表重建会把菜单锚点一起换掉，先把打开的菜单收掉。
    closeRowMenu(false);
    var tb = $("#accounts");
    tb.textContent = "";
    var accts = d.accounts || [];
    if (!accts.length) {
      tb.appendChild(emptyRow(11, "暂无账号，点击「导入本机 CodeArts Agent 登录态」或在下方手工录入"));
      return;
    }
    accts.forEach(function(a){
      var statusCell = cell("");
      if (a.enabled) {
        var okStatus = a.status === "ok";
        statusCell.appendChild(pill(okStatus ? "正常" : "异常", okStatus ? "ok" : "err"));
        // 失败账号会进入冷却，冷却期内不参与轮转；到期自动做一次半开探测。
        if (!okStatus && a.cooldown_until) {
          var cdLeft = a.cooldown_until * 1000 - Date.now();
          var cd = mk("span", "sub", cdLeft > 0 ? "冷却 " + fmtDur(Math.ceil(cdLeft / 1000)) : "待探测");
          cd.title = cdLeft > 0
            ? "冷却期内不参与轮转，到期自动放一次探测请求，成功即恢复 ok"
            : "冷却已到期，下次轮询会放一次探测请求";
          statusCell.appendChild(document.createTextNode(" "));
          statusCell.appendChild(cd);
        }
      } else {
        statusCell.appendChild(pill("已停用"));
      }
      if (a.has_refresh) statusCell.appendChild(document.createTextNode(" "));
      if (a.has_refresh) statusCell.appendChild(pill("可续期", "ok"));
      if (a.enabled && a.credit_expiring > 0) {
        var cp = pill("到期优先", "ok");
        cp.title = "积分快照显示有 " + a.credit_expiring + " 积分即将到期（重置 " + (a.credit_reset_date || "—") + "）；开启「到期积分优先调度」时该账号会被优先调度";
        statusCell.appendChild(document.createTextNode(" "));
        statusCell.appendChild(cp);
      }

      var expCell = cell("", "cell-stack");
      if (a.permanent) {
        // 永久 AK/SK：没有过期时间，也不参与续期。
        expCell.appendChild(mk("span", "mono", "永不过期"));
        expCell.appendChild(mk("span", "sub", "IAM AK/SK"));
      } else if (a.expires_at) {
        expCell.appendChild(mk("span", "mono", fmtTime(a.expires_at)));
        var left = mk("span", "sub", fmtDur(a.expires_in));
        if (a.expires_in !== undefined && a.expires_in <= 300) left.classList.add("warn-text");
        expCell.appendChild(left);
      } else {
        expCell.appendChild(mk("span", "mono", "—"));
      }

      var claimCell = cell("");
      if (a.claimed_today) {
        claimCell.appendChild(pill("已领", "ok"));
      } else {
        claimCell.appendChild(pill("未领", "warn"));
      }
      if (a.last_claim_msg) claimCell.title = a.last_claim_msg;
      if (a.last_claim_err) claimCell.title = a.last_claim_err;

      var ops = cell("", "actions-cell");
      ops.appendChild(btn("探活", "xs", function(){ checkin(a.id); }));
      ops.appendChild(btn("领福利", "xs", function(){ claim(a.id); }));
      ops.appendChild(moreBtn(function(){
        var items = [
          { label:"重读登录态", fn:function(){ accountOp("reimport", {id:a.id}); } },
          { label:"导出 session", fn:function(){ exportSession(a.id); } },
          { label:"备注…", fn:function(){
            var v = prompt("账号备注（如注册手机号）", a.note || "");
            if (v === null) return;
            accountOp("set-note", {id:a.id, note:v.trim()});
          } }
        ];
        if (a.failures > 0) {
          items.push({ label:"清除错误", fn:function(){ accountOp("clear-errors", {id:a.id}); } });
        }
        items.push({ label: a.enabled ? "停用账号" : "启用账号", fn:function(){ accountOp("toggle", {id:a.id}); } });
        items.push({ sep:true });
        items.push({ label:"删除账号", danger:true, fn:function(){
          if (confirm("确定删除账号 " + a.name + "？")) accountOp("remove", {id:a.id});
        } });
        return items;
      }));

      var r = row([
        cell(a.name), cell(a.note || "—", a.note ? "" : "muted"), cell(a.source, "muted"),
        cell(a.access_key || "—", "mono"),
        expCell, creditCell(a), statusCell, claimCell, cell(a.failures, "mono"),
        cell(fmtTime(a.last_used), "muted nowrap"), ops
      ]);
      if (a.last_err) r.title = a.last_err;
      tb.appendChild(r);
    });
  }

  // --- API 端点 ---
  //
  // 端点固定是「当前控制台地址 + /v1」：控制台就是网关本身，不写死端口，
  // 反向代理/自定义端口部署时也能直接复制出正确地址。
  function endpointURL(){
    return location.origin + "/v1";
  }
  function renderEndpoint(){
    var el = $("#keyEndpoint");
    if (!el) return;
    el.textContent = endpointURL();
    // 回环地址从公网测不到 —— 写进按钮 title，不额外占一行。
    var local = /^(127\.0\.0\.1|localhost|\[::1\])(:\d+)?$/.test(location.host);
    $("#speedEndpoint").title = local
      ? "在线测速（tcptest.cn）：当前是本机回环地址，公网节点访问不到，需换成可路由地址"
      : "在线测速（tcptest.cn）：用公网节点访问 " + location.host;
  }

  function renderKeys(d){
    var tb = $("#keys");
    var keys = d.keys || [];
    if (!keys.length) {
      tb.textContent = "";
      tb.appendChild(emptyRow(7, "暂无密钥（未签发时 /v1 仅允许本机直连）"));
      return;
    }
    // 按 key 复用行：requests 会随流量变化，整表重建会丢 hover / 焦点。
    var byKey = {};
    for (var i = 0; i < tb.children.length; i++) {
      var kk = tb.children[i].getAttribute("data-key");
      if (kk) byKey[kk] = tb.children[i];
    }
    var out = keys.map(function(k){
      var tr = byKey[k.key];
      if (tr) { updateKeyRow(tr, k); return tr; }
      return keyRow(k);
    });
    for (var j = 0; j < out.length; j++) {
      if (tb.children[j] !== out[j]) tb.insertBefore(out[j], tb.children[j] || null);
    }
    while (tb.children.length > out.length) tb.removeChild(tb.lastChild);
  }

  function keyRow(k){
    var tr = row([
      cell(k.key, "mono"), cell(k.name), cell(""), cell(k.requests, "mono"),
      cell(fmtTime(k.created_at), "muted"), cell(fmtTime(k.last_used), "muted"),
      cell("", "actions-cell")
    ]);
    tr.setAttribute("data-key", k.key);
    tr.children[2].appendChild(pill(""));
    var ops = tr.children[6];
    // 列表里只有脱敏值，复制必须回源取明文（一次一个、带管理鉴权、留审计）。
    ops.appendChild(btn("复制", "xs", function(){
      return api("/admin/api/keys?reveal=" + encodeURIComponent(k.key)).then(function(r){
        return copyText(r.key, "已复制密钥");
      });
    }));
    ops.appendChild(btn("", "xs", function(){ keyOp("toggle", k.key); }));
    ops.appendChild(btn("删除", "xs danger", function(){
      if (confirm("确定删除该密钥？")) keyOp("key", k.key);
    }));
    updateKeyRow(tr, k);
    return tr;
  }

  function updateKeyRow(tr, k){
    setText(tr.children[1], k.name);
    var pillEl = tr.children[2].children[0];
    setText(pillEl, k.enabled ? "启用" : "停用");
    setClass(pillEl, "badge " + (k.enabled ? "ok" : ""));
    setText(tr.children[3], k.requests);
    setText(tr.children[4], fmtTime(k.created_at));
    setText(tr.children[5], fmtTime(k.last_used));
    setText(tr.children[6].children[1], k.enabled ? "停用" : "启用");
  }

  // 日志表每 5 秒会拿到新数据。以前是整表重建：滚动位置跳回顶部、hover 丢失，
  // 看上去就是「页面自己闪一下」。现在按行签名复用 DOM，通常只往头部插几行。
  var logRowSigs = [];

  function logSig(l){
    return [l.time, l.model, l.stream ? 1 : 0, l.status, l.err_class || "",
      l.latency_ms, l.attempts, l.prompt_len, l.output_len,
      l.account, l.key, l.err || ""].join("\u0001");
  }

  function renderLogs(logs){
    var tb = $("#logs");
    if (!logs || !logs.length) {
      var filtered = $("#logStatus").value || $("#logClass").value || $("#logQ").value.trim();
      tb.textContent = "";
      logRowSigs = [];
      tb.appendChild(emptyRow(11, filtered ? "没有匹配的日志（试试清空筛选条件）" : "暂无日志"));
      return;
    }
    // 先把行放到池子里：签名相同的旧行直接复用，剩下的现建。
    var pool = {};
    var existing = tb.children;
    for (var i = 0; i < existing.length; i++) {
      var s = logRowSigs[i];
      if (!s) continue;
      (pool[s] || (pool[s] = [])).push(existing[i]);
    }
    var out = [];
    for (var j = 0; j < logs.length; j++) {
      var sig = logSig(logs[j]);
      var q = pool[sig];
      out.push(q && q.length ? q.shift() : logRow(logs[j]));
    }
    for (var k = 0; k < out.length; k++) {
      if (tb.children[k] !== out[k]) tb.insertBefore(out[k], tb.children[k] || null);
    }
    while (tb.children.length > out.length) tb.removeChild(tb.lastChild);
    logRowSigs = logs.map(logSig);
  }

  function logRow(l){
    // err_class → [中文标签, 颜色类]。审核/参数是请求侧错误（不污染账号）用 warn；
    // 登录态/无账号/上游/内部属于需要介入的故障用 err；客户端中断是正常行为用 muted。
    var clsMap = {
      moderation:["审核","warn"], client_request:["参数","warn"],
      credentials:["登录态","err"], no_account:["无账号","err"],
      transient:["临时","warn"], rate_limited:["限流","warn"],
      network:["网络","warn"],
      upstream:["上游","err"], websearch:["联网搜索","warn"], client_abort:["中断","muted"], internal:["内部","err"]
    };
    var st = cell("");
    st.appendChild(pill(l.status, l.status >= 500 ? "err" : (l.status >= 400 ? "warn" : "ok")));
    var cls = cell("");
    if (l.err_class) {
      var m = clsMap[l.err_class] || [l.err_class, "warn"];
      cls.appendChild(pill(m[0], m[1]));
    }
    // attempts > 1 说明发生过换号重试，并入耗时列显示，避免再加一列。
    var lat = l.latency_ms + "ms";
    if (l.attempts > 1) lat += " · " + l.attempts + "次";
    var errCell = cell(l.err || "", "muted trunc");
    if (l.err) errCell.title = l.err;
    // 积分：upstream = 上游账单（准确）；estimate = 本地估算（带 ≈）；
    // 空来源且为 0 表示这次请求没计费（失败 / 未走到上游）。
    var creditTxt = "—", creditTitle = "";
    if (l.credits > 0) {
      creditTxt = (l.credit_source === "estimate" ? "≈" : "") + fmtCredits(l.credits);
      creditTitle = l.credit_source === "estimate"
        ? "本地估算（上游账单查询失败）"
        : "上游账单口径";
      if (l.in_tokens || l.out_tokens) {
        creditTitle += " | 输入 " + fmtNum(l.in_tokens) + " token（缓存 " + fmtNum(l.cached_tokens || 0) + "）";
        creditTitle += " | 输出 " + fmtNum(l.out_tokens) + " token";
      }
    } else if (l.credit_source === "upstream") {
      creditTxt = "0"; creditTitle = "上游账单口径（不足 0.01 积分）";
    }
    if (l.upstream_msg_id) {
      creditTitle += (creditTitle ? " | " : "") + "上游消息 ID: " + l.upstream_msg_id;
    }
    var creditCellEl = cell(creditTxt, "mono");
    if (creditTitle) creditCellEl.title = creditTitle;
    return row([
      cell(fmtTime(l.time), "muted"), cell(l.model, "mono"), creditCellEl, cell(l.stream ? "是" : "否", "mono"),
      st, cls, cell(lat, "mono"), cell(l.prompt_len + " / " + l.output_len, "mono"),
      cell(l.account, "mono"), cell(l.key, "mono"), errCell
    ]);
  }

  function renderSettings(s){
    // 用户正在编辑设置时不要用服务端值覆盖 —— 否则在别的页签点一下
    // 「探活」触发的 refresh() 会静默丢掉未保存的改动。
    setAttr($("#sPw"), "placeholder", s.admin_password_set ? "已设置，留空则不修改" : "未设置");
    if (serverHasPw !== !!s.admin_password_set) {
      serverHasPw = !!s.admin_password_set;
      updatePwToggle();
    }
    // 调试采样是即时生效的开关（没有「未保存」概念），所以不受 settingsDirty 保护：
    // 在日志页改了开关，回到设置页必须看到一致的状态。
    setChecked($("#sDebugCapture"), !!s.debug_capture);
    setValue($("#sDebugRate"), String(s.debug_capture_sample || 1));
    if (settingsDirty) return;
    setValue($("#sDefaultModel"), s.default_model || "");
    setValue($("#sForceModel"), s.force_model || "");
    setValue($("#sTimeout"), s.request_timeout_sec || "");
    setValue($("#sRetries"), s.max_retries || "");
    setValue($("#sLogKeep"), s.log_keep || "");
    setValue($("#sLiveTTL"), s.live_ttl_sec || "");
    setValue($("#sCors"), s.cors_origin || "");
    setValue($("#sUserData"), s.user_data_dir || "");
    setValue($("#sLoginPort"), s.login_port || 0);
    setChecked($("#sAllowCredit"), !!s.allow_credit_models);
    setChecked($("#sModelFallback"), !!s.model_fallback);
    setChecked($("#sCreditPriority"), (s.credit_priority === undefined ? true : !!s.credit_priority));
    setChecked($("#sAllowAnonymous"), !!s.allow_anonymous);
    setChecked($("#sRateLimit"), !!s.rate_limit_enabled);
    setValue($("#sRateQPS"), s.rate_limit_qps || "");
    setValue($("#sRateBurst"), s.rate_limit_burst || "");
    setValue($("#sRateConc"), (s.rate_limit_concurrency === undefined ? "" : s.rate_limit_concurrency));
    setChecked($("#sAutoClaim"), !!s.auto_claim);
    setValue($("#sClaimHour"), (s.claim_after_hour === undefined ? 0 : s.claim_after_hour));
    setValue($("#sClaimMinute"), (s.claim_after_minute === undefined ? 5 : s.claim_after_minute));
    setChecked($("#sAutoRefresh"), !!s.auto_refresh);
    setChecked($("#sWriteBack"), !!s.write_back_to_ide);
    setValue($("#sPluginName"), s.plugin_name || "");
    setValue($("#sPluginVersion"), s.plugin_version || "");
    setValue($("#sIDEName"), s.ide_name || "");
    setValue($("#sIDEVersion"), s.ide_version || "");
    setValue($("#sAliases"), JSON.stringify(s.model_aliases || {}, null, 2));
    setChecked($("#sSearchEnabled"), !!s.websearch_enabled);
    setValue($("#sSearchProvider"), s.websearch_provider || "searxng");
    // Key 一旦存过，输入框就变成「留空 = 不改」；提示文案也跟着变。
    searchKeySet = !!s.websearch_api_key_set;
    // 服务端存的是「当前 provider 的端点」：没在改设置时用它刷新当前槽，
    // 其它 provider 的端点沿用本地记忆（切回去不会丢）。
    if (!settingsDirty) {
      searchEndpoints[$("#sSearchProvider").value] = s.websearch_endpoint || "";
      persistSearchEndpoints();
    }
    syncSearchProviderHint();
    setValue($("#sSearchKey"), "");
    setValue($("#sSearchResults"), s.websearch_max_results || 5);
    setValue($("#sSearchTimeout"), s.websearch_timeout_sec || 8);
    setValue($("#sSearchCache"), (s.websearch_cache_ttl_sec === undefined ? 300 : s.websearch_cache_ttl_sec));
    setValue($("#sSearchPageChars"), (s.websearch_max_page_chars === undefined ? 1500 : s.websearch_max_page_chars));
    setChecked($("#sSearchFetch"), !!s.websearch_fetch_pages);
  }

  function markSettingsDirty(ev){
    // 顺手记下是哪个子页签里的东西变了：子页签会把大部分设置藏起来，
    // 不标一下就是「改了 A 分类、切到 B 分类、回来才发现没保存」。
    if (ev && ev.target && ev.target.closest) {
      var pane = ev.target.closest(".settings-pane");
      if (pane) markPaneDirty(pane.id.replace("pane-", ""));
    }
    if (settingsDirty) return;
    settingsDirty = true;
    $("#settingsDirty").classList.remove("hidden");
  }
  function markPaneDirty(name){
    var b = document.querySelector('.subtab[data-pane="' + name + '"]');
    if (!b || b.classList.contains("dirty")) return;
    b.classList.add("dirty");
    b.title = "这个分类里有未保存的改动";
  }
  function clearSettingsDirty(){
    settingsDirty = false;
    $("#settingsDirty").classList.add("hidden");
    document.querySelectorAll(".subtab.dirty").forEach(function(b){
      b.classList.remove("dirty");
      b.removeAttribute("title");
    });
  }

  function refresh(opts){
    opts = opts || {};
    return api("/admin/api/state").then(function(d){
      state.data = d;
      // 指纹守卫：数据没变就不重建 DOM。
      //
      // 自动刷新每 5 秒跑一次，而绝大多数轮询拿到的数据和上一次完全一样；
      // 整块重建会打断选择、丢滚动位置、让表格闪烁。
      // KPI 与模型表分开守卫：请求计数一变就重建整张模型表是没必要的。
      if (changed("kpis", [d.stats, accountsSig(d.accounts), (d.keys || []).length,
          (d.models || []).length, !!((d.settings || {}).allow_credit_models)])) {
        renderKpiCards(d);
      }
      if (changed("models", modelsSig(d.models))) renderModels(d);
      if (changed("accounts", accountsSig(d.accounts))) renderAccounts(d);
      if (changed("keys", keysSig(d.keys))) renderKeys(d);
      if (changed("settings", d.settings || {})) renderSettings(d.settings || {});
      if (changed("build", d.build || {})) renderBuild(d.build || {});
      renderUptime(d.uptime_seconds);
      if (!opts.silent) loadHealth();
      var now = new Date();
      var p2 = function(n){ return (n < 10 ? "0" : "") + n; };
      $("#lastSync").textContent = "已更新 " + p2(now.getHours()) + ":" + p2(now.getMinutes()) + ":" + p2(now.getSeconds());
      // 账号页可见时补一次额度（首次进页签时 refresh 可能还没回来）
      if ($("#tab-accounts").classList.contains("on")) { loadCredits(false); loadUsageForAccounts(); }
      // 接入页的密钥/模型可能刚变
      if ($("#tab-guide").classList.contains("on")) renderGuide();
    }).catch(function(e){
      // 自动刷新失败不弹 toast：网络抖一下就会刷屏。
      if (opts.silent) { $("#lastSync").textContent = "自动刷新失败"; return; }
      if (e.status === 401) {
        toast("需要管理密码：请在右上角输入后点「记住」\n" + e.message, "err");
        $("#pw").focus();
      } else if (e.status === 403) {
        toast("管理接口拒绝访问：未设置密码时仅允许本机直连\n" + e.message, "err");
      } else {
        toast("加载失败：" + e.message, "err");
      }
    });
  }

  // --- 渲染指纹 ---

  var sigCache = {};
  // changed 报告「这个 key 对应的数据是否变了」，变了才值得重绘。
  function changed(key, value){
    var s;
    try { s = JSON.stringify(value); } catch(e){ s = String(value); }
    if (sigCache[key] === s) return false;
    sigCache[key] = s;
    return true;
  }
  // 投影：只取「变了就该重绘」的字段。
  // expires_in 这类派生值每秒都在变，绝不能进指纹 —— 否则守卫形同虚设。
  function accountsSig(list){
    return (list || []).map(function(a){
      return [a.id, a.enabled, a.status, a.claimed_today, a.credit_expiring, a.has_refresh, a.note].join("|");
    }).join(",");
  }
  function keysSig(list){
    return (list || []).map(function(k){ return [k.key, k.name, k.enabled, k.requests].join("|"); }).join(",");
  }
  function modelsSig(list){
    return (list || []).map(function(m){ return [m.id, m.credit, m.available].join("|"); }).join(",");
  }

  // renderBuild 在侧栏底部显示当前构建：升级后一眼确认跑的是哪个版本。
  function renderBuild(b){
    var el = $("#build");
    if (!el) return;
    // 版本号本身就是「日期-哈希」，再拼一次 commit 会重复。
    var label = b.version || "dev";
    if (b.commit && label.indexOf(b.commit) === -1) label += " · " + b.commit;
    el.textContent = label;
    el.title = "构建 " + (b.version || "dev") +
      (b.commit ? "\n提交 " + b.commit : "") +
      (b.build_time ? "\n时间 " + b.build_time : "") +
      (b.go ? "\n" + b.go : "");
  }
  // logQuery 把筛选控件拼成查询串。过滤在服务端做：控制台只看
  // 「最近的失败」时不该把 500 条全量传过来。
  function logQuery(){
    var p = new URLSearchParams();
    p.set("limit", $("#logLimit").value);
    if ($("#logStatus").value) p.set("status", $("#logStatus").value);
    if ($("#logClass").value) p.set("class", $("#logClass").value);
    var q = $("#logQ").value.trim();
    if (q) p.set("q", q);
    var minCredits = parseFloat($("#logMinCredits").value);
    if (minCredits > 0) p.set("min_credits", String(minCredits));
    return p.toString();
  }
  function refreshLogs(silent){
    return api("/admin/api/logs?" + logQuery()).then(function(d){
      if (!changed("logs", d.logs)) return;
      renderLogs(d.logs);
    }).catch(function(e){ if (!silent) toast("日志加载失败：" + e.message, "err"); });
  }

  // --- 调试采样 ---

  // renderSamples 渲染采样列表。服务端已按「最新在前」返回，这里不再排序。
  // expandedSamples 记住用户展开过的采样（rid → 已取回的请求体）。
  //
  // 自动刷新会重建表格，不记住的话每 5 秒就把展开的详情收起来。
  var expandedSamples = {};

  function renderSamples(d){
    setChecked($("#dbgCapture"), !!d.enabled);
    setValue($("#dbgRate"), String(d.sample_rate || 1));
    // 设置页是同一个开关，两处必须一致。
    setChecked($("#sDebugCapture"), !!d.enabled);
    setValue($("#sDebugRate"), String(d.sample_rate || 1));
    setText($("#dbgDir"), d.dir
      ? "采样目录：" + d.dir + "（保留 " + (d.keep_days || 3) + " 天，超期自动删除）"
      : "采样目录不可用：无法创建 samples 目录，请检查状态文件所在磁盘权限");

    var rows = d.samples || [];
    if (!changed("samples", {enabled: !!d.enabled, rate: d.sample_rate, rows: rows})) return;

    var tb = $("#samples");
    tb.textContent = "";
    if (!rows.length) {
      tb.appendChild(emptyRow(6, d.enabled ? "暂无采样（还没有请求命中）" : "采样已关闭"));
      return;
    }
    rows.forEach(function(s){
      var tr = row([
        cell(fmtTime(s.time), "muted"),
        cell(s.path, "mono"),
        cell(s.model || "—", "mono"),
        cell(s.stream ? "是" : "否", "mono"),
        cell(s.request_id, "mono")
      ]);
      tr.setAttribute("data-rid", s.request_id);
      var ops = cell("", "actions-cell");
      ops.appendChild(btn("查看", "xs", function(){ return toggleSample(tr, s.request_id); }));
      ops.appendChild(btn("重放", "xs", function(){ return replaySample(s.request_id); }));
      tr.appendChild(ops);
      tb.appendChild(tr);
    });

    // 重建后把之前展开的详情补回去（只补还在列表里的）。
    Object.keys(expandedSamples).forEach(function(rid){
      var tr = tb.querySelector('tr[data-rid="' + rid + '"]');
      if (tr) insertSampleDetail(tr, rid, expandedSamples[rid]);
    });
  }

  function insertSampleDetail(tr, rid, body){
    var detail = document.createElement("tr");
    detail.setAttribute("data-detail", rid);
    var td = document.createElement("td");
    td.colSpan = 6;
    var pre = mk("pre", "sample-body mono");
    pre.textContent = JSON.stringify(body, null, 2);
    td.appendChild(pre);
    detail.appendChild(td);
    tr.insertAdjacentElement("afterend", detail);
  }

  // toggleSample 在该行下方展开/收起脱敏后的请求体。
  function toggleSample(tr, rid){
    var next = tr.nextElementSibling;
    if (next && next.getAttribute("data-detail") === rid) {
      next.remove();
      delete expandedSamples[rid];
      return Promise.resolve();
    }
    if (expandedSamples[rid]) {
      insertSampleDetail(tr, rid, expandedSamples[rid]);
      return Promise.resolve();
    }
    return api("/admin/api/samples/" + encodeURIComponent(rid)).then(function(d){
      expandedSamples[rid] = d.body;
      insertSampleDetail(tr, rid, d.body);
    }).catch(function(e){ toast("取采样失败：" + e.message, "err"); });
  }

  // replaySample 把那次请求原样再发一遍（会真实消耗积分）。
  function replaySample(rid){
    if (!confirm("重放会用原请求体真实再调用一次上游（消耗积分），继续？")) return Promise.resolve();
    return api("/admin/api/replay/" + encodeURIComponent(rid), {method:"POST"}).then(function(r){
      toast("重放完成：上游状态 " + r.status, r.status < 400 ? "ok" : "warn");
    }).catch(function(e){ toast("重放失败：" + e.message, "err"); });
  }

  function refreshSamples(){
    return api("/admin/api/samples?limit=50").then(renderSamples)
      .catch(function(e){ toast("采样加载失败：" + e.message, "err"); });
  }

  // --- 用量页 ---

  // lastUsageData 缓存最近一次用量响应：切「请求 / 积分 / token」指标只重画，
  // 不重新请求（changed() 守卫会让「数据没变」的刷新跳过渲染）。
  var lastUsageData = null;

  function refreshUsage(){
    var days = parseInt($("#usageDays").value, 10) || 1;
    var hours = selectedHours();
    return api("/admin/api/usage?days=" + days + "&hours=" + hours).then(function(d){
      lastUsageData = d;
      if (!changed("usage", d)) return;
      renderUsage(d);
    }).catch(function(){ /* 自动刷新时静默 */ });
  }

  // ---- 用量页 ----
  //
  // 数据全部来自 /admin/api/usage：服务端已经算好 total / by_hour / by_day /
  // by_model / by_account / actual，前端只拼表格和画柱子 —— 口径只有服务端
  // 一处，避免 JS 里再 reduce 出第二套数。

  // pctOf 是「占比」；分母为 0 时给破折号，别显示 0%（看起来像全没命中）。
  function pctOf(part, total) {
    return total > 0 ? Math.round(part / total * 100) + "%" : "—";
  }

  // avgMs 把「耗时之和 / 请求数」变成人读的平均延迟。
  function avgMs(sum, n) {
    // 老数据（升级前记录的日志）没有 latency_ms，别把它显示成 0 ms。
    if (!n || !sum) return "—";
    var v = sum / n;
    return v >= 1000 ? (v / 1000).toFixed(1) + " s" : Math.round(v) + " ms";
  }

  // tpsOf 是平均吐字速度（输出 token / 秒）。
  //
  // 分母用整请求耗时：它包含首字等待，所以这是「平均」而不是「生成阶段峰值」。
  // 旧数据没有 latency_ms / out_tokens 时给破折号。
  function tpsOf(outTokens, latencyMS) {
    if (!outTokens || !latencyMS) return "—";
    var v = outTokens / (latencyMS / 1000);
    return v >= 100 ? v.toFixed(0) : v.toFixed(1);
  }

  // niceCeil 把任意正数向上取到 1/2/5 × 10^n，给 Y 轴刻度用。
  function niceCeil(v) {
    if (!(v > 0)) return 1;
    var exp = Math.floor(Math.log10(v));
    var base = Math.pow(10, exp);
    var f = v / base;
    var nf = f <= 1 ? 1 : f <= 2 ? 2 : f <= 5 ? 5 : 10;
    return nf * base;
  }

  // pad2 是两位补零（小时 / 月日都要用）。
  function pad2(n) { return (n < 10 ? "0" : "") + n; }

  function renderUsageKpis(d) {
    var t = d.total || {};
    var tps = tpsOf(t.timed_out_tokens || 0, t.out_latency_ms || 0);
    renderKpisInto("#usageKpis", [
      { k: "请求", v: fmtNum(t.requests || 0),
        sub: t.failures ? "失败 " + fmtNum(t.failures) : "" },
      { k: "输入 token", v: fmtNum(t.in_tokens || 0) },
      { k: "输出 token", v: fmtNum(t.out_tokens || 0) },
      { k: "缓存命中", v: pctOf(t.cached_tokens || 0, t.in_tokens || 0),
        title: "命中缓存的输入 token 占比；越高越省钱" },
      { k: "积分", v: fmtCredits(t.credits || 0) },
      { k: "平均延迟", v: avgMs(t.latency_ms || 0, t.timed_requests || 0),
        title: "只统计有耗时记录的请求（含失败）；记账查询发生在响应之后，不计入延迟" },
      { k: "吐字速度", v: tps === "—" ? "—" : tps + " tok/s",
        title: "输出 token ÷ 有输出的请求耗时（含首字等待），是平均吞吐" }
    ]);
  }

  // 逐小时时序。后端只返回有数据的桶，这里补齐空小时，
  // 否则柱子会把「昨晚 3 点没人用」挤掉，看着像一直在跑。
  // selectedHours 把「今天」换算成小时数：从 00:00 到当前小时（含）。
  function selectedHours() {
    var v = $("#hourHours").value;
    if (v === "today") return Math.min(72, new Date().getHours() + 1);
    var n = parseInt(v, 10);
    return n > 0 && n <= 72 ? n : 24;
  }

  // appendTicks 给一根纵轴画 n+1 个刻度（0..n*step），bottom 对齐柱区基线。
  function appendTicks(el, step, n, fmt, H) {
    for (var k = 0; k <= n; k++) {
      var lbl = mk("span", "", fmt(step * k));
      lbl.style.bottom = Math.round(H * k / n) + "px";
      el.appendChild(lbl);
    }
  }

  // 每小时时序：一张图里三条序列、三种图形、三根轴。
  //
  //   柱（蓝）   = 请求数   → 左轴（整数刻度）
  //   实线（绿） = token    → 右轴（整数刻度）
  //   虚线（黄） = 积分     → 左内轴（0.01 起，两位小数）
  //
  // 三个单位量纲差太远（请求个位数、token 上千、积分零点几），共用一根轴
  // 必然有两条贴地；所以各自一根轴，颜色和图例一一对应。
  // 折线用 SVG 铺在柱子上方：viewBox 固定 100×88 + preserveAspectRatio=none，
  // 横坐标随容器宽度缩放，线宽用 non-scaling-stroke 保持像素不拉伸。
  function renderHourChart(d) {
    var hours = selectedHours();
    var byHour = {};
    (d.by_hour || []).forEach(function (r) { byHour[r.hour] = r; });

    var now = new Date();
    now.setMinutes(0, 0, 0);
    var list = [];
    for (var i = hours - 1; i >= 0; i--) {
      var dt = new Date(now.getTime() - i * 3600 * 1000);
      var key = dt.getFullYear() + "-" + pad2(dt.getMonth() + 1) + "-" + pad2(dt.getDate()) +
        "T" + pad2(dt.getHours());
      var r = byHour[key] || {};
      list.push({
        key: key, date: (dt.getMonth() + 1) + "/" + dt.getDate(),
        hh: pad2(dt.getHours()) + ":00",
        requests: r.requests || 0,
        credits: r.credits || 0,
        tokens: (r.in_tokens || 0) + (r.out_tokens || 0)
      });
    }

    var maxReq = 0, maxTok = 0, maxCred = 0;
    list.forEach(function (x) {
      if (x.requests > maxReq) maxReq = x.requests;
      if (x.tokens > maxTok) maxTok = x.tokens;
      if (x.credits > maxCred) maxCred = x.credits;
    });

    var H = 88; // 柱区像素高度（与 CSS 的 .usage-grid / .usage-lines 一致）
    var reqStep = Math.max(1, Math.ceil(niceCeil(maxReq / 4)));
    var tokStep = Math.max(1, Math.ceil(niceCeil(maxTok / 4)));
    var credStep = Math.max(0.01, niceCeil(maxCred / 4));
    var reqTop = reqStep * 4, tokTop = tokStep * 4, credTop = credStep * 4;
    var step = Math.max(1, Math.ceil(hours / 12));

    var plot = $("#hourChart"), axis = $("#hourAxis");
    var yReq = $("#hourYAxis"), yTok = $("#hourYTokens"), yCred = $("#hourYCredit");
    var mod = hours > 48 ? " ultra" : hours > 24 ? " dense" : "";
    plot.className = "usage-plot" + mod;
    axis.className = "usage-axis" + mod;
    plot.textContent = "";
    axis.textContent = "";
    yReq.textContent = "";
    yTok.textContent = "";
    yCred.textContent = "";

    // 网格线按「请求数」轴的刻度画（左轴是主刻度）。
    var grid = mk("div", "usage-grid");
    for (var t = 1; t < 4; t++) {
      var line = mk("i");
      line.style.bottom = Math.round(H * t / 4) + "px";
      grid.appendChild(line);
    }
    plot.appendChild(grid);

    appendTicks(yReq, reqStep, 4, function (v) { return fmtNum(Math.round(v)); }, H);
    appendTicks(yTok, tokStep, 4, function (v) { return fmtNum(Math.round(v)); }, H);
    appendTicks(yCred, credStep, 4, function (v) { return v.toFixed(2); }, H);

    var n = list.length;
    var NS = "http://www.w3.org/2000/svg";
    var svg = document.createElementNS(NS, "svg");
    svg.setAttribute("class", "usage-lines");
    svg.setAttribute("viewBox", "0 0 100 88");
    svg.setAttribute("preserveAspectRatio", "none");
    var pts = function (get, top) {
      var out = [];
      for (var i = 0; i < n; i++) {
        var v = get(list[i]);
        var y = top > 0 ? H - Math.min(1, v / top) * H : H;
        out.push(((i + 0.5) * 100 / n).toFixed(3) + "," + y.toFixed(2));
      }
      return out.join(" ");
    };
    var tokPts = pts(function (x) { return x.tokens; }, tokTop);
    var area = document.createElementNS(NS, "polygon");
    area.setAttribute("class", "tok-area");
    area.setAttribute("points", "0,88 " + tokPts + " 100,88");
    svg.appendChild(area);
    var tokLine = document.createElementNS(NS, "polyline");
    tokLine.setAttribute("class", "tok");
    tokLine.setAttribute("points", tokPts);
    svg.appendChild(tokLine);
    var credLine = document.createElementNS(NS, "polyline");
    credLine.setAttribute("class", "cred");
    credLine.setAttribute("points", pts(function (x) { return x.credits; }, credTop));
    svg.appendChild(credLine);
    plot.appendChild(svg);

    list.forEach(function (x, idx) {
      var v = x.requests;
      var col = mk("div", "usage-bar" + (v > 0 ? "" : " zero"));
      col.title = x.date + " " + x.hh + "：" + fmtNum(x.requests) + " 次请求 · " +
        fmtCredits(x.credits) + " 积分 · " + fmtNum(x.tokens) + " token";
      // 24 小时内柱子够宽，标上请求数；更密时只看悬停提示。
      if (hours <= 24) col.appendChild(mk("div", "val", v > 0 ? fmtNum(v) : ""));
      var bar = mk("div", "bar");
      bar.style.height = Math.max(2, Math.round(Math.min(1, v / reqTop) * H)) + "px";
      col.appendChild(bar);
      plot.appendChild(col);
      axis.appendChild(mk("span", "", idx % step === 0 ? x.hh : ""));
    });
  }

  function renderUsageModels(d) {
    var models = d.by_model || [];
    var tb = $("#usageModels");
    tb.textContent = "";
    if (!models.length) {
      tb.appendChild(emptyRow(8, "暂无数据（发一次请求后就有了）"));
      return;
    }
    models.forEach(function (m) {
      tb.appendChild(row([
        cell(m.model, "mono"), cell(fmtNum(m.requests), "mono"),
        cell(fmtNum(m.in_tokens), "mono"), cell(fmtNum(m.out_tokens), "mono"),
        cell(pctOf(m.cached_tokens, m.in_tokens), "mono"),
        cell(fmtCredits(m.credits), "mono"),
        cell(avgMs(m.latency_ms, m.timed_requests), "mono"),
        cell(tpsOf(m.timed_out_tokens, m.out_latency_ms), "mono")
      ]));
    });
  }

  function renderUsageAccounts(d) {
    var accounts = d.by_account || [];
    var tb = $("#usageAccounts");
    tb.textContent = "";
    if (!accounts.length) {
      tb.appendChild(emptyRow(8, "暂无数据"));
      return;
    }
    accounts.forEach(function (a) {
      var name = a.account || (a.account_id ? a.account_id : "（历史，无账号）");
      var nameCell = cell(name, "mono");
      if (a.account_id) nameCell.title = a.account_id;
      tb.appendChild(row([
        nameCell, cell(fmtNum(a.requests), "mono"),
        cell(fmtNum(a.in_tokens), "mono"), cell(fmtNum(a.out_tokens), "mono"),
        cell(pctOf(a.cached_tokens, a.in_tokens), "mono"),
        cell(fmtCredits(a.credits), "mono"),
        cell(avgMs(a.latency_ms, a.timed_requests), "mono"),
        cell(tpsOf(a.timed_out_tokens, a.out_latency_ms), "mono")
      ]));
    });
  }

  function renderUsageActual(d) {
    var actual = d.actual || [];
    var tb = $("#usageActual");
    tb.textContent = "";
    if (!actual.length) {
      tb.appendChild(emptyRow(3, "暂无快照差值（账号积分快照每 10 分钟刷新一次）"));
      return;
    }
    actual.slice().sort(function (a, b) { return a.day < b.day ? 1 : -1; }).forEach(function (r) {
      tb.appendChild(row([
        cell(r.day, "mono"), cell(r.account || r.account_id, "mono"),
        cell(fmtCredits(r.credits), "mono")
      ]));
    });
  }

  function renderUsageDaily(d) {
    var actual = d.actual || [];
    var actualByDay = {};
    actual.forEach(function (r) { actualByDay[r.day] = (actualByDay[r.day] || 0) + (r.credits || 0); });

    var dayMap = {};
    (d.by_day || []).forEach(function (r) {
      dayMap[r.day] = {
        day: r.day, requests: r.requests || 0, credits: r.credits || 0,
        in: r.in_tokens || 0, out: r.out_tokens || 0, cached: r.cached_tokens || 0,
        latency: r.latency_ms || 0, timed: r.timed_requests || 0,
        outLatency: r.out_latency_ms || 0, timedOut: r.timed_out_tokens || 0, actual: undefined
      };
    });
    Object.keys(actualByDay).forEach(function (day) {
      if (!dayMap[day]) {
        dayMap[day] = { day: day, requests: 0, credits: 0, in: 0, out: 0, cached: 0,
          latency: 0, timed: 0, outLatency: 0, timedOut: 0 };
      }
      dayMap[day].actual = actualByDay[day];
    });
    var list = Object.keys(dayMap).map(function (k) { return dayMap[k]; })
      .sort(function (a, b) { return a.day < b.day ? 1 : -1; });

    var tb = $("#usageDaily");
    tb.textContent = "";
    if (!list.length) {
      tb.appendChild(emptyRow(10, "暂无数据"));
      return;
    }
    list.forEach(function (x) {
      var diff = x.actual === undefined ? null : x.actual - x.credits;
      var diffTxt = diff === null ? "—" : (diff > 0 ? "+" : "") + diff.toFixed(2);
      var diffCell = cell(diffTxt, "mono");
      if (diff !== null && Math.abs(diff) > 0.05) {
        diffCell.title = "上游实测与网关账单的差额（账号在 IDE / 其它客户端的消耗，或快照刷新滞后）";
      }
      tb.appendChild(row([
        cell(x.day, "mono"), cell(fmtNum(x.requests), "mono"),
        cell(fmtNum(x.in), "mono"), cell(fmtNum(x.out), "mono"),
        cell(pctOf(x.cached, x.in), "mono"),
        cell(fmtCredits(x.credits), "mono"),
        cell(avgMs(x.latency, x.timed), "mono"),
        cell(tpsOf(x.timedOut, x.outLatency), "mono"),
        cell(x.actual === undefined ? "—" : fmtCredits(x.actual), "mono"),
        diffCell
      ]));
    });
  }

  function renderUsage(d) {
    renderUsageKpis(d);
    renderHourChart(d);
    renderUsageModels(d);
    renderUsageAccounts(d);
    renderUsageActual(d);
    renderUsageDaily(d);

    var t = d.total || {};
    var summary = "网关账单 " + fmtCredits(t.credits || 0) + " 积分";
    if (typeof d.actual_total === "number") summary += " · 上游实测 " + fmtCredits(d.actual_total) + " 积分";
    setText($("#usageCreditSummary"), summary);
  }

  // ---- 余额页 ----
  //
  // 数据来自 /admin/api/balance-trend：服务端已经算好窗口净变化、今日 / 昨日、
  // 消耗 / 到账与每日分桶，前端只负责画图和拼表格。
  //
  // 图是纯 SVG：viewBox 固定 1000×300 + preserveAspectRatio=none，
  // 横坐标随容器缩放，线宽靠 non-scaling-stroke 保持像素宽（与用量页同一套做法）。
  var BAL_H = 300;
  // 最近一次 /balance-trend 的响应：resize 时按新宽度重画（SVG 用像素坐标）。
  var balLastData = null;

  // fmtBal 是积分水位的显示格式：整数 + 千分位（水位是连续量，小数没有意义）。
  function fmtBal(v){
    return fmtNum(Math.round(Number(v) || 0));
  }
  // fmtDelta 给变化量：带正负号，0 附近显示破折号（避免「+0」到处刷屏）。
  function fmtDelta(v){
    var n = Number(v) || 0;
    if (Math.abs(n) < 0.5) return "—";
    return (n > 0 ? "+" : "−") + fmtNum(Math.round(Math.abs(n)));
  }
  function deltaCls(v){
    var n = Number(v) || 0;
    return n > 0 ? "up" : n < 0 ? "down" : "";
  }
  // balIdx 把账号下标映射到 c0..c7，超过 8 个账号后循环使用（图例与表首条同色）。
  function balIdx(i){ return "bal-c" + (i % 8); }

  function refreshBalance(){
    var days = parseInt($("#balDays").value, 10) || 30;
    return api("/admin/api/balance-trend?days=" + days).then(function(d){
      // 指纹只取数据字段：to 每次请求都变，不能进指纹，否则每 5 秒全量重建。
      if (!changed("balance", {days: d.days, from: d.from, updated_at: d.updated_at,
          summary: d.summary, accounts: d.accounts, daily: d.daily})) return;
      balLastData = d;
      renderBalance(d);
    }).catch(function(){ /* 自动刷新时静默 */ });
  }

  function renderBalance(d){
    var accounts = d.accounts || [];
    var s = d.summary || {};

    // --- 顶部总账 ---
    var box = $("#balSummary");
    box.textContent = "";
    [
      {k: "账号", v: fmtNum(s.accounts || 0)},
      {k: "可用", v: (s.available || 0) + " / " + (s.accounts || 0)},
      {k: "当前合计", v: fmtBal(s.current_total)},
      {k: "窗口净变化", v: fmtDelta(s.net_change), cls: deltaCls(s.net_change)},
      {k: "采样点数", v: fmtNum(s.samples || 0)}
    ].forEach(function(x){
      var c = mk("div", "cell");
      c.appendChild(mk("div", "k", x.k));
      c.appendChild(mk("div", "v" + (x.cls ? " " + x.cls : ""), x.v));
      box.appendChild(c);
    });

    setText($("#balUpdated"), d.updated_at
      ? "更新于 " + fmtShort(d.updated_at)
      : "暂无采样");

    // --- 图例 ---
    var lg = $("#balLegend");
    lg.textContent = "";
    if (!accounts.length) {
      lg.appendChild(mk("span", "muted", "账号池为空"));
    } else {
      accounts.forEach(function(a, i){
        var it = mk("span", "item");
        it.appendChild(mk("i", "sw " + balIdx(i)));
        it.appendChild(mk("span", null, a.account || a.account_id));
        lg.appendChild(it);
      });
    }

    // --- 折线 ---
    var plot = $("#balPlot"), yaxis = $("#balYAxis"), xaxis = $("#balXAxis");
    plot.textContent = "";
    yaxis.textContent = "";
    xaxis.textContent = "";

    var t0 = (d.from || 0) * 1000, t1 = (d.to || 0) * 1000;
    var lo = Infinity, hi = -Infinity;
    accounts.forEach(function(a){
      (a.points || []).forEach(function(p){
        if (p.v < lo) lo = p.v;
        if (p.v > hi) hi = p.v;
      });
    });
    var hasData = isFinite(lo) && isFinite(hi) && t1 > t0;

    if (!hasData || !(d.summary || {}).samples) {
      // 有账号快照但还没有走势（刚部署）：表格里有「当前」，图上只给提示。
      plot.appendChild(mk("div", "bal-empty",
        "还没有采样点。后台每 10 分钟刷新一次账号积分，半小时内就能看到曲线。"));
    } else {
      if (hi - lo < 1) { hi = lo + 1; }
      // 上下各留 6% 余量，避免曲线贴边。轴不从 0 起（图下已标注）：
      // 余额从几千掉到几百时，从 0 起画会把整段变化压成一条直线。
      //
      // 这里**不**把 lo 取整到 1/2/5 档：余额量级上千时，2,000 的步长会把
      // 几千的波动全塞进一格（轴变成 0 / 2,000 / 4,000 …）。保留自然刻度
      // （如 1,818 / 2,749 / …），与截图里的 927 / 2,087 / 3,246 同一种风格。
      var pad = (hi - lo) * 0.06;
      lo = Math.max(0, Math.floor((lo - pad) / 10) * 10);
      hi = Math.ceil((hi + pad) / 10) * 10;
      if (hi - lo < 4) hi = lo + 4;
      var ticks = 4;
      var step = (hi - lo) / ticks;
      var span = hi - lo || 1;

      // 网格 + 左轴刻度。
      var grid = mk("div", "grid");
      for (var k = 1; k <= ticks; k++) {
        var line = mk("i");
        line.style.bottom = (k / ticks * 100) + "%";
        grid.appendChild(line);
      }
      plot.appendChild(grid);
      for (var k = 0; k <= ticks; k++) {
        var lbl = mk("span", null, fmtNum(Math.round(lo + step * k)));
        lbl.style.bottom = (k / ticks * 100) + "%";
        yaxis.appendChild(lbl);
      }

      // 用像素坐标而不是固定的 1000 宽逻辑坐标：点太少时要补圆点，
      // viewBox 一拉伸圆就变成椭圆了。容器尺寸变了由 resize 重绘。
      var W = Math.max(160, plot.clientWidth || 1000);
      var NS = "http://www.w3.org/2000/svg";
      var svg = document.createElementNS(NS, "svg");
      svg.setAttribute("viewBox", "0 0 " + W + " " + BAL_H);
      svg.setAttribute("preserveAspectRatio", "none");
      svg.setAttribute("role", "img");
      svg.setAttribute("aria-label", "各账号积分水位趋势");
      var sx = function(ms){ return (ms - t0) / (t1 - t0) * W; };
      var sy = function(v){ return (1 - Math.min(1, Math.max(0, (v - lo) / span))) * BAL_H; };
      accounts.forEach(function(a, i){
        var pts = a.points || [];
        if (!pts.length) return;
        var dstr = "";
        pts.forEach(function(p, idx){
          // 阶梯线：余额在两次采样之间保持不变，所以先横再竖。
          if (idx === 0) {
            dstr += "M" + sx(p.at * 1000).toFixed(1) + " " + sy(p.v).toFixed(1);
          } else {
            dstr += "H" + sx(p.at * 1000).toFixed(1) + "V" + sy(p.v).toFixed(1);
          }
        });
        // 末值保持到「此刻」：两次采样之间余额不变，线停在半小时前会让人
        // 以为数据断了。但只对「刚刚还在报数」的账号这样做——停更一天的账号
        // 拉一条平线到右边缘，会被误读成「一天没掉分」。
        if (t1 - pts[pts.length - 1].at * 1000 <= 2 * 3600 * 1000) {
          dstr += "H" + sx(t1).toFixed(1);
        }
        var path = document.createElementNS(NS, "path");
        path.setAttribute("d", dstr);
        path.setAttribute("class", "line " + balIdx(i));
        svg.appendChild(path);
        // 点稀的时候补圆点：1 个点画不出线，2 个点会缩成一根竖线。
        if (pts.length <= 60) {
          pts.forEach(function(p, idx){
            var c = document.createElementNS(NS, "circle");
            c.setAttribute("cx", sx(p.at * 1000).toFixed(1));
            c.setAttribute("cy", sy(p.v).toFixed(1));
            // 末点放大一点：窗口很长、数据只有今天这一小段时，
            // 至少能在右边缘看到「当前水位」这个点。
            c.setAttribute("r", idx === pts.length - 1 ? "3.5" : "2.5");
            c.setAttribute("class", "bal-dot " + balIdx(i));
            svg.appendChild(c);
          });
        }
      });
      plot.appendChild(svg);

      // 横轴：固定 6 个日期标签，覆盖整个窗口（近 7/14/30 天）。
      var spanMs = t1 - t0;
      for (var i = 0; i < 6; i++) {
        var dt = new Date(t0 + spanMs * i / 5);
        xaxis.appendChild(mk("span", null, (dt.getMonth() + 1) + "-" + pad2(dt.getDate())));
      }
    }

    renderBalanceDaily(d, accounts);
    renderBalanceTable(accounts);
  }

  // renderBalanceDaily 画「每日变化」：一天一列，列内按账号顺序并排。
  // 零线在中间：正值向上（绿），负值向下（红），采样不完整时降低不透明度。
  function renderBalanceDaily(d, accounts){
    var daily = d.daily || [];
    var box = $("#balDaily"), axis = $("#balDailyAxis");
    box.textContent = "";
    box.classList.remove("bal-daily-empty");
    axis.textContent = "";

    var yax = $("#balDailyYAxis");
    if (!(d.summary || {}).samples) {
      box.classList.add("bal-daily-empty");
      box.appendChild(mk("div", "bal-empty", "还没有采样，无法计算日变化。"));
      $("#balDailyLegend").textContent = "";
      yax.textContent = "";
      yax.style.visibility = "hidden";
      return;
    }
    yax.style.visibility = "";
    // 窗口里的每一天都占一列：没有采样的日子空着，只有有数据的那天出柱子。
    // 这样横轴上的日期是连续的，不会因为前面几天没数据就只剩今天一列。
    var days = daily;

    // 正负分开量程：某一侧完全没有值时，零线贴到那一侧的边上。
    // 否则整张图一半是空白，柱子离横轴老远（看起来像两个不相干的东西）。
    var posMax = 0, negMax = 0;
    days.forEach(function(row){
      (row.cells || []).forEach(function(c){
        var v = c.delta || 0;
        if (v > posMax) posMax = v;
        if (-v > negMax) negMax = -v;
      });
    });

    // 零线永远居中：位置固定，柱子的长短才好跨天比较。
    // 不做「按量级浮动」，也不因为某一侧为空就贴边。
    var H = 208; // 容器 210px，留 2px 给零线
    var posH = Math.round(H / 2);
    var negH = H - posH;

    // 左侧刻度：上限 / 0 / 下限（量程为 0 时只留一个 0）。
    yax.textContent = "";
    var ticks = [[0, negH + "px"]];
    if (posMax > 0) ticks.push([posMax, "100%"]);
    if (negMax > 0) ticks.push([-negMax, "0%"]);
    ticks.forEach(function(t){
      var lbl = mk("span", null, (t[0] > 0 ? "+" : t[0] < 0 ? "−" : "") + fmtNum(Math.round(Math.abs(t[0]))));
      lbl.style.bottom = t[1];
      yax.appendChild(lbl);
    });

    var labelStep = Math.max(1, Math.ceil(days.length / 10));

    days.forEach(function(row, idx){
      var col = mk("div", "bal-day");
      var pos = mk("div", "pos");
      var neg = mk("div", "neg");
      pos.style.height = posH + "px";
      neg.style.height = negH + "px";
      (row.cells || []).forEach(function(c){
        var v = c.delta || 0;
        var bar = mk("div", "bar" + (v < 0 ? " loss" : v === 0 ? " flat" : "") + (c.complete || v === 0 ? "" : " partial"));
        bar.title = row.day + " · " + (c.account || c.account_id) + "：" + fmtDelta(v) +
          (c.complete ? "" : "（当天采样 " + (c.samples || 0) + " 个点，不完整）");
        var half = v < 0 ? negH : posH;
        var peak = v < 0 ? negMax : posMax;
        bar.style.height = (v === 0 || !peak) ? "1px"
          : Math.max(1, Math.round(Math.min(1, Math.abs(v) / peak) * (half - 2))) + "px";
        // 当天一个采样点都没有的账号不放空白条：列宽已经按天定死，
        // 空 div 会占位（其他账号的条被挤细），视觉上像是「掉了」。
        if (!(c.samples > 0)) { return; }
        (v < 0 ? neg : pos).appendChild(bar);
      });
      col.appendChild(pos);
      col.appendChild(neg);
      box.appendChild(col);

      // 首尾两列一定带日期，中间按步长显示（否则最后一根柱子对不上任何刻度）。
      var show = idx === 0 || idx === days.length - 1 || idx % labelStep === 0;
      axis.appendChild(mk("span", null, show ? row.day.slice(5) : ""));
    });

    var lg = $("#balDailyLegend");
    lg.textContent = "";
    [["sw gain", "净增"], ["sw loss", "净减"], ["sw partial", "采样不完整"]].forEach(function(x){
      var it = mk("span", "item");
      it.appendChild(mk("i", x[0]));
      it.appendChild(mk("span", null, x[1]));
      lg.appendChild(it);
    });
    if (accounts.length) {
      var order = accounts.map(function(a, i){ return (i + 1) + ". " + (a.account || a.account_id); }).join("  ");
      lg.appendChild(mk("span", "muted", "组内左→右：" + order));
    }
  }

  // renderBalanceTable 渲染「按账号」表：最后一行是合计。
  function renderBalanceTable(accounts){
    var tb = $("#balAccounts");
    tb.textContent = "";
    if (!accounts.length) {
      tb.appendChild(emptyRow(10, "暂无账号"));
      return;
    }

    // 两行格：上一行今日，下一行窗口合计（与截图里的「消耗* / 到账*」一致）。
    function twoLine(today, total){
      var td = mk("td", "cell-stack num");
      td.appendChild(mk("b", null, fmtBal(today)));
      td.appendChild(mk("span", "sub", fmtBal(total)));
      return td;
    }

    var tot = {current: 0, net: 0, today: 0, yesterday: 0, spendToday: 0, spendWindow: 0,
      gainToday: 0, gainWindow: 0, samples: 0, available: 0};

    accounts.forEach(function(a, i){
      var tr = mk("tr");
      var nameCell = mk("td");
      var wrap = mk("span", "bal-name " + balIdx(i));
      wrap.appendChild(mk("i"));
      var nm = mk("span", null, a.account || a.account_id);
      nm.title = a.account_id;
      wrap.appendChild(nm);
      nameCell.appendChild(wrap);
      tr.appendChild(nameCell);

      tr.appendChild(cell(fmtBal(a.current), "num"));
      tr.appendChild(cell(a.has_data ? fmtBal(a.min) : "—", "num"));
      tr.appendChild(cell(a.has_data ? fmtBal(a.max) : "—", "num"));
      tr.appendChild(cell(fmtDelta(a.net), "num " + deltaCls(a.net)));
      tr.appendChild(cell(fmtDelta(a.today), "num " + deltaCls(a.today)));
      tr.appendChild(cell(fmtDelta(a.yesterday), "num " + deltaCls(a.yesterday)));
      tr.appendChild(twoLine(a.spend_today, a.spend_window));
      tr.appendChild(twoLine(a.gain_today, a.gain_window));
      tr.appendChild(cell(fmtNum(a.samples), "num"));
      tb.appendChild(tr);

      tot.current += a.current || 0;
      tot.net += a.net || 0;
      tot.today += a.today || 0;
      tot.yesterday += a.yesterday || 0;
      tot.spendToday += a.spend_today || 0;
      tot.spendWindow += a.spend_window || 0;
      tot.gainToday += a.gain_today || 0;
      tot.gainWindow += a.gain_window || 0;
      tot.samples += a.samples || 0;
      if (a.has_data) tot.available++;
    });

    var tr = mk("tr", "total");
    var first = mk("td");
    first.appendChild(mk("div", null, "合计"));
    first.appendChild(mk("div", "sub muted", "可用 " + tot.available + "/" + accounts.length));
    tr.appendChild(first);
    tr.appendChild(cell(fmtBal(tot.current), "num"));
    tr.appendChild(cell("—", "num"));
    tr.appendChild(cell("—", "num"));
    tr.appendChild(cell(fmtDelta(tot.net), "num " + deltaCls(tot.net)));
    tr.appendChild(cell(fmtDelta(tot.today), "num " + deltaCls(tot.today)));
    tr.appendChild(cell(fmtDelta(tot.yesterday), "num " + deltaCls(tot.yesterday)));
    tr.appendChild(twoLine(tot.spendToday, tot.spendWindow));
    tr.appendChild(twoLine(tot.gainToday, tot.gainWindow));
    tr.appendChild(cell(fmtNum(tot.samples), "num"));
    tb.appendChild(tr);
  }

  // --- 接入指引 ---

  // renderGuide 生成当前网关的接入示例。
  // 地址取 location.origin（不写死 10087），密钥用列表里的**脱敏值**占位；
  // 点「复制」时才回源取明文（与密钥页同一套口径）。
  function renderGuide(){
    var d = state.data || {};
    var origin = location.origin;
    // 这一页是全静态示例，每隔 5 秒的自动刷新会再调一次。全部走守卫：
    // 值没变就不碰 DOM，否则五个代码块 + 输入框每 5 秒刷一遍。
    setValue($("#guideBase"), origin + "/v1");
    setValue($("#guideAnthropic"), origin);

    var allKeys = d.keys || [];
    var enabled = allKeys.filter(function(k){ return k.enabled; });
    var masked = enabled.length ? enabled[0].key : "";
    var keyEl = $("#guideKey");
    setValue(keyEl, masked || "");
    // 区分「一个都没签发」和「签发了但全停用」—— 后者提示「尚未签发」会让人
    // 以为密钥丢了，跑去重新建一个。
    var ph = masked ? ""
      : (allKeys.length ? "密钥已停用，请到「密钥」页启用" : "尚未签发密钥");
    setAttr(keyEl, "placeholder", ph);

    var model = (d.models && d.models[0] && d.models[0].id) ||
      ((d.settings || {}).default_model) || "glm-5.3";
    var key = masked || "sk-ca-...";

    setText($("#exChat"),
      "curl " + origin + "/v1/chat/completions \\\n" +
      "  -H 'Authorization: Bearer " + key + "' \\\n" +
      "  -H 'Content-Type: application/json' \\\n" +
      "  -d '{\"model\":\"" + model + "\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'\n\n" +
      "# 流式：再加 \"stream\": true（响应是 SSE，以 data: [DONE] 结束）");

    setText($("#exResponses"),
      "curl " + origin + "/v1/responses \\\n" +
      "  -H 'Authorization: Bearer " + key + "' \\\n" +
      "  -H 'Content-Type: application/json' \\\n" +
      "  -d '{\"model\":\"" + model + "\",\"input\":\"hi\"}'\n\n" +
      "# 有状态续写：上一轮响应里的 id 填进 previous_response_id（网关内存保存 1 小时）");

    setText($("#exAnthropic"),
      "curl " + origin + "/v1/messages \\\n" +
      "  -H 'x-api-key: " + key + "' \\\n" +
      "  -H 'anthropic-version: 2023-06-01' \\\n" +
      "  -H 'Content-Type: application/json' \\\n" +
      "  -d '{\"model\":\"" + model + "\",\"max_tokens\":256,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'\n\n" +
      "# token 计数：POST /v1/messages/count_tokens（本地估算，不消耗积分）");

    setText($("#exClaude"),
      "export ANTHROPIC_BASE_URL=" + origin + "\n" +
      "export ANTHROPIC_AUTH_TOKEN=" + key + "\n" +
      "# 然后正常跑 claude 即可（/v1/messages 与 count_tokens 都已支持）");

    setText($("#exPython"),
      "from openai import OpenAI\n" +
      "client = OpenAI(base_url=\"" + origin + "/v1\", api_key=\"" + key + "\")\n" +
      "r = client.chat.completions.create(model=\"" + model + "\",\n" +
      "    messages=[{\"role\": \"user\", \"content\": \"hi\"}])\n" +
      "print(r.choices[0].message.content)");
  }

  // copyGuideKey 回源取密钥明文（列表里只有脱敏值）。
  function copyGuideKey(){
    var masked = $("#guideKey").value.trim();
    if (!masked) { toast("还没有可用的密钥，请先到「密钥」页签发", "err"); return Promise.resolve(); }
    return api("/admin/api/keys?reveal=" + encodeURIComponent(masked)).then(function(r){
      return copyText(r.key, "已复制密钥");
    });
  }

  // --- 自动刷新 ---

  // 只拉数据、只更新变化的区块：不做整页刷新，也不重建没变的表格。
  // 页面不可见时跳过（省电，也避免后台标签页把日志滚动位置刷掉）。
  var autoTimer = null;
  function startAutoRefresh(){
    if (autoTimer) return;
    autoTimer = setInterval(function(){
      if (document.hidden) return;
      refresh({silent: true});
      if ($("#tab-logs").classList.contains("on")) { refreshLogs(true); refreshSamples(); }
      if ($("#tab-usage").classList.contains("on")) refreshUsage();
      if ($("#tab-balance").classList.contains("on")) refreshBalance();
    }, 5000);
    // 切回前台时立刻补一次，不然要等最多 5 秒。
    document.addEventListener("visibilitychange", function(){
      if (document.hidden) return;
      refresh({silent: true});
      if ($("#tab-logs").classList.contains("on")) { refreshLogs(true); refreshSamples(); }
      if ($("#tab-usage").classList.contains("on")) refreshUsage();
      if ($("#tab-balance").classList.contains("on")) refreshBalance();
    });
  }

  // saveDebug 只提交调试采样相关的两个字段，不动其它设置
  // （后端按字段非 nil 更新，不会把其它设置覆盖成零值）。
  function saveDebug(patch){
    return api("/admin/api/settings", {method:"POST", body: patch}).then(function(){
      toast("已更新", "ok");
      return refreshSamples();
    }).catch(function(e){ toast("失败：" + e.message, "err"); return refreshSamples(); });
  }

  function accountOp(action, extra){
    var body = { action: action };
    if (extra) for (var k in extra) body[k] = extra[k];
    return api("/admin/api/accounts", {method:"POST", body:body}).then(function(){
      toast("操作成功", "ok");
      return refresh();
    }).catch(function(e){ toast("失败：" + e.message, "err"); });
  }
  function keyOp(kind, key){
    return api("/admin/api/keys?" + kind + "=" + encodeURIComponent(key), {method:"DELETE"})
      .then(function(){ toast("操作成功","ok"); return refresh(); })
      .catch(function(e){ toast("失败：" + e.message, "err"); });
  }
  function checkin(id){
    toast("探活中，请稍候…");
    return api("/admin/api/checkin", {method:"POST", body: id ? {id:id} : {}}).then(function(r){
      var results = r.results || [];
      var bad = results.filter(function(x){ return !x.ok; });
      toast("探活完成：" + results.length + " 个账号，失败 " + bad.length +
        (bad.length ? "\n" + bad.map(function(x){ return x.name + ": " + x.err; }).join("\n") : ""),
        bad.length ? "err" : "ok");
      return refresh();
    }).catch(function(e){ toast("探活失败：" + e.message, "err"); });
  }

  function claim(id){
    toast("领取中，请稍候…");
    return api("/admin/api/claim", {method:"POST", body: id ? {id:id} : {}}).then(function(r){
      var results = r.results || [];
      var bad = results.filter(function(x){ return !x.ok; });
      toast("领取完成：" + (r.succeeded||0) + "/" + (r.total||0) + " 成功" +
        (bad.length ? "\n" + bad.map(function(x){ return x.name + ": " + x.message; }).join("\n") : ""),
        bad.length ? "err" : "ok");
      return refresh();
    }).catch(function(e){ toast("领取失败：" + e.message, "err"); });
  }

  function exportSession(id){
    toast("导出中…");
    api("/admin/api/accounts", {method:"POST", body:{action:"export", id:id}}).then(function(r){
      $("#exportJSON").value = r.session_json || "";
      $("#exportMeta").textContent = "账号 " + (r.name||"") +
        " · 凭据有效期至 " + fmtTime(r.expires_at) +
        " · 可续期 " + (r.has_refresh ? "是" : "否");
      $("#exportPanel").classList.remove("hidden");
      toast("已导出（含 refresh_token，请勿外传）", "ok");
    }).catch(function(e){ toast("导出失败：" + e.message, "err"); });
  }

  // --- 浏览器登录 ---
  var loginTimer = null, loginSid = null;
  function stopLogin(){
    if (loginTimer) { clearInterval(loginTimer); loginTimer = null; }
    loginSid = null;
  }
  function pollLogin(){
    if (!loginSid) return;
    api("/admin/api/login/poll", {method:"POST", body:{session_id:loginSid}}).then(function(r){
      var map = {
        pending:   "等待在浏览器里完成登录…",
        confirmed: "登录成功",
        failed:    "登录失败",
        expired:   "授权超时（5 分钟）",
        canceled:  "已取消"
      };
      $("#loginStatus").textContent = (map[r.status] || r.status) + (r.message ? " · " + r.message : "");
      if (r.status === "confirmed") {
        stopLogin();
        toast("浏览器登录成功：" + (r.account_name || r.account || "") +
          (r.access_key ? "\nAK " + r.access_key : ""), "ok");
        $("#loginPanel").classList.add("hidden");
        refresh();
      } else if (r.status === "failed" || r.status === "expired" || r.status === "canceled") {
        stopLogin();
      }
    }).catch(function(e){ stopLogin(); toast("轮询失败：" + e.message, "err"); });
  }

  var WELFARE_STATUS = {
    ELIGIBLE:  ["可领取", "ok"],
    CLAIMED:   ["已领取", "warn"],
    CONFIRMED: ["已到账", "ok"],
    CONSUMED:  ["已使用", ""],
    ENTRY:     ["需先完成前置", "warn"]
  };

  function renderWelfare(campaigns){
    var tb = $("#welfare");
    tb.textContent = "";
    if (!campaigns || !campaigns.length) { tb.appendChild(emptyRow(7, "暂无活动")); return; }
    campaigns.forEach(function(c){
      var st = WELFARE_STATUS[c.status] || [c.status || "—", ""];
      var stCell = cell(""); stCell.appendChild(pill(st[0], st[1]));

      var autoCell = cell("");
      autoCell.appendChild(c.auto_claimable ? pill("是", "ok") : pill("否", ""));

      var reward = c.benefit_amount ? (c.benefit_amount + " " + (c.benefit_unit || "")) : "—";
      var period = (c.start_time || "").slice(0,10) + " ~ " + (c.end_time || "").slice(0,10);

      tb.appendChild(row([
        cell(c.title), cell(c.type, "mono"), cell(reward, "mono"),
        stCell, autoCell, cell(c.trigger_event || c.trigger_mode || "—", "mono"),
        cell(period, "muted")
      ]));
    });
  }

  // 当前选中的福利账号（空 = 让后端按调度池挑一个）
  var welfareAccountID = "";

  function loadWelfare(){
    $("#welfareHint").textContent = "加载中…";
    var q = welfareAccountID ? ("?id=" + encodeURIComponent(welfareAccountID)) : "";
    return api("/admin/api/welfare" + q).then(function(r){
      renderWelfare(r.campaigns);
      renderWelfareAccountPicker(r);
      $("#welfareHint").textContent = r.claimable_count
        ? ("当前账号 " + (r.account || "") + "，可领 " + r.claimable_count + " 项")
        : ("当前账号 " + (r.account || "") + "，暂无可领");
    }).catch(function(e){
      $("#welfareHint").textContent = "";
      toast("获取活动列表失败：" + e.message, "err");
    });
  }

  // renderWelfareAccountPicker 把账号下拉框填满，并同步当前选择。
  //
  // 之前只显示一个账号名，很容易被误解成「只有这个号能领」——实际上
  // 活动清单是按账号查的，每个号的可领项都不同。
  function renderWelfareAccountPicker(r){
    var sel = $("#welfareAccount");
    if (!sel) return;
    var list = r.accounts || [];
    sel.textContent = "";
    list.forEach(function(a){
      var o = document.createElement("option");
      o.value = a.id;
      o.textContent = a.name + (a.permanent ? "（永久 AK/SK）" : "");
      if (a.id === r.id) o.selected = true;
      sel.appendChild(o);
    });
    if (r.id) welfareAccountID = r.id;
    sel.disabled = list.length <= 1;
    // 账号只有一个时说明白，别让用户以为漏了
    sel.title = list.length <= 1
      ? "当前只有 1 个账号"
      : "活动清单按账号查询，每个账号的可领项不同";
  }

  // isTrustedAuthURL 只允许华为云门户 / 本机回环的授权地址。
  //
  // 授权 URL 由网关自己拼出来（/admin/api/login/start），理论上不可注入；
  // 但这里是唯一一处「拿后端返回的 URL 去跳转」的地方，加一道白名单成本很低，
  // 可以挡住管理密码泄露、或将来某个 bug 让 portal_host 变成可控的情况。
  function isTrustedAuthURL(raw){
    var u;
    try { u = new URL(raw); } catch { return false; }
    if (u.protocol !== "https:" && u.protocol !== "http:") return false;
    var host = u.hostname.toLowerCase();
    if (u.protocol === "http:") {
      // 明文 http 只允许本机（本地联调）
      return host === "127.0.0.1" || host === "localhost" || host === "::1";
    }
    return host === "huaweicloud.com" || host.endsWith(".huaweicloud.com")
        || host === "huaweicloud.cn"  || host.endsWith(".huaweicloud.cn")
        || host === "huawei.com"      || host.endsWith(".huawei.com");
  }

  // setLoginURL 把授权地址写进只读输入框，并只在通过白名单时才挂到链接上。
  function setLoginURL(raw, callback, tunnel){
    var ok = isTrustedAuthURL(raw);
    $("#loginURL").value = raw || "";
    $("#loginCallback").textContent = callback || "—";
    $("#loginTunnel").value = tunnel || "";
    var a = $("#loginLink");
    if (ok) {
      a.href = raw;
      a.classList.remove("hidden");
    } else {
      a.removeAttribute("href");
      a.classList.add("hidden");
      toast("授权地址不在白名单内，已禁止直接打开：\n" + raw +
        "\n（仅允许 *.huaweicloud.com / *.huaweicloud.cn / *.huawei.com 与本机回环）", "err");
    }
    return ok;
  }

  // --- events ---
  var TAB_IDS = { overview:1, usage:1, balance:1, accounts:1, keys:1, guide:1, logs:1, settings:1 };
  var TAB_META = {
    overview: { title:"概览", hint:"运行状态与总体用量", doc:"概览" },
    usage:    { title:"用量", hint:"用量、积分与时序明细", doc:"用量" },
    balance:  { title:"余额", hint:"各账号积分水位与每日变化", doc:"余额" },
    accounts: { title:"账号", hint:"账号池、凭据与活动福利领取", doc:"账号" },
    keys:     { title:"密钥", hint:"API 密钥的签发与吊销", doc:"密钥" },
    guide:    { title:"接入", hint:"把客户端指向这个网关", doc:"接入" },
    logs:     { title:"日志", hint:"请求元信息与错误", doc:"日志" },
    settings: { title:"设置", hint:"运行参数与访问控制", doc:"设置" }
  };
  // resetScrollTop 把窗口滚回顶部。
  //
  // 页面滚动在 window 上（.main 不单独滚），所以 window.scrollTo 就够；
  // scrollingElement 一并归零是为了防某些浏览器在滚动锁定时的残留。
  function resetScrollTop(){
    window.scrollTo(0, 0);
    if (document.scrollingElement) document.scrollingElement.scrollTop = 0;
    document.documentElement.scrollTop = 0;
    document.body.scrollTop = 0;
  }

  // activateTab 是唯一的页签切换入口：同步 class / aria-selected / URL hash。
  // warm=false 时只切显示、不触发额外请求（首次加载用）。
  function activateTab(name, updateHash, warm){
    if (!TAB_IDS[name]) return;
    document.querySelectorAll("nav button").forEach(function(x){
      var sel = x.dataset.tab === name;
      x.classList.toggle("on", sel);
      x.setAttribute("aria-selected", sel ? "true" : "false");
      x.tabIndex = sel ? 0 : -1;
    });
    document.querySelectorAll("section[role=tabpanel]").forEach(function(x){
      x.classList.toggle("on", x.id === "tab-" + name);
    });
    var meta = TAB_META[name];
    $("#pageTitle").textContent = meta.title;
    $("#pageHint").textContent = meta.hint;
    document.title = meta.doc + " · CodeArts2API 控制台";
    // 页签之间不复用滚动位置：概览滚到底再切到用量，停在页面中间会
    // 让人以为「新页签没内容」。切换即回到顶部（含 hashchange 的深链）。
    resetScrollTop();
    // 隐藏的 section 宽度是 0，KPI 列数只能在页面可见后算。
    layoutKpis($("#kpis"));
    layoutKpis($("#usageKpis"));
    if (updateHash) {
      try { history.replaceState(null, "", "#" + name); } catch { location.hash = name; }
    }
    if (warm === false) return;
    if (name === "logs") { refreshLogs(); refreshSamples(); }
    if (name === "overview") { loadWelfare(); }
    if (name === "usage") { refreshUsage(); }
    if (name === "balance") { refreshBalance(); }
    if (name === "accounts") { loadCredits(false); loadUsageForAccounts(); }
    if (name === "guide") renderGuide();
  }
  document.querySelectorAll("nav button").forEach(function(b){
    b.onclick = function(){ activateTab(b.dataset.tab, true); };
    // WAI-ARIA tabs：左右方向键切换
    b.addEventListener("keydown", function(e){
      var keys = Object.keys(TAB_IDS);
      var i = keys.indexOf(b.dataset.tab);
      if (e.key === "ArrowRight") { i = (i + 1) % keys.length; }
      else if (e.key === "ArrowLeft") { i = (i - 1 + keys.length) % keys.length; }
      else return;
      e.preventDefault();
      activateTab(keys[i], true);
      document.querySelector('nav button[data-tab="' + keys[i] + '"]').focus();
    });
  });
  window.addEventListener("hashchange", function(){
    activateTab(location.hash.slice(1), false);
  });

  // --- 设置页子页签 ---
  //
  // 设置项二十来个，全平铺时「活动福利领取时刻」得滚两屏才看得到。按类型拆成
  // 5 个子页签，与主页面签共用一套交互（class on / aria-selected / 方向键切换）。
  // 子页签**不写 URL hash** —— 主页面签已经占了 #settings，写进去会让
  // 「刷新后回到设置页」变成「回到设置页的某个分类」。选择记在 localStorage。
  var PANE_IDS = ["models", "service", "limits", "auto", "search"];
  var PANE_KEY = "codearts2api_settings_pane";
  function activatePane(name, focus){
    if (PANE_IDS.indexOf(name) < 0) name = PANE_IDS[0];
    document.querySelectorAll(".subtab").forEach(function(b){
      var sel = b.dataset.pane === name;
      b.classList.toggle("on", sel);
      b.setAttribute("aria-selected", sel ? "true" : "false");
      b.tabIndex = sel ? 0 : -1;
      if (sel && focus) b.focus();
    });
    document.querySelectorAll(".settings-pane").forEach(function(p){
      p.classList.toggle("on", p.id === "pane-" + name);
    });
    try { localStorage.setItem(PANE_KEY, name); } catch(e){}
  }
  document.querySelectorAll(".subtab").forEach(function(b){
    b.onclick = function(){ activatePane(b.dataset.pane); };
    b.addEventListener("keydown", function(e){
      var i = PANE_IDS.indexOf(b.dataset.pane);
      if (e.key === "ArrowRight") { i = (i + 1) % PANE_IDS.length; }
      else if (e.key === "ArrowLeft") { i = (i - 1 + PANE_IDS.length) % PANE_IDS.length; }
      else return;
      e.preventDefault();
      activatePane(PANE_IDS[i], true);
    });
  });
  (function initSettingsPane(){
    var saved = null;
    try { saved = localStorage.getItem(PANE_KEY); } catch(e){}
    activatePane(saved || PANE_IDS[0]);
  })();

  on("#savePw", function(){
    storePw(pw());
    updatePwToggle();
    $("#pwPanel").classList.remove("open");
    $("#pwToggle").setAttribute("aria-expanded", "false");
    toast("已在本标签页记住管理密码（关闭标签页即失效）", "ok");
    return refresh();
  });
  on("#forgetPw", function(){
    clearStoredPw();
    $("#pw").value = "";
    updatePwToggle();
    toast("已清除本标签页记住的管理密码", "ok");
  });
  // 顶栏密码框折叠：默认只露一个锁图标，点开才展开输入框。
  on("#pwToggle", function(){
    var panel = $("#pwPanel");
    var open = panel.classList.toggle("open");
    $("#pwToggle").setAttribute("aria-expanded", open ? "true" : "false");
    if (open) $("#pw").focus();
  });
  // 点面板外部自动收起，避免挡住下面的内容
  document.addEventListener("click", function(ev){
    var panel = $("#pwPanel");
    if (!panel || !panel.classList.contains("open")) return;
    if (panel.contains(ev.target) || $("#pwToggle").contains(ev.target)) return;
    panel.classList.remove("open");
    $("#pwToggle").setAttribute("aria-expanded", "false");
  });
  // 更新顶栏锁图标的状态文案
  function updatePwToggle(){
    var btn = $("#pwToggle");
    var txt = $("#pwToggleText");
    if (!btn || !txt) return;
    var typed = pw() !== "";
    var label, title, on;
    if (!serverHasPw) {
      // 没设密码：管理接口只靠「本机直连」兜底，不是「未解锁」。
      label = "无需密码";
      title = "未设置管理密码：管理接口仅允许本机直连；需要远程访问请到「设置 → 管理密码」配置";
      on = false;
    } else if (typed) {
      label = "已解锁";
      title = "已记住管理密码（仅本标签页）";
      on = true;
    } else {
      label = "未解锁";
      title = "需要管理密码才能访问账号、密钥等接口";
      on = false;
    }
    txt.textContent = label;
    btn.classList.toggle("on", on);
    btn.title = title;

    var hint = $("#pwModeHint");
    if (hint) {
      hint.textContent = serverHasPw
        ? "只在本标签页记住，关闭标签页即失效。密码等价于「导出完整登录态」的权限。"
        : "当前没有设置管理密码，管理接口只允许本机直连访问；要远程访问，请到「设置 → 管理密码」设置一个。";
    }
  }
  on("#refresh", function(){
    var jobs = [refresh()];
    if ($("#tab-logs").classList.contains("on")) jobs.push(refreshLogs(), refreshSamples());
    return Promise.all(jobs);
  });
  on("#importLocal", function(){ return accountOp("import", {}); });
  // 凭据类型切换：永久 AK/SK 不需要 security_token，隐藏该项并更新提示。
  // 注意 select 用 change 事件，不能复用只绑 onclick 的 on()。
  $("#manualKind").addEventListener("change", function(){
    var isAKSK = this.value === "aksk";
    $("#manualSTSField").classList.toggle("hidden", isAKSK);
    if (isAKSK) $("#manualSTS").value = "";
    $("#manualKindHint").textContent = isAKSK
      ? "永久 AK/SK 不过期、无需续期；请勿与 STS 临时凭据混用。"
      : "STS 临时凭据约 2 小时过期，到期需重新录入或用 refresh_token 续期。";
  });
  on("#addManual", function(){
    var ak = $("#manualAK").value.trim(), sk = $("#manualSK").value.trim();
    var kind = $("#manualKind").value;
    if (!ak || !sk) { toast("请填写 Access Key 与 Secret Key", "err"); return; }
    var payload = {
      name: $("#manualName").value.trim(), access_key: ak, secret_key: sk,
      credential_kind: kind, note: $("#manualNote").value.trim()
    };
    if (kind !== "aksk") payload.security_token = $("#manualSTS").value.trim();
    return accountOp("manual", payload).then(function(){
      $("#manualAK").value = ""; $("#manualSK").value = ""; $("#manualSTS").value = "";
    });
  });
  on("#addSession", function(){
    var js = $("#sessionJSON").value.trim();
    if (!js) { toast("请粘贴 session JSON", "err"); return; }
    return accountOp("session-json", {
      name: $("#sessionName").value.trim(),
      note: $("#sessionNote").value.trim(),
      session_json: js
    }).then(function(){
      $("#sessionJSON").value = "";
    });
  });
  on("#checkinAll", function(){ return checkin(null); });
  on("#refreshCredits", function(){ return loadCredits(true); });
  on("#probeAllModels", function(){
    var ids = Object.keys(lastModels).filter(function(id){
      return lastModels[id].source !== "alias";
    });
    if (!ids.length) { toast("模型清单为空，先点「从上游刷新清单」", "err"); return; }
    // 每个模型都是一次真实请求、真实扣积分，全量探测前必须二次确认。
    if (!window.confirm("将向 " + ids.length + " 个模型各发一次真实请求（max_tokens=8，会真实扣积分），确定继续？")) return;
    ids.forEach(function(id){ modelProbeState[id] = { state: "busy" }; redrawModelRow(id); });
    $("#modelHint").textContent = "正在测活 " + ids.length + " 个模型…（每个会真实消耗极少量积分）";

    return api("/admin/api/models/probe", {method:"POST", body:{}}).then(function(r){
      (r.results || []).forEach(function(x){
        modelProbeState[x.model] = x.ok
          ? { state: "ok", account: x.account }
          : { state: "err", err: x.error || "未知错误", account: x.account };
        redrawModelRow(x.model);
      });
      $("#modelHint").textContent = "可用 " + (r.available||0) + " / " + (r.total||0) +
        (r.unavailable ? "，不可用 " + r.unavailable : "");
      toast("测活完成：" + (r.available||0) + "/" + (r.total||0) + " 可用", "ok");
    }).catch(function(e){
      var msg = (e && e.message) ? e.message : "未知错误";
      ids.forEach(function(id){ modelProbeState[id] = { state: "err", err: msg }; redrawModelRow(id); });
      $("#modelHint").textContent = "测活失败：" + msg;
      toast("测活失败：" + msg, "err");
    });
  });
  on("#claimAll", function(){ return claim(null); });
  on("#loadWelfare", loadWelfare);
  // 切换账号后重新拉该账号的活动清单（各账号的领取状态不同）
  $("#welfareAccount").addEventListener("change", function(){
    welfareAccountID = this.value;
    loadWelfare();
  });
  on("#claimWelfareNow", function(){ return claim(null).then(loadWelfare); });
  on("#genInvite", function(){
    toast("生成中…");
    // 用当前选中的账号生成，避免「看到的是 A 号、生成的是 B 号」
    var body = welfareAccountID ? {id: welfareAccountID} : {};
    return api("/admin/api/invite", {method:"POST", body:body}).then(function(r){
      $("#inviteURL").value = r.invite_url || "";
      $("#inviteStat").textContent = "已激活 " + (r.activation_count||0) + " / " + (r.max_activations||0) +
        " 次 · 状态 " + (r.status||"") + " · 账号 " + (r.account||"");
      $("#invitePanel").classList.remove("hidden");
      toast("已生成邀请链接", "ok");
    }).catch(function(e){ toast("生成失败：" + e.message, "err"); });
  });
  on("#copyInvite", function(){ return copyText($("#inviteURL").value, "已复制邀请链接"); });
  on("#browserLogin", function(){
    api("/admin/api/login/start", {method:"POST", body:{}}).then(function(r){
      setLoginURL(r.auth_url, r.callback, r.tunnel_hint);
      $("#loginPasted").value = "";
      $("#loginStatus").textContent = "等待在浏览器里完成登录…";
      $("#loginPanel").classList.remove("hidden");
      stopLogin();
      loginSid = r.session_id;
      loginTimer = setInterval(pollLogin, 2000);
    }).catch(function(e){ toast("启动登录失败：" + e.message, "err"); });
  });
  on("#loginCopy", function(){
    var v = $("#loginURL").value;
    if (!v) { toast("请先点击「浏览器登录」获取授权地址", "err"); return; }
    return copyText(v, "已复制授权地址");
  });
  on("#copyExport", function(){
    return copyText($("#exportJSON").value, "已复制到剪贴板（注意不要外传）");
  });
  on("#closeExport", function(){
    $("#exportJSON").value = "";
    $("#exportPanel").classList.add("hidden");
  });
  on("#loginSubmit", function(){
    var u = $("#loginPasted").value.trim();
    if (!u) { toast("请把浏览器地址栏里那条回调 URL 粘进来", "err"); return; }
    if (!loginSid) { toast("登录会话已失效，请重新点击「浏览器登录」", "err"); return; }
    api("/admin/api/login/callback", {method:"POST", body:{session_id:loginSid, url:u}}).then(function(){
      $("#loginStatus").textContent = "已收到授权码，正在换取凭据…";
      $("#loginPasted").value = "";
      pollLogin();
    }).catch(function(e){ toast("提交失败：" + e.message, "err"); });
  });
  on("#copyTunnel", function(){
    var v = $("#loginTunnel").value;
    if (!v) { toast("请先点击「浏览器登录」", "err"); return; }
    return copyText(v, "已复制 SSH 隧道命令");
  });
  on("#loginCancel", function(){
    if (loginSid) api("/admin/api/login/cancel", {method:"POST", body:{session_id:loginSid}}).catch(function(){});
    stopLogin();
    $("#loginPanel").classList.add("hidden");
  });
  on("#refreshModels", function(){
    toast("正在从上游刷新模型清单…");
    api("/admin/api/models/refresh", {method:"POST"}).then(function(r){
      toast("已刷新，共 " + (r.models||[]).length + " 个模型", "ok");
      return refresh();
    }).catch(function(e){ toast("刷新失败：" + e.message, "err"); });
  });
  on("#addKey", function(){
    api("/admin/api/keys", {method:"POST", body:{name:$("#keyName").value.trim()}}).then(function(r){
      $("#keyName").value = "";
      // 明文只出现这一次：能自动复制就先复制，复制失败才把全文打出来。
      return copyText(r.key, "已创建密钥并复制到剪贴板").then(function(ok){
        if (!ok) toast("已创建密钥（仅显示一次，请立即保存）：\n" + r.key, "ok");
        return refresh();
      });
    }).catch(function(e){ toast("创建失败：" + e.message, "err"); });
  });
  on("#reloadLogs", refreshLogs);
  on("#reloadSamples", refreshSamples);
  // 开关/采样率一改就生效（后端热更新，不用重启）。
  $("#dbgCapture").onchange = function(){
    return saveDebug({ debug_capture: $("#dbgCapture").checked });
  };
  $("#dbgRate").onchange = function(){
    return saveDebug({ debug_capture_sample: parseInt($("#dbgRate").value, 10) || 1 });
  };
  // 设置页的同一个开关：改完立即生效，不必再点保存。
  $("#sDebugCapture").onchange = function(){
    return saveDebug({ debug_capture: $("#sDebugCapture").checked });
  };
  $("#sDebugRate").onchange = function(){
    return saveDebug({ debug_capture_sample: parseInt($("#sDebugRate").value, 10) || 1 });
  };
  on("#clearLogs", function(){
    if (!confirm("确定清空全部日志？")) return;
    api("/admin/api/logs", {method:"DELETE"}).then(function(){ toast("已清空","ok"); return refreshLogs(); })
      .catch(function(e){ toast("失败：" + e.message, "err"); });
  });
  on("#clearStats", function(){
    if (!confirm("确定重置统计？")) return;
    api("/admin/api/stats", {method:"DELETE"}).then(function(){ toast("已重置","ok"); return refresh(); })
      .catch(function(e){ toast("失败：" + e.message, "err"); });
  });
  on("#testSearch", function(){
    var q = $("#sSearchQuery").value.trim();
    if (!q) { toast("先填一个测试关键词", "warn"); return; }
    return api("/admin/api/websearch/test", {method:"POST", body:{query:q}}).then(function(r){
      var first = (r.results && r.results[0]) ? r.results[0].title : "";
      toast("搜索成功：" + (r.count || 0) + " 条" + (first ? "（" + first + "）" : ""), "ok");
    }).catch(function(e){ toast("搜索失败：" + e.message, "err"); });
  });
  // 切检索后端时同步占位符和三条提示：SearXNG 是「内网一个 docker、不需要 Key」，
  // Tavily / Brave 则是「端点留空用官方地址 + 填 API Key」。写死一句「SearXNG 填实例地址；
  // Tavily / Brave 留空」对两种人都没用 —— 选了哪个就只说那个。
  var SEARCH_HINTS = {
    searxng: {
      provider: "docker 起一个即可。",
      endpoint: "实例地址，如 http://searxng:8080。",
      key: "挂了 Basic Auth 就填 user:pass。",
      endpointPh: "http://searxng:8080",
      keyPh: "user:pass（可选）"
    },
    tavily: {
      provider: "官方托管。",
      endpoint: "留空用官方地址。",
      key: "Tavily API Key。",
      endpointPh: "",
      keyPh: "tvly-..."
    },
    brave: {
      provider: "官方托管。",
      endpoint: "留空用官方地址。",
      key: "Brave API Key。",
      endpointPh: "",
      keyPh: "Brave Subscription Token"
    }
  };
  // searchKeySet：已存过 Key 时输入框语义是「留空不改」，提示要以它为准。
  var searchKeySet = false;

  // 服务端只存一个 websearch_endpoint（当前 provider 的），但端点天然是按后端分开的：
  // 切到 Tavily 还带着 SearXNG 的 http://searxng:8080 就是错的（会被当成 Tavily 端点）。
  // 所以每个 provider 各记一份在 localStorage，切换时找回自己的值，保存时只交当前那个。
  var SEARCH_EP_KEY = "codearts2api_search_endpoints";
  var searchEndpoints = {};
  try {
    searchEndpoints = JSON.parse(localStorage.getItem(SEARCH_EP_KEY) || "{}") || {};
  } catch (e) { searchEndpoints = {}; }
  function persistSearchEndpoints(){
    try { localStorage.setItem(SEARCH_EP_KEY, JSON.stringify(searchEndpoints)); } catch (e) {}
  }

  function syncSearchProviderHint(){
    var p = $("#sSearchProvider").value;
    var h = SEARCH_HINTS[p] || SEARCH_HINTS.searxng;
    setText($("#sSearchProviderHint"), h.provider);
    setText($("#sSearchEndpointHint"), h.endpoint);
    setText($("#sSearchKeyHint"), searchKeySet ? "已设置时留空表示不修改。" : h.key);
    setAttr($("#sSearchEndpoint"), "placeholder", h.endpointPh);
    setAttr($("#sSearchKey"), "placeholder", searchKeySet ? "已设置，留空则不修改" : h.keyPh);
    // 端点框换成这个后端自己记的地址（没填过就留空 → 用官方地址）。
    setValue($("#sSearchEndpoint"), searchEndpoints[p] || "");
  }
  if ($("#sSearchProvider")) {
    $("#sSearchProvider").addEventListener("change", syncSearchProviderHint);
    // 边输边记：change 触发时 select 已经是新 provider，来不及回存旧值。
    $("#sSearchEndpoint").addEventListener("input", function(){
      searchEndpoints[$("#sSearchProvider").value] = this.value.trim();
      persistSearchEndpoints();
    });
    syncSearchProviderHint();
  }
  // 数字输入框：空值/null 表示「这次不改这个字段」（服务端收到 null 就保持原值），
  // 不能退化成 0 —— 否则用户清空分钟格再点保存，领取时刻会被悄悄改成整点。
  function numOrNull(id){
    var el = $(id);
    var v = el ? el.value.trim() : "";
    if (v === "") return null;
    var n = parseInt(v, 10);
    return isNaN(n) ? null : n;
  }
  on("#saveSettings", function(){
    var aliases;
    try { aliases = JSON.parse($("#sAliases").value || "{}"); }
    catch { toast("模型别名不是合法 JSON", "err"); return; }
    var body = {
      model_aliases: aliases,
      default_model: $("#sDefaultModel").value.trim(),
      force_model: $("#sForceModel").value.trim(),
      request_timeout_sec: parseInt($("#sTimeout").value, 10) || 0,
      max_retries: parseInt($("#sRetries").value, 10) || 0,
      log_keep: parseInt($("#sLogKeep").value, 10) || 0,
      live_ttl_sec: parseInt($("#sLiveTTL").value, 10) || 0,
      cors_origin: $("#sCors").value.trim(),
      user_data_dir: $("#sUserData").value.trim(),
      login_port: parseInt($("#sLoginPort").value, 10) || 0,
      allow_credit_models: $("#sAllowCredit").checked,
      model_fallback: $("#sModelFallback").checked,
      debug_capture: $("#sDebugCapture").checked,
      debug_capture_sample: parseInt($("#sDebugRate").value, 10) || 1,
      credit_priority: $("#sCreditPriority").checked,
      allow_anonymous: $("#sAllowAnonymous").checked,
      rate_limit_enabled: $("#sRateLimit").checked,
      rate_limit_qps: parseInt($("#sRateQPS").value, 10) || 0,
      rate_limit_burst: parseInt($("#sRateBurst").value, 10) || 0,
      rate_limit_concurrency: parseInt($("#sRateConc").value, 10) || 0,
      auto_claim: $("#sAutoClaim").checked,
      claim_after_hour: numOrNull("#sClaimHour"),
      claim_after_minute: numOrNull("#sClaimMinute"),
      auto_refresh: $("#sAutoRefresh").checked,
      write_back_to_ide: $("#sWriteBack").checked,
      plugin_name: $("#sPluginName").value.trim(),
      plugin_version: $("#sPluginVersion").value.trim(),
      ide_name: $("#sIDEName").value.trim(),
      ide_version: $("#sIDEVersion").value.trim(),
      websearch_enabled: $("#sSearchEnabled").checked,
      websearch_provider: $("#sSearchProvider").value,
      websearch_endpoint: $("#sSearchEndpoint").value.trim(),
      websearch_max_results: parseInt($("#sSearchResults").value, 10) || 5,
      websearch_timeout_sec: parseInt($("#sSearchTimeout").value, 10) || 8,
      websearch_cache_ttl_sec: parseInt($("#sSearchCache").value, 10) || 0,
      websearch_max_page_chars: parseInt($("#sSearchPageChars").value, 10) || 1500,
      websearch_fetch_pages: $("#sSearchFetch").checked
    };
    // API Key 留空 = 不修改（与管理员密码同一策略，避免每次保存都抹掉密钥）。
    var sk = $("#sSearchKey").value.trim();
    if (sk !== "") body.websearch_api_key = sk;
    var np = $("#sPw").value;
    if (np !== "") body.admin_password = np;
    return api("/admin/api/settings", {method:"POST", body:body}).then(function(r){
      $("#sPw").value = "";
      toast("设置已保存", "ok");
      clearSettingsDirty();
      renderSettings(r.settings || {});
      if (np !== "") { storePw(np); $("#pw").value = np; }
      return refresh();
    }).catch(function(e){ toast("保存失败：" + e.message, "err"); });
  });
  on("#clearPw", function(){
    if (!confirm("确定清除管理密码？\n清除后管理接口仅允许本机直连访问，反向代理 / 容器场景将无法再打开控制台。")) return;
    return api("/admin/api/settings", {method:"POST", body:{clear_admin_password:true}}).then(function(r){
      $("#sPw").value = "";
      clearStoredPw();
      $("#pw").value = "";
      toast("管理密码已清除", "ok");
      clearSettingsDirty();
      renderSettings(r.settings || {});
      return refresh();
    }).catch(function(e){ toast("清除失败：" + e.message, "err"); });
  });
  $("#logLimit").onchange = function(){ refreshLogs(); };
  ["#logStatus", "#logClass"].forEach(function(sel){
    $(sel).onchange = function(){ refreshLogs(); };
  });
  // 关键字搜索：回车触发，避免每敲一个字母就打一次接口。
  $("#logQ").addEventListener("keydown", function(e){
    if (e.key === "Enter") refreshLogs();
  });
  $("#logQ").addEventListener("search", function(){ refreshLogs(); });
  $("#usageDays").onchange = refreshUsage;
  $("#balDays").onchange = refreshBalance;
  on("#balRefresh", function(){ return refreshBalance(); });
  $("#hourHours").onchange = refreshUsage;
  $("#logMinCredits").oninput = function(){ refreshLogs(true); };
  // 示例块折叠：<details> 自带开关，这里只做「全部折叠/展开」批量操作。
  on("#toggleAllExamples", function(){
    var blocks = Array.prototype.slice.call(document.querySelectorAll("details.guide-block"));
    if (!blocks.length) return;
    var anyClosed = blocks.some(function(d){ return !d.open; });
    blocks.forEach(function(d){ d.open = anyClosed; });
    $("#toggleAllExamples").textContent = anyClosed ? "全部折叠" : "全部展开";
  });
  // 单个折叠后同步批量按钮的文案。
  document.querySelectorAll("details.guide-block").forEach(function(d){
    d.addEventListener("toggle", function(){
      var blocks = Array.prototype.slice.call(document.querySelectorAll("details.guide-block"));
      var anyClosed = blocks.some(function(x){ return !x.open; });
      $("#toggleAllExamples").textContent = anyClosed ? "全部展开" : "全部折叠";
    });
  });
  on("#copyEndpoint", function(){ return copyText(endpointURL(), "已复制端点"); });
  on("#speedEndpoint", function(){
    // tcptest.cn 的 HTTP 测速把目标放在路径段里：/http/:target
    // （路由定义见其前端 `path:"/http/:target?"`），不带 scheme。
    // 目标直接拼在路径段里，不做 encodeURIComponent：冒号在路径段里是合法
    // 字符（RFC 3986 pchar 含 ":"），编码成 %3A 反而可能让站点解析不出端口。
    window.open("https://www.tcptest.cn/http/" + location.host, "_blank", "noopener");
  });
  on("#copyBase", function(){ return copyText($("#guideBase").value, "已复制"); });
  on("#copyAnthropic", function(){ return copyText($("#guideAnthropic").value, "已复制"); });
  on("#copyKey", copyGuideKey);
  [
    ["#copyChat", "#exChat"],
    ["#copyResponses", "#exResponses"],
    ["#copyAnthropicCurl", "#exAnthropic"],
    ["#copyClaude", "#exClaude"],
    ["#copyPython", "#exPython"]
  ].forEach(function(pair){
    on(pair[0], function(){ return copyText($(pair[1]).textContent, "已复制"); });
  });

  // 只读输入框：点击即全选。
  // 用事件绑定而不是内联 onclick 属性，这样 CSP 不需要放开内联事件处理器。
  document.querySelectorAll("[data-select-on-click]").forEach(function(el){
    el.addEventListener("click", function(){ this.select(); });
  });

  // 设置表单一旦被编辑就标记为脏，refresh() 不再覆盖它。
  ["input", "change"].forEach(function(evt){
    $("#tab-settings").addEventListener(evt, markSettingsDirty);
  });

  // --- 主题（深色 / 浅色） ---
  //
  // 首次访问跟随系统偏好，手动切换后写 localStorage 记住。
  // 只切 data-theme 属性，颜色全在 CSS 变量里 —— 不重建任何 DOM。
  var THEME_KEY = "codearts2api_theme";
  function applyTheme(t){
    document.documentElement.setAttribute("data-theme", t);
    var dark = t !== "light";
    $("#themeIconDark").classList.toggle("hidden", !dark);
    $("#themeIconLight").classList.toggle("hidden", dark);
    var label = dark ? "切换到浅色主题" : "切换到深色主题";
    $("#themeToggle").setAttribute("aria-label", label);
    $("#themeToggle").title = label;
  }
  function initTheme(){
    var saved = null;
    try { saved = localStorage.getItem(THEME_KEY); } catch(e){}
    if (saved !== "light" && saved !== "dark") {
      saved = (window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches)
        ? "light" : "dark";
    }
    applyTheme(saved);
  }
  on("#themeToggle", function(){
    var next = document.documentElement.getAttribute("data-theme") === "light" ? "dark" : "light";
    applyTheme(next);
    try { localStorage.setItem(THEME_KEY, next); } catch(e){}
  });
  initTheme();

  var initialPw = loadPw();
  if (initialPw) $("#pw").value = initialPw;
  updatePwToggle();
  if (pwMigrated) {
    toast("已清除旧版本存在 localStorage 里的明文管理密码，请重新输入一次。", "err");
  }
  $("#pw").addEventListener("keydown", function(e){ if (e.key === "Enter") { $("#savePw").click(); } });
  $("#toast").addEventListener("click", function(){ this.className = "toast"; });

  var initialTab = location.hash.slice(1);
  initialTab = TAB_IDS[initialTab] ? initialTab : "overview";
  activateTab(initialTab, false, true);
  renderEndpoint();
  refresh();
  refreshUsage();
  setInterval(loadHealth, 20000);
  // 每 5 秒增量刷新一次（只改变化的 DOM，不整页刷新）。
  startAutoRefresh();
})();
