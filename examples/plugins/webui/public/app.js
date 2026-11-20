// 入口：登录、侧栏导航、路由、实时推送、重启等待
import { h, $, icon, api, auth, toast, openStream, setUnauthorizedHandler } from "./core.js";
import { chatState, loadChats, onMessage, onChat, onChatRemoved, onChatRenamed, renderChatPage, selectChat, unreadOf } from "./chat.js";
import { renderOverview, renderConnections, renderPlugins, renderLogs, renderConfig, renderUpdate, setRestartFlow } from "./admin.js";

const NAV = [
  { group: "聊天", items: [
    { route: "group", label: "群聊", icon: "group" },
    { route: "private", label: "私聊", icon: "person" },
  ] },
  { group: "管理", items: [
    { route: "overview", label: "概览", icon: "pulse" },
    { route: "connections", label: "连接", icon: "link" },
    { route: "plugins", label: "插件", icon: "plug" },
    { route: "logs", label: "日志", icon: "list" },
    { route: "config", label: "配置", icon: "sliders" },
    { route: "update", label: "更新", icon: "download" },
  ] },
];

let selfName = "webui";
let stopPage = () => {};
let stopStream = () => {};
let currentRoute = "";
let loggedIn = false;

// ---------- 主题：跟随系统 / 浅色 / 深色 ----------

const THEME_KEY = "goqbot-webui-theme";
const THEMES = [["auto", "跟随系统", "auto"], ["light", "浅色", "sun"], ["dark", "深色", "moon"]];
function getTheme() { try { return localStorage.getItem(THEME_KEY) || "auto"; } catch { return "auto"; } }
function applyTheme(t) {
  document.documentElement.dataset.theme = t;
  const btn = $("#theme");
  if (btn) {
    const [, label, ic] = THEMES.find((x) => x[0] === t) ?? THEMES[0];
    btn.replaceChildren(icon(ic), h("span", null, label));
    btn.title = "切换主题";
  }
}
applyTheme(getTheme());

// ---------- 登录 ----------

function showLogin(message = "") {
  loggedIn = false;
  stopStream();
  stopPage();
  $("#app").hidden = true;
  $("#login").hidden = false;
  $("#login-error").textContent = message;
  $("#login-token").focus();
}

setUnauthorizedHandler(() => {
  auth.set("");
  showLogin("登录已失效，请重新输入口令");
});

$("#login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const t = $("#login-token").value.trim();
  if (!t) return;
  auth.set(t);
  try {
    await api("POST", "/api/login", undefined, { silent401: true });
    $("#login-token").value = "";
    start();
  } catch (err) {
    auth.set("");
    $("#login-error").textContent = err.message;
  }
});

// ---------- 侧栏 ----------

function buildRail() {
  const rail = $("#rail");
  rail.replaceChildren(
    h("a", { class: "brand", href: "#/group", "aria-label": "goqbot 首页" }, h("span", { class: "brand-mark", "aria-hidden": "true" }, "g"), h("span", null, "goqbot")),
    ...NAV.map((g) => h("div", { class: "nav-group", role: "group", "aria-label": g.group },
      g.items.map((it) => h("a", { class: "nav", href: "#/" + it.route, dataset: { route: it.route } },
        icon(it.icon), h("span", null, it.label), h("span", { class: "nav-badge", hidden: true }))))),
    h("div", { class: "rail-foot" },
      h("span", { id: "live", class: "live", title: "" }),
      h("button", { id: "theme", class: "nav", type: "button", onclick: () => {
        const i = THEMES.findIndex((x) => x[0] === getTheme());
        const next = THEMES[(i + 1) % THEMES.length][0];
        try { localStorage.setItem(THEME_KEY, next); } catch { /* 只在本次生效 */ }
        applyTheme(next);
      } }),
      h("button", { class: "nav", type: "button", onclick: () => { auth.set(""); showLogin(); } }, icon("logout"), h("span", null, "退出"))));
  applyTheme(getTheme());
}

function updateBadges() {
  for (const type of ["group", "private"]) {
    const el = document.querySelector(`.nav[data-route="${type}"] .nav-badge`);
    if (!el) continue;
    const n = unreadOf(type);
    el.hidden = !n;
    el.textContent = n > 99 ? "99+" : String(n);
  }
  const total = unreadOf("group") + unreadOf("private");
  document.title = total ? `(${total}) goqbot` : "goqbot";
}
chatState.onUnreadChange = updateBadges;

function setLive(ok) {
  const el = $("#live");
  if (!el) return;
  el.classList.toggle("on", ok);
  el.title = ok ? "实时接收中" : "实时连接已断开，正在重连";
  el.textContent = ok ? "实时" : "重连中";
}

// ---------- 路由 ----------

function route() {
  const [, name = "group", ...rest] = location.hash.replace(/^#/, "").split("/");
  const key = rest.length ? decodeURIComponent(rest.join("/")) : "";
  const known = NAV.flatMap((g) => g.items).some((it) => it.route === name);
  if (!known) { location.hash = "#/group"; return; }
  for (const a of document.querySelectorAll(".rail .nav[data-route]")) {
    a.classList.toggle("active", a.dataset.route === name);
    if (a.dataset.route === name) a.setAttribute("aria-current", "page"); else a.removeAttribute("aria-current");
  }
  // 同一个聊天页里切换会话，不重建页面
  if (name === currentRoute && (name === "group" || name === "private") && selectChat(name, key)) return;
  stopPage();
  currentRoute = name;
  const view = $("#view");
  view.className = "view" + (name === "group" || name === "private" ? " view-chat" : "");
  const render = {
    group: () => renderChatPage(view, "group", key),
    private: () => renderChatPage(view, "private", key),
    overview: () => renderOverview(view),
    connections: () => renderConnections(view),
    plugins: () => renderPlugins(view, selfName),
    logs: () => renderLogs(view),
    config: () => renderConfig(view),
    update: () => renderUpdate(view),
  }[name];
  stopPage = render() ?? (() => {});
}
window.addEventListener("hashchange", () => {
  if (takeTokenFromHash()) tryLogin();
  else if (loggedIn) route();
});

// ---------- 启动 ----------

async function start() {
  loggedIn = true;
  $("#login").hidden = true;
  $("#app").hidden = false;
  buildRail();
  try {
    const r = await api("GET", "/api/info");
    selfName = r.self;
    chatState.appId = r.app_id;
    chatState.botName = r.info?.qq?.bot_name || "机器人";
    await loadChats();
  } catch (e) {
    if (e.status === 401) return;
    toast(e.message, "error");
  }
  stopStream();
  stopStream = openStream({ message: onMessage, chat: onChat, chat_removed: onChatRemoved, chat_renamed: onChatRenamed }, setLive);
  currentRoute = "";
  route();
}

// ---------- 重启：等 goqbot 回来 ----------

setRestartFlow(async (alreadyRequested = false) => {
  const overlay = $("#overlay");
  const text = $("#overlay-text");
  let before = null;
  try { before = (await api("GET", "/api/info")).info; } catch { /* 已经在重启 */ }
  overlay.hidden = false;
  text.textContent = "正在重启 goqbot…";
  if (!alreadyRequested) {
    try { await api("POST", "/api/restart"); } catch (e) { overlay.hidden = true; toast(e.message, "error"); return; }
  }
  const started = Date.now();
  await new Promise((r) => setTimeout(r, 2000));
  while (Date.now() - started < 120_000) {
    try {
      const { info } = await api("GET", "/api/info", undefined, { silent401: true });
      if (!before || info.pid !== before.pid || info.uptime_seconds < before.uptime_seconds) {
        overlay.hidden = true;
        toast(before && info.version !== before.version ? `已更新到 ${info.version}` : "goqbot 已重启");
        start();
        return;
      }
    } catch { /* 还没起来 */ }
    text.textContent = `正在重启 goqbot…已等待 ${Math.round((Date.now() - started) / 1000)} 秒`;
    await new Promise((r) => setTimeout(r, 1500));
  }
  text.textContent = "等了 2 分钟还没有恢复，请到运行 goqbot 的机器上查看日志";
});

// 带口令的链接：#token=xxx 自动登录，然后从地址栏去掉口令
function takeTokenFromHash() {
  const m = /(?:^#|&)token=([^&]+)/.exec(location.hash);
  if (!m) return false;
  auth.set(decodeURIComponent(m[1]));
  history.replaceState(null, "", location.pathname + "#/group");
  return true;
}

function tryLogin() {
  if (!auth.get()) { showLogin(); return; }
  api("POST", "/api/login", undefined, { silent401: true }).then(start, (e) => {
    if (e.status === 401) { auth.set(""); showLogin("口令不正确"); }
    else showLogin(e.message);
  });
}

takeTokenFromHash();
tryLogin();
