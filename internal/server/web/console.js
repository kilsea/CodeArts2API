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
  function renderKpis(kpis){
    var box = $("#kpis");
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
  }

  // 概览页拆成两路：KPI 跟着请求计数变，模型表只在上游清单变时重建。
  // 以前两者共用一个指纹，发一次请求就把整张模型表重建一遍（12 行 DOM）。
  function renderKpiCards(d){
    var s = d.stats || {};
    var accts = d.accounts || [];
    var ready = accts.filter(function(a){ return a.enabled && a.status === "ok"; }).length;
    var claimed = accts.filter(function(a){ return a.claimed_today; }).length;
    var soonest = null;
    accts.forEach(function(a){
      if (a.expires_at && (soonest === null || a.expires_at < soonest)) soonest = a.expires_at;
    });
    renderKpis([
      { k:"总请求", v: fmtNum(s.total_requests || 0) },
      { k:"失败请求", v: fmtNum(s.failed_requests || 0) },
      { k:"输出字符", v: fmtNum(s.total_chars_out || 0) },
      { k:"账号（就绪/总）", v: ready + " / " + accts.length,
        sub: soonest ? "最早过期 " + fmtShort(soonest) : "", title: soonest ? fmtTime(soonest) : "" },
      { k:"密钥", v: (d.keys || []).length },
      { k:"今日领取", v: claimed + " / " + accts.length },
      { k:"可用模型", v: (d.models || []).length,
        sub: (d.settings || {}).allow_credit_models ? "计费已开启" : "计费已关闭" },
      { k:"启动时间", v: fmtShort(s.started_at), title: fmtTime(s.started_at) }
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
    setText($("#modelHint"), d.upstream_error ? ("最近一次上游刷新失败：" + d.upstream_error) : "");
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

  // loadCredits 查询账号积分额度（上游统计插件）。
  // 默认走服务端 60 秒缓存；force=true 时带 ?fresh=1 强制回源。
  function loadCredits(force){
    if (creditLoading) return creditLoading;
    var accts = (state.data && state.data.accounts) || [];
    if (!accts.length) return Promise.resolve();
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
      updateCreditCells();
    }).catch(function(e){
      var msg = (e && e.message) || "查询失败";
      accts.forEach(function(a){ creditState[a.id] = { loading:false, ok:false, error:msg }; });
      updateCreditCells();
      toast("查询积分额度失败：" + msg, "err");
    }).then(function(){ creditLoading = null; });
    return creditLoading;
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
    if (info.expiring) bits.push("即将到期 " + fmtNum(info.expiring));
    if (info.reset_date) bits.push("重置 " + info.reset_date);
    if (info.status && info.status !== "active") bits.push(info.status);
    sub.hidden = false;
    setText(sub, bits.join(" · "));
    setAttr(sub, "title", (info.package_name || "") + (info.spec_code ? (" / " + info.spec_code) : ""));
  }

  function renderAccounts(d){
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
        statusCell.appendChild(pill(okStatus ? "ok" : (a.status || "error"), okStatus ? "ok" : "err"));
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
      ops.appendChild(btn("重读", "xs", function(){ accountOp("reimport", {id:a.id}); }));
      ops.appendChild(btn("导出", "xs", function(){ exportSession(a.id); }));
      ops.appendChild(btn(a.enabled ? "停用" : "启用", "xs", function(){ accountOp("toggle", {id:a.id}); }));
      ops.appendChild(btn("备注", "xs", function(){
        var v = prompt("账号备注（如注册手机号）", a.note || "");
        if (v === null) return;
        accountOp("set-note", {id:a.id, note:v.trim()});
      }));
      if (a.failures > 0) {
        ops.appendChild(btn("清错", "xs", function(){ accountOp("clear-errors", {id:a.id}); }));
      }
      ops.appendChild(btn("删除", "xs danger", function(){
        if (confirm("确定删除账号 " + a.name + "？")) accountOp("remove", {id:a.id});
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
      tb.appendChild(emptyRow(10, filtered ? "没有匹配的日志（试试清空筛选条件）" : "暂无日志"));
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
    return row([
      cell(fmtTime(l.time), "muted"), cell(l.model, "mono"), cell(l.stream ? "是" : "否", "mono"),
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
    setValue($("#sSearchEndpoint"), s.websearch_endpoint || "");
    setAttr($("#sSearchKey"), "placeholder", s.websearch_api_key_set ? "已设置，留空则不修改" : "SearXNG 留空");
    setValue($("#sSearchKey"), "");
    setValue($("#sSearchResults"), s.websearch_max_results || 5);
    setValue($("#sSearchTimeout"), s.websearch_timeout_sec || 8);
    setValue($("#sSearchCache"), (s.websearch_cache_ttl_sec === undefined ? 300 : s.websearch_cache_ttl_sec));
    setValue($("#sSearchPageChars"), (s.websearch_max_page_chars === undefined ? 1500 : s.websearch_max_page_chars));
    setChecked($("#sSearchFetch"), !!s.websearch_fetch_pages);
  }

  function markSettingsDirty(){
    if (settingsDirty) return;
    settingsDirty = true;
    $("#settingsDirty").classList.remove("hidden");
  }
  function clearSettingsDirty(){
    settingsDirty = false;
    $("#settingsDirty").classList.add("hidden");
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
      if (!opts.silent) loadHealth();
      var now = new Date();
      var p2 = function(n){ return (n < 10 ? "0" : "") + n; };
      $("#lastSync").textContent = "已更新 " + p2(now.getHours()) + ":" + p2(now.getMinutes()) + ":" + p2(now.getSeconds());
      // 账号页可见时补一次额度（首次进页签时 refresh 可能还没回来）
      if ($("#tab-accounts").classList.contains("on")) loadCredits(false);
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

  // --- 用量趋势 ---

  function refreshUsage(){
    var days = parseInt($("#usageDays").value, 10) || 7;
    return api("/admin/api/usage?days=" + days).then(function(d){
      if (!changed("usage", d)) return;
      renderUsage(d);
    }).catch(function(){ /* 自动刷新时静默 */ });
  }

  function renderUsage(d){
    var rows = d.rows || [];
    var days = d.days || 7;
    var byDay = {};
    rows.forEach(function(r){ byDay[r.day] = (byDay[r.day] || 0) + r.requests; });

    // 补齐没有请求的日子：不补的话柱状图会跳过空白天，看起来像天天在涨。
    var list = [];
    var today = new Date();
    var p2 = function(n){ return (n < 10 ? "0" : "") + n; };
    for (var i = days - 1; i >= 0; i--) {
      var dt = new Date(today.getFullYear(), today.getMonth(), today.getDate() - i);
      var key = dt.getFullYear() + "-" + p2(dt.getMonth() + 1) + "-" + p2(dt.getDate());
      list.push({ day: key, label: (dt.getMonth() + 1) + "/" + dt.getDate(), n: byDay[key] || 0 });
    }
    var max = 1;
    list.forEach(function(x){ if (x.n > max) max = x.n; });

    var box = $("#usageChart");
    box.textContent = "";
    list.forEach(function(x){
      var col = mk("div", "usage-bar" + (x.n ? "" : " zero"));
      col.title = x.day + "：" + x.n + " 次请求";
      col.appendChild(mk("div", "val", x.n ? String(x.n) : ""));
      var bar = mk("div", "bar");
      bar.style.height = Math.max(2, Math.round(x.n / max * 88)) + "px";
      col.appendChild(bar);
      col.appendChild(mk("div", "lbl", x.label));
      box.appendChild(col);
    });

    var byModel = {};
    rows.forEach(function(r){
      var m = byModel[r.model] || (byModel[r.model] = { model: r.model, requests: 0, chars_in: 0, chars_out: 0 });
      m.requests += r.requests;
      m.chars_in += r.chars_in;
      m.chars_out += r.chars_out;
    });
    var models = Object.keys(byModel).map(function(k){ return byModel[k]; })
      .sort(function(a, b){ return b.requests - a.requests; });

    var tb = $("#usageModels");
    tb.textContent = "";
    if (!models.length) {
      tb.appendChild(emptyRow(4, "暂无数据（发一次请求后就有了）"));
      return;
    }
    models.forEach(function(m){
      tb.appendChild(row([
        cell(m.model, "mono"), cell(fmtNum(m.requests), "mono"),
        cell(fmtNum(m.chars_in), "mono"), cell(fmtNum(m.chars_out), "mono")
      ]));
    });
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
      if ($("#tab-overview").classList.contains("on")) refreshUsage();
    }, 5000);
    // 切回前台时立刻补一次，不然要等最多 5 秒。
    document.addEventListener("visibilitychange", function(){
      if (document.hidden) return;
      refresh({silent: true});
      if ($("#tab-logs").classList.contains("on")) { refreshLogs(true); refreshSamples(); }
      if ($("#tab-overview").classList.contains("on")) refreshUsage();
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
  var TAB_IDS = { overview:1, accounts:1, keys:1, guide:1, logs:1, settings:1 };
  var TAB_META = {
    overview: { title:"概览", hint:"运行状态与用量", doc:"概览" },
    accounts: { title:"账号", hint:"账号池、凭据与活动福利领取", doc:"账号" },
    keys:     { title:"密钥", hint:"API 密钥的签发与吊销", doc:"密钥" },
    guide:    { title:"接入", hint:"把客户端指向这个网关", doc:"接入" },
    logs:     { title:"日志", hint:"请求元信息与错误", doc:"日志" },
    settings: { title:"设置", hint:"运行参数与访问控制", doc:"设置" }
  };
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
    if (updateHash) {
      try { history.replaceState(null, "", "#" + name); } catch { location.hash = name; }
    }
    if (warm === false) return;
    if (name === "logs") { refreshLogs(); refreshSamples(); }
    if (name === "overview") { loadWelfare(); refreshUsage(); }
    if (name === "accounts") loadCredits(false);
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
      claim_after_hour: parseInt($("#sClaimHour").value, 10) || 0,
      claim_after_minute: parseInt($("#sClaimMinute").value, 10) || 0,
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
