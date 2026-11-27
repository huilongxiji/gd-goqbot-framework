// 管理页面：概览、连接、插件、日志、配置文件、更新
import { h, $, fill, icon, api, auth, toast, openDialog, confirmDialog, duration, bytes, tokenProblem } from "./core.js";

/** 由 app.js 注入：重启 goqbot 并等它回来 */
let restartFlow = async () => {};
export function setRestartFlow(fn) { restartFlow = fn; }

function page(title, desc, ...body) {
  return h("div", { class: "page" }, h("header", { class: "page-head" }, h("h1", null, title), desc ? h("p", null, desc) : null), ...body);
}

function pill(kind, text) {
  return h("span", { class: "pill " + kind }, text);
}

function errorBox(e) {
  return h("p", { class: "form-error" }, e.message ?? String(e));
}

/** 下游连接的状态文字 */
function connPill(st) {
  if (!st) return pill("muted", "未保存");
  if (st.disabled) return pill("muted", "已停用");
  if (st.error) return pill("bad", "启动失败");
  if (st.type === "ws") return st.clients ? pill("ok", `已连接 ${st.clients} 个`) : pill("wait", "等待连接");
  return st.connected ? pill("ok", "已连接") : pill("wait", "正在连接");
}

/** 页面定时刷新：离开页面时停止 */
function every(ms, fn) {
  let t = setInterval(() => { if (document.visibilityState === "visible") fn(); }, ms);
  return () => clearInterval(t);
}

// ================= 概览 =================

export function renderOverview(root) {
  const box = h("div", { class: "overview" }, h("p", { class: "muted" }, "正在读取…"));
  fill(root, page("概览", null, box));
  const load = async () => {
    try {
      const { info } = await api("GET", "/api/info");
      const qq = info.qq ?? {};
      const conns = info.connections ?? [];
      const active = conns.filter((c) => !c.disabled);
      const online = active.filter((c) => (c.type === "ws" ? c.clients > 0 : c.connected));
      fill(box, 
        h("section", { class: "status-line " + (qq.ready ? "ok" : "bad") },
          h("span", { class: "dot", "aria-hidden": "true" }),
          h("div", null,
            h("h2", null, qq.ready ? `${qq.bot_name || "机器人"} 在线` : "QQ 未连接"),
            h("p", null, qq.ready ? "已连上 QQ 开放平台，正在接收消息" : "还没连上 QQ 开放平台，检查 config.yml 里的 qq 配置和网络")),
          h("button", { class: "btn", type: "button", onclick: async () => {
            if (await confirmDialog("重启 goqbot", "重启期间机器人会短暂离线，所有插件也会重新启动。", "重启")) restartFlow();
          } }, "重启 goqbot")),
        h("section", { class: "panel" },
          h("dl", { class: "facts" },
            fact("版本", info.version),
            fact("运行时长", duration(info.uptime_seconds ?? 0)),
            fact("平台", info.platform),
            fact("self_id", String(info.self_id ?? "")),
            fact("下游连接", `${online.length} / ${active.length} 条已连接`),
            fact("插件", `${info.plugins?.ready ?? 0} / ${info.plugins?.total ?? 0} 个运行中`))),
        h("section", { class: "panel" },
          h("div", { class: "panel-bar" }, h("h2", null, "下游连接"), h("a", { class: "btn ghost small", href: "#/connections" }, "管理连接")),
          conns.length ? h("ul", { class: "rows" }, conns.map((c) => h("li", null,
            h("span", { class: "tag" }, c.type === "ws" ? "正向" : "反向"),
            h("code", null, c.address || "（未填写地址）"),
            connPill(c))))
            : null,
          active.length ? null : h("p", { class: "muted" },
            conns.length ? "下游连接都是关闭的。NoneBot、AstrBot 等框架要接入时，到「连接」页打开对应的连接。"
              : "还没有下游连接。NoneBot、AstrBot 等框架需要通过下游连接接入 goqbot，可以在「连接」页添加。")));
    } catch (e) {
      fill(box, errorBox(e));
    }
  };
  load();
  return every(5000, load);
}

function fact(k, v) {
  return h("div", null, h("dt", null, k), h("dd", null, v));
}

// ================= 连接 =================
// 布局参照 NapCat 的网络配置：类型标签页 + 卡片列表，新建 / 编辑在弹窗里完成；所有操作保存后立即生效

const KINDS = {
  ws: { label: "正向 WS", desc: "goqbot 开一个端口，框架连进来" },
  "ws-reverse": { label: "反向 WS", desc: "goqbot 主动连接框架开的地址（NoneBot 默认用这种）" },
};

/** 随机口令：16 位字母数字 */
function randomToken() {
  const chars = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789";
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) => chars[b % chars.length]).join("");
}

/** "0.0.0.0:6700" / "ws://[::]:6700" → { host, port } */
function splitAddr(a) {
  const s = String(a ?? "").replace(/^wss?:\/\//, "");
  const i = s.lastIndexOf(":");
  if (i < 0) return { host: s, port: "" };
  let host = s.slice(0, i);
  if (host.startsWith("[") && host.endsWith("]")) host = host.slice(1, -1);
  return { host, port: s.slice(i + 1) };
}
function joinAddr(host, port) {
  return (host.includes(":") ? `[${host}]` : host) + ":" + port;
}

function tile(label, value, full = false) {
  return h("div", { class: "tile" + (full ? " full" : "") }, h("span", null, label), h("b", null, value));
}

function switchCard(label, desc, checked, onchange) {
  const input = h("input", { type: "checkbox", role: "switch", onchange: (e) => onchange(e.target.checked) });
  input.checked = checked;
  return h("label", { class: "switch-card" }, h("span", null, h("b", null, label), h("small", null, desc)), h("span", { class: "switch" }, input));
}

export function renderConnections(root) {
  // defToken / defHeartbeat：旧配置里的全局 onebot.access_token / heartbeat_interval，连接没单独设置时沿用
  const state = { servers: [], status: [], defToken: "", defHeartbeat: 5000, tab: "all", busy: false };
  const tabs = h("div", { class: "tabs", role: "tablist" });
  const grid = h("div", { class: "ncards" });
  const menu = h("details", { class: "menu" },
    h("summary", { class: "btn primary" }, icon("plus"), "新建"),
    h("div", { class: "menu-pop" },
      Object.entries(KINDS).map(([kind, k]) => h("button", { type: "button", onclick: () => { menu.open = false; edit(kind, -1); } },
        h("b", null, k.label), h("small", null, k.desc)))));
  const closeMenu = (e) => { if (!menu.contains(e.target)) menu.open = false; };
  document.addEventListener("click", closeMenu);
  fill(root, page("下游连接", "NoneBot、AstrBot 等框架通过这里接入 goqbot。每条连接的开关、口令、心跳单独设置，新建、修改、开关后立即生效，不用重启。",
    h("div", { class: "conn-toolbar" }, tabs, h("span", { class: "spacer" }), menu),
    grid));

  const kindOf = (item) => (item.ws ? "ws" : "ws-reverse");
  /** 连接实际使用的口令和心跳：自己写了用自己的，否则沿用全局 */
  const tokenOf = (c) => (typeof c["access-token"] === "string" ? c["access-token"] : state.defToken);
  const heartbeatOf = (c) => {
    const v = Number.isInteger(c["heartbeat-interval"]) ? c["heartbeat-interval"] : state.defHeartbeat;
    return v === 0 ? 5000 : v;
  };
  const hbText = (v) => (v < 0 ? "不发送" : `${v} 毫秒`);
  const cfgOf = (item) => item.ws ?? item["ws-reverse"];
  const statusOf = (i, kind) => state.status.find((x) => x.index === i && x.type === kind);

  async function load() {
    try {
      const r = await api("GET", "/api/connections");
      state.servers = r.servers;
      state.status = r.status;
      state.defToken = r.access_token;
      state.defHeartbeat = r.heartbeat_interval || 5000;
      render();
    } catch (e) {
      fill(grid, errorBox(e));
    }
  }

  /** 保存整份连接列表，立即生效。成功返回 true */
  async function save(servers) {
    if (state.busy) return false;
    state.busy = true;
    try {
      const r = await api("PUT", "/api/connections", { servers });
      const a = r.result.applied ?? {};
      const errs = a.errors ?? [];
      state.status = r.status;
      if (errs.length) toast("已保存，但有连接启动失败：" + errs.join("；"), "error");
      else toast("已保存，立即生效");
      await load();
      return true;
    } catch (e) {
      toast(e.message, "error");
      return false;
    } finally {
      state.busy = false;
    }
  }

  function render() {
    const count = (k) => state.servers.filter((x) => k === "all" || kindOf(x) === k).length;
    fill(tabs, [["all", "全部"], ["ws", "正向 WS"], ["ws-reverse", "反向 WS"]].map(([k, label]) =>
      h("button", { type: "button", role: "tab", class: "tab" + (state.tab === k ? " active" : ""), "aria-selected": String(state.tab === k),
        onclick: () => { state.tab = k; render(); } }, label, h("span", null, count(k)))));
    const items = state.servers.map((item, i) => ({ item, i })).filter(({ item }) => state.tab === "all" || kindOf(item) === state.tab);
    if (!items.length) {
      fill(grid, h("div", { class: "ncards-empty" },
        h("p", null, state.servers.length ? "这一类还没有连接。" : "还没有连接。"),
        h("p", { class: "muted" }, "点右上角的「新建」添加一条，框架那边填上对应的地址就能接入。")));
      return;
    }
    fill(grid, items.map(({ item, i }) => card(item, i)));
  }

  function card(item, i) {
    const kind = kindOf(item);
    const c = cfgOf(item);
    const st = statusOf(i, kind);
    const sw = h("input", { type: "checkbox", role: "switch", "aria-label": "开关这条连接", onchange: async (e) => {
      const servers = structuredClone(state.servers);
      cfgOf(servers[i]).disabled = !e.target.checked;
      if (!(await save(servers))) e.target.checked = !e.target.checked;
    } });
    sw.checked = !c.disabled;
    let tiles;
    if (kind === "ws") {
      const { host, port } = splitAddr(c.address);
      tiles = [tile("类型", KINDS.ws.label), tile("敏感能力", c["allow-sensitive-plugins"] ? "允许" : "不允许"),
        tile("主机", host || "0.0.0.0"), tile("端口", port || "—"),
        tile("口令", tokenOf(c) ? "已设置" : "未设置"), tile("心跳", hbText(heartbeatOf(c))),
        tile("框架连接地址", `ws://${host === "0.0.0.0" || host === "" || host === "::" ? "127.0.0.1" : host}:${port}`, true)];
    } else {
      tiles = [tile("类型", KINDS["ws-reverse"].label), tile("敏感能力", c["allow-sensitive-plugins"] ? "允许" : "不允许"),
        c.universal ? tile("连接地址", c.universal, true) : null,
        c.api ? tile("API 地址", c.api, true) : null,
        c.event ? tile("Event 地址", c.event, true) : null,
        tile("口令", tokenOf(c) ? "已设置" : "未设置"), tile("心跳", hbText(heartbeatOf(c))),
        tile("重连间隔", `${c["reconnect-interval"] ?? 3000} 毫秒`, true)];
    }
    return h("article", { class: "ncard" + (c.disabled ? " off" : "") },
      h("header", null,
        h("h2", null, c.name || KINDS[kind].label),
        h("label", { class: "switch" }, sw)),
      h("div", { class: "ncard-state" },
        c.disabled ? pill("muted", "已关闭") : connPill(st),
        st?.error ? h("small", { class: "form-error", title: st.error }, st.error) : null),
      h("div", { class: "tiles" }, tiles),
      h("footer", null,
        h("button", { class: "btn small", type: "button", onclick: () => edit(kind, i) }, icon("edit"), "编辑"),
        h("button", { class: "btn small danger", type: "button", onclick: () => remove(i) }, icon("trash"), "删除")));
  }

  async function remove(i) {
    const c = cfgOf(state.servers[i]);
    const name = c.name || KINDS[kindOf(state.servers[i])].label;
    if (!(await confirmDialog("删除连接", `删除「${name}」？连着的框架会立即断开。`, "删除", true))) return;
    const servers = structuredClone(state.servers);
    servers.splice(i, 1);
    save(servers);
  }

  /** 新建（i = -1）或编辑第 i 条 */
  function edit(kind, i) {
    const isNew = i < 0;
    const d = isNew
      ? (kind === "ws" ? { disabled: false, address: "127.0.0.1:6700" } : { disabled: false, universal: "ws://127.0.0.1:8080/onebot/v11/ws", "reconnect-interval": 3000 })
      : structuredClone(cfgOf(state.servers[i]));
    // 口令和心跳：新建时随机生成口令；编辑旧配置时把沿用的全局值填进来，保存后就是这条连接自己的
    d["access-token"] = isNew ? randomToken() : tokenOf(d);
    d["heartbeat-interval"] = isNew ? 5000 : heartbeatOf(d);
    const warn = h("p", { class: "warn full" });
    let isLocal = () => true;
    const updateWarn = () => {
      const noToken = !String(d["access-token"] ?? "").trim();
      warn.textContent = noToken && d["allow-sensitive-plugins"] ? "没有设置 access_token 时，敏感能力不会开放给这条连接。"
        : noToken && !isLocal() ? "没有设置 access_token：其他机器也能连上并控制机器人，建议设置口令。" : "";
    };
    const tokenInput = h("input", { type: "password", value: d["access-token"], autocomplete: "off", spellcheck: "false", placeholder: "不填则不校验",
      oninput: (e) => { d["access-token"] = e.target.value; updateWarn(); } });
    const reveal = h("button", { class: "btn ghost small", type: "button", onclick: () => {
      tokenInput.type = tokenInput.type === "password" ? "text" : "password";
      reveal.textContent = tokenInput.type === "password" ? "显示" : "隐藏";
    } }, "显示");
    const tokenField = h("label", { class: "field full" }, h("span", null, "access_token"),
      h("span", { class: "inline" }, tokenInput, reveal,
        h("button", { class: "btn ghost small", type: "button", onclick: () => {
          d["access-token"] = randomToken();
          tokenInput.value = d["access-token"];
          tokenInput.type = "text";
          reveal.textContent = "隐藏";
          updateWarn();
        } }, "随机生成")),
      h("small", null, "框架那边要填同样的口令"));
    const heartbeatField = h("label", { class: "field" }, h("span", null, "心跳间隔（毫秒）"),
      h("input", { type: "number", step: 1000, value: d["heartbeat-interval"], oninput: (e) => { d["heartbeat-interval"] = Number(e.target.value); } }),
      h("small", null, "负数 = 不发心跳"));
    const err = h("p", { class: "form-error", role: "alert" });
    const input = (key, attrs = {}) => h("input", { type: "text", spellcheck: "false", value: d[key] ?? "", ...attrs, oninput: (e) => { d[key] = e.target.value; } });
    const field = (label, el, hint, full = true) => h("label", { class: "field" + (full ? " full" : "") }, h("span", null, label), el, hint ? h("small", null, hint) : null);

    let body;
    if (kind === "ws") {
      const a = splitAddr(d.address);
      const preview = h("small", null);
      isLocal = () => ["127.0.0.1", "localhost", "::1"].includes(a.host.trim() || "0.0.0.0");
      const update = () => {
        const host = a.host.trim() || "0.0.0.0";
        preview.textContent = `框架填的地址：ws://${host === "0.0.0.0" || host === "::" ? "127.0.0.1" : host}:${a.port || "端口"}`;
        updateWarn();
      };
      body = [
        field("主机", h("input", { type: "text", spellcheck: "false", value: a.host, placeholder: "127.0.0.1", oninput: (e) => { a.host = e.target.value; update(); } }),
          "127.0.0.1 = 只允许本机连接；0.0.0.0 = 允许其他机器连接", false),
        field("端口", h("input", { type: "number", min: 1, max: 65535, value: a.port, placeholder: "6700", oninput: (e) => { a.port = e.target.value; update(); } }), null, false),
        h("div", { class: "full preview" }, preview),
        tokenField,
        heartbeatField,
      ];
      d.__addr = a;
      update();
    } else {
      body = [
        field("连接地址", input("universal", { placeholder: "ws://127.0.0.1:8080/onebot/v11/ws" }), "框架开的 OneBot V11 反向 WebSocket 地址。NoneBot 默认 ws://127.0.0.1:8080/onebot/v11/ws"),
        tokenField,
        heartbeatField,
        field("重连间隔（毫秒）", h("input", { type: "number", min: 100, step: 100, value: d["reconnect-interval"] ?? 3000, oninput: (e) => { d["reconnect-interval"] = Number(e.target.value); } }), "断线后等多久再连", false),
        h("details", { class: "full more" + (d.api || d.event ? "" : ""), open: !!(d.api || d.event) },
          h("summary", null, "分开填写 API / Event 地址（一般不用）"),
          h("div", { class: "form-grid" },
            field("API 地址", input("api", { placeholder: "可不填" }), null, false),
            field("Event 地址", input("event", { placeholder: "可不填" }), null, false))),
      ];
    }

    const form = h("form", { class: "conn-edit", onsubmit: async (e) => {
      e.preventDefault();
      err.textContent = "";
      const out = { ...d };
      delete out.__addr;
      out.name = String(out.name ?? "").trim();
      out["access-token"] = String(out["access-token"] ?? "").trim();
      if (!Number.isInteger(out["heartbeat-interval"])) { err.textContent = "心跳间隔要填整数"; return; }
      if (kind === "ws") {
        const host = d.__addr.host.trim() || "0.0.0.0";
        const port = Number(d.__addr.port);
        if (!Number.isInteger(port) || port < 1 || port > 65535) { err.textContent = "端口要填 1 到 65535 之间的数字"; return; }
        out.address = joinAddr(host, port);
      } else {
        for (const k of ["universal", "api", "event"]) out[k] = String(out[k] ?? "").trim();
        if (!out.universal && !out.api && !out.event) { err.textContent = "请填写连接地址"; return; }
        if ([out.universal, out.api, out.event].some((u) => u && !/^wss?:\/\//.test(u))) { err.textContent = "地址要以 ws:// 或 wss:// 开头"; return; }
      }
      const servers = structuredClone(state.servers);
      const item = { [kind]: out };
      if (isNew) servers.push(item); else servers[i] = item;
      if (await save(servers)) close();
    } },
      h("div", { class: "form-grid" },
        switchCard("启用", "保存后立即开始工作", !d.disabled, (v) => { d.disabled = !v; }),
        switchCard("允许敏感能力", "可以调用插件的敏感能力，如获取 Cookie（需要设置口令）", !!d["allow-sensitive-plugins"], (v) => { d["allow-sensitive-plugins"] = v; updateWarn(); }),
        field("名称", input("name", { placeholder: "例如 NoneBot，只用于显示", maxlength: 40 })),
        body,
        warn),
      err,
      h("div", { class: "actions" },
        h("button", { class: "btn", type: "button", onclick: () => close() }, "关闭"),
        h("button", { class: "btn primary", type: "submit" }, "保存")));
    updateWarn();
    const close = openDialog((isNew ? "新建" : "编辑") + KINDS[kind].label, form, { mid: true });
  }

  load();
  const stop = every(3000, () => { if (!$("#dialog").open && !state.busy) load(); });
  return () => { stop(); document.removeEventListener("click", closeMenu); };
}

// ================= 插件 =================

const STATE_TEXT = {
  ready: ["ok", "运行中"], starting: ["wait", "启动中"], pending: ["wait", "等待启动"], restarting: ["wait", "重启中"],
  error: ["bad", "出错"], closing: ["muted", "正在关闭"], closed: ["muted", "已停止"], disabled: ["muted", "未开启"],
};

export function renderPlugins(root, selfName) {
  const list = h("div", { class: "plugin-list" }, h("p", { class: "muted" }, "正在读取…"));
  fill(root, page("插件", "放进 plugins 文件夹的插件。开关插件要重启 goqbot 后生效。", list));
  const load = async () => {
    try {
      const { plugins } = await api("GET", "/api/plugins");
      if (!plugins.length) {
        fill(list, h("p", { class: "panel muted" }, "plugins 文件夹里还没有插件。"));
        return;
      }
      fill(list, ...plugins.map((p) => {
        const [kind, label] = STATE_TEXT[p.state] ?? ["muted", p.state];
        const isSelf = p.name === selfName;
        const enabled = p.state !== "disabled";
        const caps = (p.capabilities ?? []).length;
        return h("article", { class: "plugin" },
          h("div", { class: "plugin-top" },
          h("div", { class: "plugin-main" },
            h("h2", null, p.name, p.version ? h("small", null, "v" + p.version) : null),
            h("p", null,
              pill(kind, label),
              h("span", { class: "tag" }, p.kind === "js" ? "JS / TS" : "可执行文件"),
              caps ? h("span", { class: "muted" }, `提供 ${caps} 个能力`) : null),
            p.reason ? h("p", { class: "form-error" }, `最近一次失败：${p.reason}（详情点「日志」）`) : null,
            p.restarts ? h("p", { class: "muted" }, `已自动重启 ${p.restarts} 次`) : null),
          h("div", { class: "plugin-actions" },
            h("button", { class: "btn small", type: "button", onclick: () => showLogs(p.name) }, "日志"),
            isSelf ? null : h("button", { class: "btn small", type: "button", disabled: !enabled, onclick: () => restart(p.name) }, "重启"),
            isSelf ? h("span", { class: "muted small" }, "当前页面") : h("button", { class: "btn small", type: "button", onclick: () => toggle(p.name, enabled) }, enabled ? "关闭" : "开启"))),
          permBlock(p, isSelf));
      }));
    } catch (e) {
      fill(list, errorBox(e));
    }
  };
  async function restart(name) {
    try { await api("POST", "/api/plugins/restart", { plugin: name }); toast(`已重启 ${name}`); load(); }
    catch (e) { toast(e.message, "error"); }
  }
  async function toggle(name, enabled) {
    try {
      await api("POST", "/api/plugins/enabled", { plugin: name, enabled: !enabled });
      toast(enabled ? `已关闭 ${name}，重启 goqbot 后生效` : `已开启 ${name}，重启 goqbot 后生效`);
      load();
    } catch (e) { toast(e.message, "error"); }
  }
  load();
  return every(5000, load);
}

/** 从插件页跳到配置页时，要定位到的插件权限卡片 */
let grantsFocus = "";

/** 一项能力的标签：中文说明，鼠标停留显示能力名 */
function capChip(label, name, { sensitive = false, off = false } = {}) {
  return h("span", { class: "chip" + (sensitive ? " sensitive" : "") + (off ? " off" : ""), title: name },
    label, sensitive ? h("b", null, "敏感") : null);
}

/** 插件页里的权限区：能做什么、被收回了什么、权限范围、对外提供什么 */
function permBlock(p, isSelf) {
  const perms = p.permissions ?? [];
  const caps = p.capabilities ?? [];
  const on = perms.filter((x) => x.granted);
  const off = perms.filter((x) => !x.granted);
  const sensitiveOn = on.filter((x) => x.sensitive).length;
  const row = (label, ...content) => h("div", { class: "perm-row" }, h("span", { class: "perm-label" }, label), h("div", { class: "perm-value" }, content));
  if (!perms.length && !caps.length) {
    return h("div", { class: "perm" },
      row("能做什么", h("span", { class: "muted" }, p.state === "ready" ? "这个插件没有申请任何权限，只能处理别人发给它的调用" : "插件运行后才能看到它申请的权限")));
  }
  return h("div", { class: "perm" },
    row("能做什么",
      on.length ? on.map((x) => capChip(capLabel(x), x.name, { sensitive: x.sensitive })) : h("span", { class: "muted" }, "所有权限都被收回了，什么都做不了")),
    off.length ? row("已收回", off.map((x) => capChip(capLabel(x), x.name, { sensitive: x.sensitive, off: true }))) : null,
    row("权限范围",
      h("span", null, p.grants ? "已收紧：只允许「能做什么」里列出的能力，其他一律拒绝" : "按插件声明：插件申请的能力全部允许",
        h("span", { class: "muted" }, `；共 ${on.length} 项${sensitiveOn ? `，其中 ${sensitiveOn} 项是敏感能力` : ""}`))),
    caps.length ? row("对外提供", caps.map((c) => capChip(c.description || c.name, c.name, { sensitive: c.sensitive }))) : null,
    perms.length ? h("div", { class: "perm-foot" },
      isSelf ? h("small", { class: "muted" }, "这是当前页面所在的插件，不能在这里修改自己的权限")
        : h("button", { class: "btn small", type: "button", onclick: () => { grantsFocus = p.name; location.hash = "#/config"; } }, icon("edit"), "修改权限")) : null);
}

async function showLogs(name) {
  const pre = h("pre", { class: "logbox" }, "正在读取…");
  openDialog(`${name} 的日志`, pre, { wide: true });
  try {
    const r = await api("GET", `/api/plugins/logs?plugin=${encodeURIComponent(name)}&lines=300`);
    const lines = r.lines ?? [];
    if (r.stderr?.length) lines.push("", "---------- stderr ----------", ...r.stderr);
    if (!lines.length) lines.push("今天还没有这个插件的日志");
    fillLog(pre, lines, "");
    pre.scrollTop = pre.scrollHeight;
  } catch (e) {
    pre.textContent = e.message;
  }
}

// ================= 日志 =================

const LOG_LINE = /^\[(Info|Warning|Error|Debug)\]\s+(?:\d{4}-\d{2}-\d{2}\s+)?(\d{2}:\d{2}:\d{2})\s?(.*)$/;
const LEVEL_TEXT = { Info: "信息", Warning: "警告", Error: "错误", Debug: "调试" };

/** 把日志行分成条目：带 [级别] 开头的行开始一条，后面不带前缀的行（如消息边框）属于同一条 */
function logEntries(lines) {
  const out = [];
  for (const l of lines) {
    const m = LOG_LINE.exec(l);
    if (m) out.push({ level: m[1], time: m[2], lines: [m[3]] });
    else if (out.length) out.at(-1).lines.push(l);
    else out.push({ level: "", time: "", lines: [l] });
  }
  return out;
}

/** 插件日志弹窗等简单场景用：整段显示，错误和警告上色 */
function fillLog(pre, lines, filter) {
  const f = filter.toLowerCase();
  const shown = f ? lines.filter((l) => l.toLowerCase().includes(f)) : lines;
  fill(pre, (shown.length ? shown : ["（没有日志）"]).map((l) => {
    const cls = /^\[Error\]/.test(l) ? "l-err" : /^\[Warning\]/.test(l) ? "l-warn" : "";
    return h("span", { class: "line " + cls }, l + "\n");
  }));
}

export function renderLogs(root) {
  const state = { files: [], date: "", today: "", count: 500, filter: "", level: "all", auto: true, lines: [] };
  const fileSel = h("select", { "aria-label": "日志文件", onchange: (e) => { state.date = e.target.value; load(true); } });
  const levels = h("div", { class: "tabs", role: "tablist" });
  const search = h("input", { type: "search", placeholder: "搜索日志", oninput: (e) => { state.filter = e.target.value.trim(); draw(); } });
  const countSel = h("select", { "aria-label": "显示行数", onchange: (e) => { state.count = +e.target.value; load(); } },
    [200, 500, 1000, 2000].map((n) => h("option", { value: n, selected: n === state.count }, `最后 ${n} 行`)));
  const autoInput = h("input", { type: "checkbox", role: "switch", onchange: (e) => { state.auto = e.target.checked; } });
  autoInput.checked = true;
  const autoLabel = h("label", { class: "switch log-auto" }, autoInput, h("span", null, "自动刷新"));
  const meta = h("p", { class: "log-meta" });
  const view = h("div", { class: "logview", role: "log" }, h("p", { class: "muted" }, "正在读取…"));

  fill(root, page("日志", null,
    h("div", { class: "log-toolbar" },
      h("label", { class: "log-file" }, h("span", null, "日志文件"), fileSel),
      h("span", { class: "spacer" }),
      countSel, autoLabel,
      h("button", { class: "btn", type: "button", onclick: () => load() }, "刷新")),
    h("div", { class: "log-toolbar" },
      levels,
      h("label", { class: "search log-search" }, icon("search"), search)),
    meta, view));

  function drawLevels() {
    fill(levels, [["all", "全部"], ["Warning", "警告"], ["Error", "错误"]].map(([k, label]) =>
      h("button", { type: "button", role: "tab", class: "tab" + (state.level === k ? " active" : ""), "aria-selected": String(state.level === k),
        onclick: () => { state.level = k; drawLevels(); draw(); } }, label)));
  }

  function drawFiles() {
    const files = state.files.some((f) => f.date === state.date) ? state.files : [{ date: state.date, size: 0, missing: true }, ...state.files];
    fill(fileSel, files.map((f) => h("option", { value: f.date, selected: f.date === state.date },
      `${f.date}${f.date === state.today ? "（今天）" : ""}　${f.missing ? "没有日志" : bytes(f.size)}`)));
    const isToday = state.date === state.today;
    autoInput.disabled = !isToday;
    autoLabel.title = isToday ? "每 3 秒刷新一次" : "只有今天的日志会自动刷新";
  }

  function draw() {
    const atBottom = view.scrollHeight - view.scrollTop - view.clientHeight < 40;
    const f = state.filter.toLowerCase();
    let entries = logEntries(state.lines);
    const total = entries.length;
    if (state.level !== "all") entries = entries.filter((e) => e.level === state.level || (state.level === "Warning" && e.level === "Error"));
    if (f) entries = entries.filter((e) => e.lines.some((l) => l.toLowerCase().includes(f)) || e.time.includes(f));
    const file = `goqbot-${state.date}.log`;
    meta.textContent = state.lines.length === 0 ? `${file} 还没有内容`
      : entries.length === total ? `${file}，显示最后 ${state.lines.length} 行`
      : `${file}，显示最后 ${state.lines.length} 行，筛选出 ${entries.length} 条`;
    if (!entries.length) {
      fill(view, h("p", { class: "log-empty" }, state.lines.length ? "没有符合条件的日志" : "这一天还没有日志"));
      return;
    }
    fill(view, entries.map((e) => h("div", { class: "log-entry lv-" + (e.level || "none") },
      h("span", { class: "log-time" }, e.time),
      e.level ? h("span", { class: "log-level" }, LEVEL_TEXT[e.level] ?? e.level) : h("span", { class: "log-level" }),
      h("span", { class: "log-msg" }, e.lines.join("\n")))));
    if (atBottom) view.scrollTop = view.scrollHeight;
  }

  async function load(scrollEnd = false) {
    try {
      const q = `lines=${state.count}` + (state.date ? `&date=${state.date}` : "");
      const r = await api("GET", `/api/logs?${q}`);
      if (!state.today) state.today = r.date; // 第一次不带日期，返回的就是 goqbot 那边的今天
      state.date = r.date;
      state.files = r.files ?? [];
      state.lines = r.lines ?? [];
      drawFiles();
      draw();
      if (scrollEnd) view.scrollTop = view.scrollHeight;
    } catch (e) {
      fill(view, errorBox(e));
    }
  }

  drawLevels();
  load(true);
  return every(3000, () => { if (state.auto && state.date === state.today) load(); });
}

// ================= 配置（图形化） =================
// 只把改过的项写进 config.yml（保留注释）；WebUI 自己的设置和内置指令存在插件数据目录，保存后立即生效

const INTENTS = [
  ["ReadyHandler", "连接就绪", "必须保留", true],
  ["ErrorNotifyHandler", "连接错误", "必须保留", true],
  ["GroupATMessageEventHandler", "群里 @ 机器人的消息", ""],
  ["GroupMessageEventHandler", "群里的全部消息", "需要群主授权"],
  ["C2CMessageEventHandler", "单聊消息", ""],
  ["InteractionHandler", "按钮点击", "用按钮时必须打开"],
  ["C2CFriendEventHandler", "加好友、删好友", ""],
  ["GroupAddRobotEventHandler", "机器人被拉进群", ""],
  ["GroupDelRobotEventHandler", "机器人被移出群", ""],
  ["GroupMemberEventHandler", "群成员进群、退群", "需在开放平台开启群成员事件"],
  ["GroupJoinRequestEventHandler", "用户申请加群", "需机器人是群管理员"],
  ["GroupMsgRejectEventHandler", "群关闭了机器人主动发言", ""],
  ["GroupMsgReceiveEventHandler", "群开启了机器人主动发言", ""],
  ["C2CMsgRejectEventHandler", "用户关闭了主动消息推送", ""],
  ["C2CMsgReceiveEventHandler", "用户开启了主动消息推送", ""],
];

/** 能力的中文说明（插件权限里显示） */
const CAP_LABELS = {
  "events.subscribe": "接收消息和事件", "events.unsubscribe": "取消接收事件", "events.emit": "向框架发送自定义事件",
  "host.info": "查看运行状态", "host.logs": "读取 goqbot 日志", "host.config.get": "读取配置文件（含密钥）",
  "host.config.set": "整份覆盖配置文件", "host.config.update": "修改配置", "host.restart": "重启 goqbot", "host.update": "更新 goqbot 程序",
  "plugins.list": "查看插件列表", "plugins.set_enabled": "开关其他插件", "plugins.set_grants": "修改其他插件的权限",
  "plugins.restart": "重启其他插件", "plugins.logs": "读取插件日志",
  send_group_msg: "发群消息", send_private_msg: "发私聊消息", send_msg: "发消息", delete_msg: "撤回消息",
};
function capLabel(perm) {
  if (perm.wildcard) return perm.name === "*" ? "全部普通能力" : `名称符合 ${perm.name} 的普通能力`;
  return CAP_LABELS[perm.name] ?? (perm.name.includes(".") ? "goqbot 能力" : "OneBot 动作或插件能力");
}

const RESTART_LABELS = {
  qq: "QQ 账号与事件", "onebot.message_format": "消息格式", "onebot.debug": "OneBot 调试", "onebot.downtime_message": "掉线自动回复",
  idmap: "ID 对照表", log: "日志", plugins: "插件", webui: "WebUI 的开关、地址和端口",
};

/** 字节数换成 MB 文字 */
function mb(n) {
  if (typeof n !== "number") return "—";
  if (n < 1048576) return `${Math.max(1, Math.round(n / 1024))} KB`;
  return `${(n / 1048576).toFixed(n < 10 * 1048576 ? 1 : 0)} MB`;
}

/** ID 对照表现在的大小 */
function idmapUsage(st) {
  if (!st) return "";
  if (!st.persistent) return "现在是纯内存模式，没有文件。";
  return `现在文件 ${mb(st.file_bytes)}，存着 ${Number(st.messages ?? 0).toLocaleString("zh-CN")} 条消息记录、${Number(st.ids ?? 0).toLocaleString("zh-CN")} 个用户和群的 ID。`;
}

export function renderConfig(root) {
  const box = h("div", { class: "settings" }, h("p", { class: "muted" }, "正在读取…"));
  const bar = h("div", { class: "save-bar", hidden: true });
  const result = h("div", { class: "save-result", "aria-live": "polite" });
  fill(root, page("配置", "修改 goqbot 和 WebUI 的设置。下游连接在「连接」页修改。", result, box, h("div", { id: "grants-slot" }), bar));

  let orig, form, origWeb, web;
  const get = (obj, path) => path.split(".").reduce((o, k) => (o == null ? undefined : o[k]), obj);
  const set = (obj, path, v) => {
    const keys = path.split(".");
    let o = obj;
    for (const k of keys.slice(0, -1)) o = o[k] ??= {};
    o[keys.at(-1)] = v;
  };

  async function load() {
    try {
      const r = await api("GET", "/api/settings");
      orig = r.goqbot;
      // 配置文件里没写的项按 goqbot 的默认值显示
      for (const [path, def] of IDMAP_DEFAULTS) if (get(orig, path) === undefined) set(orig, path, def);
      form = structuredClone(orig);
      origWeb = { host: r.webui.host, port: r.webui.port, token: "", commands: r.commands, history: r.history };
      web = structuredClone(origWeb);
      render(r);
    } catch (e) {
      fill(box, errorBox(e));
    }
  }

  // ---------- 改动检测 ----------
  const FIELDS = ["qq.app_id", "qq.client_secret", "qq.token", "qq.uin", "qq.use_uin", "qq.sandbox", "qq.shard_count", "qq.shard_id",
    "qq.mention_mode", "qq.ip_reject_action", "qq.ip_reject_retry_interval", "qq.developer_openid", "qq.text_intent",
    "onebot.message_format", "onebot.debug", "onebot.downtime_message", "idmap.disabled", "idmap.path", "idmap.keep_days", "idmap.max_size_mb",
    "log.enabled", "log.path", "log.debug", "plugins.path", "plugins.node"];
  const IDMAP_DEFAULTS = [["idmap.keep_days", 3], ["idmap.max_size_mb", 1024]];
  function goqbotChanges() {
    const out = {};
    for (const f of FIELDS) {
      let a = get(orig, f);
      const b = get(form, f);
      // 配置文件里没写的项按默认值比较，避免来回切换后显示成「有修改」
      if (a === undefined) a = typeof b === "boolean" ? f === "log.enabled" : typeof b === "string" ? "" : typeof b === "number" ? 0 : a;
      if (JSON.stringify(a ?? null) !== JSON.stringify(b ?? null) && b !== undefined) out[f] = b;
    }
    return out;
  }
  function webChanges() {
    const out = {};
    if (web.host !== origWeb.host || Number(web.port) !== origWeb.port) { out.host = web.host; out.port = Number(web.port); }
    if (web.token.trim()) out.token = web.token.trim();
    if (JSON.stringify(web.commands) !== JSON.stringify(origWeb.commands)) out.commands = web.commands;
    if (JSON.stringify(web.history) !== JSON.stringify(origWeb.history)) out.history = web.history;
    return out;
  }
  function changed() {
    const n = Object.keys(goqbotChanges()).length + Object.keys(webChanges()).length;
    bar.hidden = n === 0;
    if (n) fill(bar, h("span", null, `有 ${n} 项修改还没保存`), h("span", { class: "spacer" }),
      h("button", { class: "btn", type: "button", onclick: () => load() }, "撤销"),
      h("button", { class: "btn primary", type: "button", onclick: save }, "保存"));
  }

  // ---------- 表单控件 ----------
  const text = (path, attrs = {}) => {
    const el = h("input", { type: attrs.type ?? "text", spellcheck: "false", value: get(form, path) ?? "", ...attrs,
      oninput: (e) => { set(form, path, e.target.value); changed(); } });
    return el;
  };
  const num = (path, attrs = {}) => h("input", { type: "number", value: get(form, path) ?? 0, ...attrs,
    oninput: (e) => { const v = Number(e.target.value); if (Number.isInteger(v)) { set(form, path, v); changed(); } } });
  const select = (path, options, def) => h("select", { onchange: (e) => { set(form, path, e.target.value); changed(); } },
    options.map(([v, label]) => h("option", { value: v, selected: (get(form, path) || def) === v }, label)));
  const swc = (label, desc, path, { invert = false, def = false } = {}) => {
    const cur = get(form, path);
    return switchCard(label, desc, invert ? !(cur ?? def) : (cur ?? def), (v) => { set(form, path, invert ? !v : v); changed(); });
  };
  const field = (label, el, hint, full = false) => h("label", { class: "field" + (full ? " full" : "") }, h("span", null, label), el, hint ? h("small", null, hint) : null);
  const secret = (path, placeholder) => {
    const input = text(path, { type: "password", autocomplete: "off", placeholder });
    const btn = h("button", { class: "btn ghost small", type: "button", onclick: () => {
      input.type = input.type === "password" ? "text" : "password";
      btn.textContent = input.type === "password" ? "显示" : "隐藏";
    } }, "显示");
    return h("span", { class: "inline" }, input, btn);
  };
  const section = (title, desc, ...children) => h("section", { class: "panel" }, h("div", null, h("h2", null, title), desc ? h("p", { class: "muted small" }, desc) : null), ...children);

  /** ID 模式：机器人 ID（self_id）用 AppID 还是 QQ 号。用 QQ 号时要填 QQ 号 */
  function idMode() {
    const uinErr = h("small", { class: "form-error" });
    const uinInput = h("input", { type: "text", inputmode: "numeric", autocomplete: "off", spellcheck: "false", maxlength: 12,
      value: form.qq.uin ? String(form.qq.uin) : "", placeholder: "例如 3889000000",
      oninput: (e) => {
        const v = e.target.value.trim();
        if (/^\d{5,12}$/.test(v)) {
          uinErr.textContent = "";
          form.qq.uin = Number(v);
        } else {
          uinErr.textContent = v ? "QQ 号只能是 5 到 12 位数字" : "";
          form.qq.uin = 0;
        }
        changed();
      } });
    const uinField = h("label", { class: "field" }, h("span", null, "机器人 QQ 号"), uinInput, uinErr,
      h("small", null, "上报给框架的 self_id 会用这个号码"));
    const desc = h("small", null);
    const sync = () => {
      const on = !!form.qq.use_uin;
      uinField.classList.toggle("placeholder", !on); // 关闭时留空位，后面各行位置不变
      uinInput.disabled = !on;
      desc.textContent = on ? "当前：用 QQ 号作为机器人 ID" : "当前：用 AppID 作为机器人 ID";
    };
    const input = h("input", { type: "checkbox", role: "switch", onchange: (e) => { form.qq.use_uin = e.target.checked; sync(); changed(); } });
    input.checked = !!form.qq.use_uin;
    const card = h("label", { class: "switch-card" }, h("span", null, h("b", null, "ID 模式：使用 QQ 号"), desc), h("span", { class: "switch" }, input));
    sync();
    return [card, uinField];
  }

  function intents() {
    const list = Array.isArray(form.qq.text_intent) ? form.qq.text_intent : [];
    const known = new Set(INTENTS.map((x) => x[0]));
    const toggle = (name, on) => {
      const cur = new Set(form.qq.text_intent ?? []);
      on ? cur.add(name) : cur.delete(name);
      // 按固定顺序保存，不认识的项保留在最后
      form.qq.text_intent = [...INTENTS.map((x) => x[0]).filter((n) => cur.has(n)), ...[...cur].filter((n) => !known.has(n))];
      changed();
    };
    return h("div", { class: "intents" }, INTENTS.map(([name, label, note, required]) => {
      const input = h("input", { type: "checkbox", disabled: !!required, onchange: (e) => toggle(name, e.target.checked) });
      input.checked = required || list.includes(name);
      return h("label", { class: "intent" + (required ? " required" : "") }, input,
        h("span", null, h("b", null, label), h("small", null, note ? `${note}，${name}` : name)));
    }));
  }

  function render(r) {
    const preview = h("div", { class: "bubble cmd-preview" }, r.preview);
    const cmd = web.commands.status;
    const keyword = h("input", { type: "text", value: cmd.keyword, maxlength: 20, spellcheck: "false",
      oninput: (e) => { cmd.keyword = e.target.value.trim() || "#gq"; changed(); } });
    const tokenInput = h("input", { type: "password", autocomplete: "new-password", placeholder: "不修改请留空", spellcheck: "false",
      oninput: (e) => { web.token = e.target.value; changed(); } });

    const [idCard, uinField] = idMode();
    fill(box,
      section("QQ 机器人", "在 QQ 开放平台的机器人管理页可以找到 AppID 和 AppSecret。",
        h("div", { class: "form-grid" },
          field("AppID", text("qq.app_id")),
          field("AppSecret", secret("qq.client_secret", "")),
          field("机器人令牌（可不填）", secret("qq.token", "")),
          uinField,
          swc("沙箱环境", "连接 QQ 的沙箱环境，测试用", "qq.sandbox"),
          idCard,
          field("@机器人的上报方式", select("qq.mention_mode", [["strip", "去掉 @（默认）"], ["at", "转成 at 段（多框架推荐）"], ["raw", "保留原文"]], "strip")),
          field("IP 不在白名单时", select("qq.ip_reject_action", [["stop", "停止连接"], ["retry", "定时重试"], ["exit", "退出程序"]], "stop")),
          field("重试间隔（秒）", num("qq.ip_reject_retry_interval", { min: 5 }), "选「定时重试」时生效"),
          field("开发者的 OpenID（可不填）", text("qq.developer_openid"), "机器人开发者（机器人账号的管理员）自己的 OpenID，点自己的头像在用户资料里能看到。开发者是群主或管理员的群，每条消息可被动回复 50 次（否则 5 次）"),
          h("details", { class: "full more" },
            h("summary", null, "分片（一般不用改）"),
            h("div", { class: "form-grid" },
              field("分片总数", num("qq.shard_count", { min: 1 })),
              field("本进程的分片号", num("qq.shard_id", { min: 0 }), "从 0 开始")))),
      ),
      section("接收的事件", "勾选要接收的 QQ 事件，没勾选的不会推给框架。", intents()),
      section("消息上报", null,
        h("div", { class: "form-grid" },
          field("消息格式", select("onebot.message_format", [["array", "消息段数组（推荐）"], ["string", "额外附带 CQ 码"]], "array")),
          swc("OneBot 调试", "输出框架发来的动作请求、参数、耗时和连接重试过程，排查连接配置问题时打开", "onebot.debug"),
          field("掉线自动回复", text("onebot.downtime_message", { placeholder: "空 = 不回复" }), "所有框架都掉线时，自动回复给发消息的人", true))),
      section("数据与日志", null,
        h("div", { class: "form-grid" },
          swc("保存 ID 对照表", "关闭后只存在内存里，重启后无法撤回旧消息、查回用户", "idmap.disabled", { invert: true }),
          field("ID 对照表文件", text("idmap.path", { placeholder: "idmap.db" })),
          field("消息记录保留天数", num("idmap.keep_days", { min: 0, max: 36500 }),
            "撤回消息、引用旧消息时要用到；0 = 不按天数删除。重启 goqbot 后生效"),
          field("ID 对照表大小上限（MB）", num("idmap.max_size_mb", { min: 0 }),
            `0 = 不限制，最小 16；超出时从最早的消息记录删起，用户的 ID 对照不会删。重启 goqbot 后生效。${idmapUsage(r.usage?.idmap)}`),
          swc("写日志文件", "按天写入日志目录；关闭后只在终端显示", "log.enabled", { def: true }),
          swc("框架调试模式", "输出全部调试信息：启动详情、QQ 连接细节、原始报文、心跳等，日志会很多，排查完记得关掉", "log.debug"),
          field("日志目录", text("log.path", { placeholder: "logs" })))),
      section("插件", "一般不用改。",
        h("div", { class: "form-grid" },
          field("插件目录", text("plugins.path", { placeholder: "plugins" })),
          field("Node 程序", text("plugins.node", { placeholder: "留空 = 自动查找" }), "运行 JS / TS 插件用的 Node"))),
      section("内置指令", "WebUI 自带的指令，在群里或私聊发送即可触发，保存后立即生效。",
        h("div", { class: "form-grid" },
          switchCard("查看运行状态", "回复 goqbot 的运行平台和运行时长", cmd.enabled, (v) => { cmd.enabled = v; changed(); }),
          field("触发指令", keyword, "完整发送这几个字才会触发，不区分大小写"),
          h("div", { class: "full cmd-demo" }, h("small", { class: "muted" }, "回复示例"), preview))),
      section("聊天记录", "群聊、私聊页面保存的聊天记录和图片，保存后立即生效。",
        h("div", { class: "form-grid" },
          field("保留天数", histNum("keep_days", 36500), "超过天数的聊天记录和缓存的图片会删掉；0 = 一直保留"),
          field("聊天记录大小上限（MB）", histNum("max_mb", 1048576), `0 = 不限制；超出时从最早的一天删起。现在占用 ${mb(r.usage?.chat_bytes)}`),
          field("图片缓存大小上限（MB）", histNum("media_max_mb", 1048576), `0 = 不限制；超出时先删最早下载的。现在占用 ${mb(r.usage?.media_bytes)}`))),
      section("WebUI", "这个管理页面本身的设置。要关闭 WebUI，把 config.yml 里 webui 的 enabled 改成 false 后重启 goqbot。",
        h("div", { class: "form-grid" },
          field("监听地址", h("input", { type: "text", value: web.host, spellcheck: "false", oninput: (e) => { web.host = e.target.value.trim(); changed(); } }),
            "127.0.0.1 = 只能在本机打开；0.0.0.0 = 其他设备也能打开"),
          field("端口", h("input", { type: "number", min: 1, max: 65535, value: web.port, oninput: (e) => { web.port = e.target.value; changed(); } }), "地址和端口保存在 config.yml 的 webui 里，重启 goqbot 后生效"),
          field("新的登录口令", tokenInput, "至少 8 位，包含大写字母、小写字母和特殊字符（如 !@#$%）。保存后立即生效，其他已登录的页面需要重新登录", true))),
      h("p", { class: "muted small" }, `配置文件：${r.path}`));
    changed();
  }

  /** 聊天记录保留设置的数字输入框 */
  function histNum(key, max) {
    return h("input", { type: "number", min: 0, max, value: web.history?.[key] ?? 0,
      oninput: (e) => { const v = Number(e.target.value); if (Number.isInteger(v) && v >= 0) { web.history = { ...web.history, [key]: v }; changed(); } } });
  }

  async function save() {
    if (form.qq.use_uin && !(form.qq.uin > 0)) {
      fill(result, h("p", { class: "form-error" }, "ID 模式选了「使用 QQ 号」，请先填写正确的机器人 QQ 号"));
      return;
    }
    const maxMB = form.idmap?.max_size_mb;
    if (maxMB !== 0 && maxMB < 16) {
      fill(result, h("p", { class: "form-error" }, "ID 对照表大小上限要填 0（不限制）或至少 16 MB"));
      return;
    }
    const gv = goqbotChanges();
    const wv = webChanges();
    if (wv.token && tokenProblem(wv.token)) {
      fill(result, h("p", { class: "form-error" }, "新的登录口令" + tokenProblem(wv.token).replace(/^口令/, "")));
      return;
    }
    fill(result);
    const notes = [];
    try {
      if (Object.keys(wv).length) {
        const r = await api("PUT", "/api/settings/webui", wv);
        if (wv.token) auth.set(wv.token);
        if (r.restart_needed) notes.push("WebUI 的监听地址和端口");
      }
      if (Object.keys(gv).length) {
        const r = await api("PUT", "/api/settings/goqbot", { values: gv });
        for (const k of r.restart_required ?? []) notes.push(RESTART_LABELS[k] ?? k);
      }
    } catch (e) {
      fill(result, errorBox(e));
      return;
    }
    toast("已保存");
    if (notes.length) {
      fill(result, h("div", { class: "restart-note" },
        h("p", null, `已保存。这些改动要重启 goqbot 才生效：${[...new Set(notes)].join("、")}`),
        h("button", { class: "btn primary small", type: "button", onclick: () => restartFlow() }, "立即重启")));
    }
    await load();
  }

  // ---------- 插件权限：开关后立即生效，列表自动刷新 ----------
  const grantsSlot = root.querySelector("#grants-slot");
  const grantsBox = h("div", { class: "grants" }, h("p", { class: "muted" }, "正在读取…"));
  const grantsSection = section("插件权限", "每个插件能调用哪些能力、对外提供哪些能力。调用权限可以在这里收回或恢复，开关后立即生效，不用重启；标「敏感」的能力能读到密钥、凭证或改动 goqbot 本身。", grantsBox);
  let selfName = "";
  let grantsBusy = false;

  async function loadGrants() {
    try {
      const [{ plugins }, info] = await Promise.all([api("GET", "/api/plugins"), selfName ? null : api("GET", "/api/info")]);
      if (info) selfName = info.self;
      drawGrants(plugins);
    } catch (e) {
      fill(grantsBox, errorBox(e));
    }
  }

  function drawGrants(plugins) {
    const withPerms = plugins.filter((p) => (p.permissions ?? []).length || (p.capabilities ?? []).length || p.state === "ready");
    if (!withPerms.length) {
      fill(grantsBox, h("p", { class: "muted" }, "还没有已就绪的插件。插件启动后，它申请的能力会显示在这里。"));
      return;
    }
    fill(grantsBox, withPerms.map((p) => {
      const isSelf = p.name === selfName;
      const perms = p.permissions ?? [];
      const caps = p.capabilities ?? [];
      const sensitiveOn = perms.filter((x) => x.sensitive && x.granted).length;
      const sensitiveProvided = caps.filter((c) => c.sensitive).length;
      return h("article", { class: "grant-card", dataset: { plugin: p.name } },
        h("header", null,
          h("h3", null, p.name),
          sensitiveOn ? h("span", { class: "pill bad" }, `可调用 ${sensitiveOn} 项敏感能力`) : h("span", { class: "pill muted" }, "不调用敏感能力"),
          sensitiveProvided ? h("span", { class: "pill wait" }, `提供 ${sensitiveProvided} 项敏感能力`) : null,
          p.grants ? h("span", { class: "tag" }, "已收紧") : null,
          h("span", { class: "spacer" }),
          isSelf ? h("small", { class: "muted" }, "当前页面，不能修改自己的权限") : null),
        h("h4", { class: "grant-sub" }, "调用的能力"),
        perms.length ? h("ul", { class: "grant-list" }, perms.map((perm) => {
          const sw = h("input", { type: "checkbox", role: "switch", disabled: isSelf, "aria-label": `允许 ${p.name} 调用 ${perm.name}`,
            onchange: () => setGrant(p, perm.name, sw.checked, sw) });
          sw.checked = perm.granted;
          return h("li", { class: perm.granted ? "" : "off" },
            h("span", { class: "grant-name" }, h("code", null, perm.name), h("small", null, capLabel(perm))),
            perm.sensitive ? h("span", { class: "tag danger" }, "敏感") : null,
            perm.wildcard ? h("span", { class: "tag" }, "通配") : null,
            h("label", { class: "switch" }, sw));
        })) : h("p", { class: "muted small" }, "这个插件没有申请调用任何能力。"),
        caps.length ? [
          h("h4", { class: "grant-sub" }, "提供的能力"),
          h("ul", { class: "grant-list provided" }, caps.map((c) => h("li", null,
            h("span", { class: "grant-name" }, h("code", null, c.name), h("small", null, c.description || "没有说明")),
            c.sensitive ? h("span", { class: "tag danger", title: "只有开启了「允许敏感能力」并设置了口令的连接，以及被授权的插件能调用" }, "敏感") : null,
            c.registered === false ? h("span", { class: "tag", title: c.reason || "" }, "未注册") : null))),
        ] : null);
    }));
  }

  async function setGrant(p, name, on, sw) {
    if (grantsBusy) { sw.checked = !on; return; }
    const current = (p.permissions ?? []).filter((x) => x.granted).map((x) => x.name);
    const next = on ? [...new Set([...current, name])] : current.filter((x) => x !== name);
    const all = (p.permissions ?? []).map((x) => x.name);
    // 全部打开 = 不收紧（插件以后新声明的能力也会自动允许）
    const grants = all.every((x) => next.includes(x)) ? null : next;
    grantsBusy = true;
    try {
      await api("POST", "/api/plugins/grants", { plugin: p.name, grants });
      toast(on ? `已允许 ${p.name} 调用 ${name}` : `已收回 ${p.name} 的 ${name} 权限`);
      await loadGrants();
    } catch (e) {
      sw.checked = !on;
      toast(e.message, "error");
    } finally {
      grantsBusy = false;
    }
  }

  grantsSlot.replaceWith(grantsSection);
  load();
  loadGrants().then(() => {
    // 从插件页点「修改权限」过来时，滚到那个插件的卡片
    if (!grantsFocus) return;
    const card = [...grantsBox.querySelectorAll(".grant-card")].find((c) => c.dataset.plugin === grantsFocus);
    grantsFocus = "";
    if (card) {
      card.scrollIntoView({ block: "center", behavior: "smooth" });
      card.classList.add("focus");
      setTimeout(() => card.classList.remove("focus"), 2000);
    }
  });
  const stopGrants = every(5000, () => { if (!grantsBusy) loadGrants(); });
  return stopGrants;
}

// ================= 更新 =================

export function renderUpdate(root) {
  const current = h("dl", { class: "facts" });
  const latest = h("div", { class: "release" }, h("p", { class: "muted" }, "点「检查更新」查询 GitHub 上的最新版本。"));
  const proxyInput = h("input", { type: "url", placeholder: "例如 https://ghfast.top/，不填则直连 GitHub", spellcheck: "false" });
  const fileName = h("span", { class: "muted file-name" }, "还没有选择文件");
  const fileInput = h("input", { type: "file", class: "visually-hidden", accept: ".zip,.exe,application/zip,application/octet-stream",
    onchange: () => { fileName.textContent = fileInput.files?.[0] ? `${fileInput.files[0].name}（${bytes(fileInput.files[0].size)}）` : "还没有选择文件"; } });
  const checkBtn = h("button", { class: "btn primary", type: "button", onclick: check }, "检查更新");
  fill(root, page("更新 goqbot", "goqbot 程序和 WebUI 会一起更新，完成后自动重启，配置和数据都会保留。",
    h("section", { class: "panel" }, h("h2", null, "当前版本"), current),
    h("section", { class: "panel" },
      h("div", { class: "panel-bar" }, h("h2", null, "在线更新"), checkBtn),
      latest),
    h("section", { class: "panel" },
      h("h2", null, "手动上传"),
      h("p", { class: "muted" }, "选择从发布页下载的压缩包（.zip），会连 WebUI 一起更新；也可以只选解压出来的 goqbot 程序，这样只更新程序。"),
      h("div", { class: "inline" },
        h("label", { class: "btn" }, fileInput, "选择文件"), fileName,
        h("span", { class: "spacer" }),
        h("button", { class: "btn", type: "button", onclick: upload }, "上传并更新"))),
    h("section", { class: "panel" },
      h("h2", null, "下载代理"),
      h("p", { class: "muted" }, "国内连 GitHub 慢时可以填一个加速地址，下载更新时会加在 GitHub 链接前面。"),
      h("div", { class: "inline" }, proxyInput, h("button", { class: "btn", type: "button", onclick: saveProxy }, "保存")))));

  (async () => {
    try {
      const r = await api("GET", "/api/update");
      fill(current, fact("版本", r.current), fact("平台", r.platform));
      proxyInput.value = r.github_proxy;
    } catch (e) {
      fill(current, errorBox(e));
    }
  })();

  async function check() {
    checkBtn.disabled = true;
    fill(latest, h("p", { class: "muted" }, "正在查询 GitHub…"));
    try {
      const r = await api("GET", "/api/update/check");
      fill(latest, 
        h("p", null, r.has_update ? pill("ok", `有新版本 ${r.latest}`) : pill("muted", `已经是最新版本（${r.current}）`),
          " ", h("a", { href: r.url, target: "_blank", rel: "noopener noreferrer" }, "查看发布页")),
        r.notes ? h("pre", { class: "notes" }, r.notes) : null,
        r.has_update && r.asset ? h("div", { class: "form-actions" },
          h("span", { class: "muted" }, `${r.asset.name}（${bytes(r.asset.size)}）${r.asset.webui ? "" : "，只有程序，WebUI 不会更新"}`),
          h("span", { class: "spacer" }),
          h("button", { class: "btn primary", type: "button", onclick: install }, `更新到 ${r.latest}`)) : null,
        r.has_update && !r.asset ? h("p", { class: "warn" }, "这个版本没有当前平台的安装包，请到发布页手动下载。") : null);
    } catch (e) {
      fill(latest, errorBox(e));
    } finally {
      checkBtn.disabled = false;
    }
  }

  async function install() {
    if (!(await confirmDialog("更新 goqbot", "会下载新版本，替换 goqbot 程序和 WebUI 后重启。重启期间机器人会短暂离线。", "开始更新"))) return;
    fill(latest, h("p", { class: "muted" }, "正在下载和校验，请不要关闭页面…"));
    try {
      const r = await api("POST", "/api/update/install");
      toast(`已更新到 ${r.to}${r.webui ? "（含 WebUI）" : ""}，正在重启`);
      restartFlow(true);
    } catch (e) {
      fill(latest, errorBox(e));
    }
  }

  async function upload() {
    const f = fileInput.files?.[0];
    if (!f) { toast("先选择一个文件", "error"); return; }
    if (!(await confirmDialog("更新 goqbot", `用 ${f.name} 更新并重启？压缩包里带 WebUI 时会一起更新。`, "开始更新"))) return;
    try {
      const r = await api("POST", "/api/update/upload", f);
      toast(`已更新到 ${r.to}${r.webui ? "（含 WebUI）" : "（WebUI 未更新）"}，正在重启`);
      restartFlow(true);
    } catch (e) {
      toast(e.message, "error");
    }
  }

  async function saveProxy() {
    try {
      await api("PUT", "/api/update/proxy", { github_proxy: proxyInput.value });
      toast(proxyInput.value.trim() ? "下载代理已保存" : "已改为直连 GitHub");
    } catch (e) {
      toast(e.message, "error");
    }
  }
  return () => {};
}
