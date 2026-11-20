// 公共工具：建 DOM、请求接口、提示、弹窗、时间格式

/** 建元素。attrs 里 on* 是事件，class 是类名，其余写成属性；children 可以是字符串、元素、数组、null */
export function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs ?? {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith("on") && typeof v === "function") el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else if (k === "dataset") Object.assign(el.dataset, v);
    else el.setAttribute(k, v === true ? "" : String(v));
  }
  append(el, children);
  return el;
}

function append(el, children) {
  for (const c of children) {
    if (c === null || c === undefined || c === false) continue;
    if (Array.isArray(c)) append(el, c);
    else el.append(c instanceof Node ? c : String(c));
  }
}

export const $ = (sel, root = document) => root.querySelector(sel);

/** 替换元素的全部子节点，跳过 null / undefined / false（replaceChildren 会把它们当成文字） */
export function fill(el, ...children) {
  el.replaceChildren(...children.flat(Infinity).filter((c) => c !== null && c !== undefined && c !== false));
}

/** 图标：几笔线条，跟随文字颜色 */
const ICONS = {
  person: "M12 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8ZM4 20a8 8 0 0 1 16 0",
  group: "M9 11a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7ZM2.5 20a6.5 6.5 0 0 1 13 0M16 4.5a3.5 3.5 0 0 1 0 6.5M18 14a6 6 0 0 1 3.5 6",
  pulse: "M3 12h4l3-8 4 16 3-8h4",
  link: "M10 14a4 4 0 0 0 5.7 0l3-3a4 4 0 0 0-5.7-5.7l-1 1M14 10a4 4 0 0 0-5.7 0l-3 3a4 4 0 0 0 5.7 5.7l1-1",
  plug: "M9 3v4M15 3v4M7 7h10v5a5 5 0 0 1-10 0zM12 17v4",
  list: "M5 6h14M5 12h14M5 18h9",
  sliders: "M4 7h10M18 7h2M4 17h4M12 17h8M14 4v6M8 14v6",
  download: "M12 4v11M7 10l5 5 5-5M5 20h14",
  sun: "M12 16a4 4 0 1 0 0-8 4 4 0 0 0 0 8ZM12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4",
  moon: "M20 14.5A8 8 0 0 1 9.5 4 8 8 0 1 0 20 14.5Z",
  auto: "M12 3a9 9 0 1 0 0 18V3Z M12 3a9 9 0 0 1 0 18",
  logout: "M15 4h3a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2h-3M10 16l-4-4 4-4M6 12h10",
  search: "M11 18a7 7 0 1 0 0-14 7 7 0 0 0 0 14ZM20 20l-4-4",
  edit: "M4 20h4L19 9l-4-4L4 16v4ZM13.5 6.5l4 4",
  back: "M15 5l-7 7 7 7",
  plus: "M12 5v14M5 12h14",
  trash: "M5 7h14M10 11v6M14 11v6M7 7l1 13h8l1-13M9 7V4h6v3",
};
export function icon(name, cls = "icon") {
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("class", cls);
  svg.setAttribute("aria-hidden", "true");
  const path = document.createElementNS(ns, "path");
  path.setAttribute("d", ICONS[name] ?? "");
  svg.append(path);
  return svg;
}

// ---------- 登录口令与接口 ----------

const TOKEN_KEY = "goqbot-webui-token";
export const auth = {
  get() {
    try { return localStorage.getItem(TOKEN_KEY) ?? ""; } catch { return ""; }
  },
  set(t) {
    try { t ? localStorage.setItem(TOKEN_KEY, t) : localStorage.removeItem(TOKEN_KEY); } catch { /* 浏览器不让存时只在本次有效 */ }
    memoryToken = t;
  },
};
let memoryToken = "";
const token = () => auth.get() || memoryToken;

/** 接口出错：status 为 HTTP 状态码，message 为中文说明 */
export class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

let onUnauthorized = () => {};
export function setUnauthorizedHandler(fn) { onUnauthorized = fn; }

export async function api(method, path, body, { raw = false, silent401 = false } = {}) {
  const headers = { authorization: "Bearer " + token() };
  let payload;
  if (body instanceof Blob) {
    payload = body;
    headers["content-type"] = "application/octet-stream";
  } else if (body !== undefined) {
    payload = JSON.stringify(body);
    headers["content-type"] = "application/json";
  }
  let res;
  try {
    res = await fetch(path, { method, headers, body: payload });
  } catch {
    throw new ApiError(0, "连不上 WebUI，goqbot 可能没在运行");
  }
  if (raw) return res;
  let data = {};
  try { data = await res.json(); } catch { /* 空响应 */ }
  if (!res.ok) {
    if (res.status === 401 && !silent401) onUnauthorized();
    throw new ApiError(res.status, data.error || `请求失败（HTTP ${res.status}）`);
  }
  return data;
}

/** 实时推送：用 fetch 读 text/event-stream（EventSource 不能带口令头），断开后自动重连 */
export function openStream(handlers, onState) {
  let stopped = false;
  let ctrl;
  (async () => {
    let wait = 1000;
    while (!stopped) {
      ctrl = new AbortController();
      try {
        const res = await fetch("/api/stream", { headers: { authorization: "Bearer " + token() }, signal: ctrl.signal });
        if (res.status === 401) { onUnauthorized(); return; }
        if (!res.ok || !res.body) throw new Error();
        onState(true);
        wait = 1000;
        const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
        let buf = "";
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          buf += value;
          let i;
          while ((i = buf.indexOf("\n\n")) >= 0) {
            const frame = buf.slice(0, i);
            buf = buf.slice(i + 2);
            let event = "message", data = "";
            for (const line of frame.split("\n")) {
              if (line.startsWith("event: ")) event = line.slice(7);
              else if (line.startsWith("data: ")) data += line.slice(6);
            }
            if (data && handlers[event]) {
              try { handlers[event](JSON.parse(data)); } catch (e) { console.error(e); }
            }
          }
        }
      } catch { /* 断开后重连 */ }
      if (stopped) return;
      onState(false);
      await new Promise((r) => setTimeout(r, wait));
      wait = Math.min(wait * 2, 15000);
    }
  })();
  return () => { stopped = true; ctrl?.abort(); };
}

// ---------- 提示、弹窗 ----------

export function toast(text, kind = "ok") {
  const el = h("div", { class: "toast " + kind }, text);
  $("#toasts").append(el);
  setTimeout(() => el.classList.add("out"), kind === "error" ? 5000 : 2600);
  setTimeout(() => el.remove(), kind === "error" ? 5400 : 3000);
}

/** 打开弹窗，body 为元素；返回关闭函数 */
export function openDialog(title, body, { wide = false, mid = false } = {}) {
  const dlg = $("#dialog");
  dlg.classList.toggle("wide", wide);
  dlg.classList.toggle("mid", mid);
  $("#dialog-title").textContent = title;
  $("#dialog-body").replaceChildren(body);
  if (!dlg.open) dlg.showModal();
  return () => dlg.close();
}

/** 确认框，返回 Promise<boolean> */
export function confirmDialog(title, text, okText = "确定", danger = false) {
  return new Promise((resolve) => {
    let done = false;
    const finish = (v) => { if (!done) { done = true; close(); resolve(v); } };
    const body = h("div", { class: "confirm" },
      h("p", null, text),
      h("div", { class: "actions" },
        h("button", { class: "btn", type: "button", onclick: () => finish(false) }, "取消"),
        h("button", { class: "btn " + (danger ? "danger" : "primary"), type: "button", onclick: () => finish(true) }, okText)));
    const close = openDialog(title, body);
    $("#dialog").addEventListener("close", () => finish(false), { once: true });
  });
}

/** 输入框，返回 Promise<string|null> */
export function promptDialog(title, label, value = "", placeholder = "") {
  return new Promise((resolve) => {
    let done = false;
    const input = h("input", { type: "text", value, placeholder, maxlength: 40 });
    const finish = (v) => { if (!done) { done = true; close(); resolve(v); } };
    const form = h("form", { class: "confirm", onsubmit: (e) => { e.preventDefault(); finish(input.value); } },
      h("label", { class: "field" }, h("span", null, label), input),
      h("div", { class: "actions" },
        h("button", { class: "btn", type: "button", onclick: () => finish(null) }, "取消"),
        h("button", { class: "btn primary", type: "submit" }, "保存")));
    const close = openDialog(title, form);
    $("#dialog").addEventListener("close", () => finish(null), { once: true });
    input.focus();
    input.select();
  });
}

// ---------- 格式化 ----------

export function pad(n) { return String(n).padStart(2, "0"); }

/** 会话列表里的时间：今天显示时分，今年显示月日，更早显示年月日 */
export function shortTime(sec) {
  if (!sec) return "";
  const d = new Date(sec * 1000), now = new Date();
  if (d.toDateString() === now.toDateString()) return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  const y = new Date(now); y.setDate(now.getDate() - 1);
  if (d.toDateString() === y.toDateString()) return "昨天";
  if (d.getFullYear() === now.getFullYear()) return `${d.getMonth() + 1}月${d.getDate()}日`;
  return `${d.getFullYear()}/${d.getMonth() + 1}/${d.getDate()}`;
}

export function clock(sec) {
  const d = new Date(sec * 1000);
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function dayLabel(sec) {
  const d = new Date(sec * 1000), now = new Date();
  if (d.toDateString() === now.toDateString()) return "今天";
  const y = new Date(now); y.setDate(now.getDate() - 1);
  if (d.toDateString() === y.toDateString()) return "昨天";
  const w = "日一二三四五六"[d.getDay()];
  return d.getFullYear() === now.getFullYear() ? `${d.getMonth() + 1}月${d.getDate()}日 星期${w}` : `${d.getFullYear()}年${d.getMonth() + 1}月${d.getDate()}日`;
}

export function duration(sec) {
  const d = Math.floor(sec / 86400), hh = Math.floor((sec % 86400) / 3600), mm = Math.floor((sec % 3600) / 60);
  if (d) return `${d} 天 ${hh} 小时`;
  if (hh) return `${hh} 小时 ${mm} 分`;
  return `${mm} 分 ${sec % 60} 秒`;
}

export function bytes(n) {
  if (n < 1024) return n + " B";
  if (n < 1048576) return (n / 1024).toFixed(1) + " KB";
  return (n / 1048576).toFixed(1) + " MB";
}

/** 按字符串算一个稳定的色相，用来给没有头像的会话上色 */
export function hue(s) {
  // FNV-1a 再混合一次，相邻的编号（1000001、1000002）也能分到差得很远的颜色
  let x = 0x811c9dc5;
  for (const ch of String(s)) x = Math.imul(x ^ ch.codePointAt(0), 0x01000193);
  x ^= x >>> 15; x = Math.imul(x, 0x2c1b3c6d); x ^= x >>> 12;
  return (x >>> 0) % 360;
}
