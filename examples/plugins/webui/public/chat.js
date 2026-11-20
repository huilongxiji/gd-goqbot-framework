// 私聊 / 群聊页面：左边会话列表，右边消息窗口。当前阶段只接收消息
import { h, fill, icon, api, toast, promptDialog, openDialog, shortTime, dayLabel, hue } from "./core.js";
import { renderSegments, summaryOf, setImageViewer, setForwardViewer, forwardLine, stripFaces, onlyImages } from "./msg.js";

const PAGE_SIZE = 50;

/** 页面共享的状态 */
export const chatState = {
  appId: "",
  botName: "机器人",
  chats: new Map(), // key -> chat
  onUnreadChange: () => {},
};

let view = null; // 当前打开的聊天页 { type, root, current, messages, ... }

// ---------- 查看大图：全屏遮罩，点图片在「适应窗口」和「原始大小」之间切换，点空白处或按 Esc 关闭 ----------

setImageViewer((url) => {
  const img = h("img", { src: url, alt: "图片", referrerpolicy: "no-referrer" });
  const dlg = h("dialog", { class: "lightbox", "aria-label": "查看大图" },
    h("div", { class: "lightbox-stage" }, img),
    h("div", { class: "lightbox-bar" },
      h("a", { class: "lightbox-btn", href: url, target: "_blank", rel: "noopener noreferrer" }, "新标签页打开"),
      h("button", { class: "lightbox-btn", type: "button", onclick: () => dlg.close() }, "关闭")));
  // 图片比窗口小时不需要放大
  const canZoom = () => img.naturalWidth > img.clientWidth + 1 || img.naturalHeight > img.clientHeight + 1;
  img.addEventListener("load", () => dlg.classList.toggle("can-zoom", canZoom()));
  img.addEventListener("click", (e) => {
    e.stopPropagation();
    if (dlg.classList.contains("zoomed")) dlg.classList.remove("zoomed");
    else if (canZoom()) dlg.classList.add("zoomed");
  });
  img.addEventListener("error", () => img.replaceWith(h("p", { class: "lightbox-error" }, "图片已过期或无法加载")), { once: true });
  dlg.addEventListener("click", (e) => { if (e.target === dlg || e.target.classList?.contains("lightbox-stage")) dlg.close(); });
  dlg.addEventListener("close", () => dlg.remove(), { once: true });
  document.body.append(dlg);
  dlg.showModal();
});

// ---------- 聊天记录：展开全部消息 ----------

setForwardViewer((title, nodes) => {
  const ctx = view ? messageCtx() : { findReply: () => null, nameOf: () => "" };
  openDialog(title, h("ol", { class: "fwd-list" }, nodes.map((nd) => {
    const { name } = forwardLine(nd);
    const t = Number(nd.time) || 0;
    const content = Array.isArray(nd.content) ? nd.content : [{ type: "text", data: { text: String(nd.content ?? "") } }];
    const parts = renderSegments(content, ctx);
    return h("li", null,
      h("div", { class: "fwd-meta" }, h("b", null, name || "未知"), t ? h("time", null, new Date(t * 1000).toLocaleString("zh-CN", { hour12: false })) : null),
      h("div", { class: onlyImages(parts) ? "fwd-body bare" : "fwd-body" }, parts));
  })), { mid: true });
});

// ---------- 名称与头像 ----------

export function chatTitle(c) {
  if (c.remark) return c.remark;
  if (c.type === "private") return c.name || `用户 ${c.id}`;
  return c.name || `群 ${c.id}`;
}

/** 首字色块：没有头像时用，颜色按编号固定 */
function monogram(text, seed, cls) {
  const t = cls.includes("digits") ? String(text) : [...String(text || "?")][0];
  const el = h("span", { class: "avatar mono " + cls, "aria-hidden": "true" }, t);
  el.style.setProperty("--h", hue(seed));
  return el;
}

/**
 * 用户头像：优先用事件里带的头像地址（sender.avatar），
 * 没有时按官方规则拼 https://q.qlogo.cn/qqapp/<AppID>/<openid>/100，都打不开时显示首字色块
 */
function avatar(openid, fallbackText, seed, cls = "", url = "") {
  const fallback = monogram(fallbackText, seed, cls);
  const src = /^https?:\/\//i.test(url) ? url
    : openid && chatState.appId ? `https://q.qlogo.cn/qqapp/${encodeURIComponent(chatState.appId)}/${encodeURIComponent(openid)}/100` : "";
  if (!src) return fallback;
  const img = h("img", { class: "avatar " + cls, alt: "", loading: "lazy", referrerpolicy: "no-referrer", src });
  img.addEventListener("error", () => img.replaceWith(fallback), { once: true });
  return img;
}

function chatAvatar(c, cls) {
  if (c.type === "group") {
    // 官方接口不提供群头像：有群名或备注时显示首字，都没有时显示群编号的后两位
    const named = c.remark || c.name;
    return monogram(named || String(c.id).slice(-2), "g" + c.id, cls + " square" + (named ? "" : " digits"));
  }
  return avatar(c.openid, chatTitle(c), "p" + c.id, cls, c.avatar);
}

// ---------- 未读数 ----------

export function unreadOf(type) {
  let n = 0;
  for (const c of chatState.chats.values()) if (c.type === type) n += c.unread || 0;
  return n;
}

export async function loadChats() {
  const { chats } = await api("GET", "/api/chats");
  chatState.chats = new Map(chats.map((c) => [c.key, c]));
  chatState.onUnreadChange();
}

/** 实时推送：新消息 */
export function onMessage({ chat, message }) {
  const isOpen = view && view.current === chat.key;
  if (isOpen && document.visibilityState === "visible") {
    chat.unread = 0;
    api("POST", "/api/chats/read", { key: chat.key }).catch(() => {});
  }
  chatState.chats.set(chat.key, chat);
  chatState.onUnreadChange();
  if (!view) return;
  if (view.type === chat.type) renderList();
  if (isOpen) {
    view.messages.push(message);
    appendMessage(message, true);
  }
}

/** 实时推送：会话信息变了（例如改了备注） */
export function onChat(chat) {
  chatState.chats.set(chat.key, chat);
  if (view && view.type === chat.type) {
    renderList();
    if (view.current === chat.key) renderHeader();
  }
}

// ---------- 页面 ----------

export function renderChatPage(root, type, key) {
  view = {
    type, root, current: "", messages: [], more: false, loading: false, filter: "",
    list: h("div", { class: "chat-list", role: "list" }),
    panel: h("section", { class: "chat-panel" }),
  };
  const search = h("input", {
    type: "search", placeholder: type === "group" ? "搜索群备注或编号" : "搜索昵称、备注或编号", "aria-label": "搜索会话",
    oninput: (e) => { view.filter = e.target.value.trim(); renderList(); },
  });
  fill(root, h("div", { class: "chat" + (key ? " has-current" : "") },
    h("aside", { class: "chat-side" },
      h("header", { class: "chat-side-head" },
        h("h1", null, type === "group" ? "群聊" : "私聊"),
        h("label", { class: "search" }, icon("search"), search)),
      view.list),
    view.panel));
  renderList();
  if (key && chatState.chats.has(key)) openChat(key);
  else renderEmptyPanel();
  return () => { view = null; };
}

function renderList() {
  const items = [...chatState.chats.values()]
    .filter((c) => c.type === view.type)
    .filter((c) => {
      if (!view.filter) return true;
      const f = view.filter.toLowerCase();
      return chatTitle(c).toLowerCase().includes(f) || String(c.id).includes(f) || (c.name || "").toLowerCase().includes(f);
    })
    .sort((a, b) => b.last_time - a.last_time || b.seq - a.seq);
  if (!items.length) {
    fill(view.list, h("p", { class: "list-empty" },
      view.filter ? "没有匹配的会话。" :
        view.type === "group" ? "还没有收到群消息。机器人在群里被 @ 后，这个群就会出现在这里。" : "还没有收到私聊消息。有人私聊机器人后，就会出现在这里。"));
    return;
  }
  fill(view.list, ...items.map((c) =>
    h("a", { class: "chat-item" + (c.key === view.current ? " active" : ""), href: `#/${c.type}/${encodeURIComponent(c.key)}`, role: "listitem" },
      chatAvatar(c, "md"),
      h("span", { class: "chat-item-main" },
        h("span", { class: "chat-item-top" }, h("b", null, chatTitle(c)), h("time", null, shortTime(c.last_time))),
        h("span", { class: "chat-item-bottom" },
          h("span", { class: "chat-item-text" }, c.type === "group" && c.last_sender ? `${c.last_sender}：${preview(c.last_text)}` : c.last_sender === "我" ? `我：${preview(c.last_text)}` : preview(c.last_text)),
          c.unread ? h("span", { class: "badge" }, c.unread > 99 ? "99+" : c.unread) : null)))));
}

function renderEmptyPanel() {
  fill(view.panel, h("div", { class: "panel-empty" },
    icon(view.type === "group" ? "group" : "person", "icon big"),
    h("p", null, "在左边选一个会话查看消息")));
}

async function openChat(key) {
  const chat = chatState.chats.get(key);
  if (!chat) return;
  view.current = key;
  view.messages = [];
  view.more = false;
  view.root.querySelector(".chat")?.classList.add("has-current");
  view.head = h("header", { class: "panel-head" });
  view.scroller = h("div", { class: "msgs", role: "log", "aria-live": "polite" });
  fill(view.panel, view.head, view.scroller,
    h("footer", { class: "composer-off" }, "目前只接收消息，发送功能还在开发中"));
  renderHeader();
  renderList();
  view.scroller.addEventListener("scroll", () => {
    if (view.scroller.scrollTop < 80 && view.more && !view.loading) loadOlder();
  });
  view.scroller.append(h("p", { class: "msgs-hint" }, "正在读取消息…"));
  try {
    const r = await api("GET", `/api/chats/messages?key=${encodeURIComponent(key)}&limit=${PAGE_SIZE}`);
    if (view?.current !== key) return;
    view.messages = r.messages;
    view.more = r.more;
    renderMessages();
    view.scroller.scrollTop = view.scroller.scrollHeight;
  } catch (e) {
    fill(view.scroller, h("p", { class: "msgs-hint error" }, e.message));
  }
  if (chat.unread) {
    chat.unread = 0;
    chatState.onUnreadChange();
    renderList();
    api("POST", "/api/chats/read", { key }).catch(() => {});
  }
}

function renderHeader() {
  const c = chatState.chats.get(view.current);
  if (!c) return;
  const sub = c.type === "group"
    ? [h("span", { title: "goqbot 给这个群分配的编号，不是 QQ 群号" }, `群编号 ${c.id}`)]
    : [h("span", { title: "goqbot 给这个用户分配的编号，不是 QQ 号" }, `用户编号 ${c.id}`), c.remark && c.name ? h("span", null, `昵称 ${c.name}`) : null];
  fill(view.head, 
    h("a", { class: "btn ghost icon-only back", href: `#/${c.type}`, "aria-label": "返回会话列表" }, icon("back")),
    chatAvatar(c, "sm"),
    h("div", { class: "panel-title" },
      h("h2", null, h("button", {
        class: "title-btn", type: "button",
        title: c.type === "group" ? "查看群资料" : "查看用户资料",
        onclick: () => (c.type === "group" ? showGroupInfo(c) : showUserInfo({ openid: c.openid, userId: c.id, name: c.name, avatar: c.avatar })),
      }, chatTitle(c))),
      h("p", null, sub)),
    h("button", { class: "btn ghost small", type: "button", onclick: () => editRemark(c) }, icon("edit"), c.remark ? "修改备注" : "设置备注"));
}

// ---------- 群资料、用户资料 ----------

/** 把接口错误换成能看懂的说明 */
function friendlyError(msg) {
  const s = String(msg ?? "");
  if (/11253/.test(s)) return "机器人没有这个接口的权限（错误码 11253）。群基本信息、用户信息这类接口需要在 QQ 开放平台申请开通后才能用。";
  if (/发送通道未就绪|尚未初始化/.test(s)) return "QQ 还没连接上，暂时查不到。";
  if (/未知 user_id|未收到过该用户/.test(s)) return "还没收到过这个用户的消息，查不到。";
  return s || "查询失败";
}

/** 资料里的一行：名称 + 值（值为空时不显示） */
function infoRow(label, value, title) {
  if (value === undefined || value === null || value === "") return null;
  return [h("dt", null, label), h("dd", { title }, String(value))];
}

/** 接口返回的字段原样列出（对象、数组转成 JSON） */
function rawRows(info, skip = []) {
  return Object.entries(info ?? {})
    .filter(([k]) => !skip.includes(k))
    .map(([k, v]) => infoRow(k, v !== null && typeof v === "object" ? JSON.stringify(v) : v));
}

/** 点群名：调用群基本信息接口 */
async function showGroupInfo(c) {
  const body = h("div", { class: "profile" },
    h("div", { class: "profile-head" },
      chatAvatar(c, "lg"),
      h("div", null, h("b", null, chatTitle(c)), h("small", null, `群编号 ${c.id}`))),
    h("p", { class: "profile-loading" }, "正在查询群基本信息…"));
  openDialog("群资料", body, { mid: true });
  try {
    const { info, chat } = await api("GET", `/api/chats/group-info?key=${encodeURIComponent(c.key)}`);
    onChat(chat);
    const tags = Array.isArray(info.group_tags) ? info.group_tags.join("、") : "";
    fill(body,
      h("div", { class: "profile-head" },
        chatAvatar(chat, "lg"),
        h("div", null, h("b", null, info.group_name || chatTitle(chat)), h("small", null, c.remark && info.group_name ? `备注：${c.remark}` : `群编号 ${c.id}`))),
      h("dl", { class: "profile-list" },
        infoRow("群简介", info.group_finger_memo ?? info.group_memo),
        infoRow("分类", info.group_class_text),
        infoRow("标签", tags),
        infoRow("成员数", info.member_count ?? info.group_member_num),
        infoRow("群编号", c.id, "goqbot 给这个群分配的编号，不是 QQ 群号"),
        infoRow("群 OpenID", info.group_openid || c.openid)));
  } catch (e) {
    body.querySelector(".profile-loading")?.replaceWith(
      h("p", { class: "profile-error" }, friendlyError(e.message)),
      h("dl", { class: "profile-list" },
        infoRow("群编号", c.id, "goqbot 给这个群分配的编号，不是 QQ 群号"),
        infoRow("群 OpenID", c.openid)));
  }
}

/**
 * 点头像：调用 /v2/users/{user_openid}/info 查用户信息。
 * u = { openid, userId, name, card, avatar, role, bot }，来自消息或会话
 */
async function showUserInfo(u) {
  const roleText = u.role === "owner" ? "群主" : u.role === "admin" ? "管理员" : u.role === "member" ? "成员" : "";
  const head = (name, url) => h("div", { class: "profile-head" },
    avatar(u.openid, name, "p" + u.userId, "lg", url),
    h("div", null, h("b", null, name), u.card && u.card !== name ? h("small", null, `群昵称：${u.card}`) : h("small", null, `用户编号 ${u.userId}`)));
  const known = () => h("dl", { class: "profile-list" },
    infoRow("群昵称", u.card),
    infoRow("群身份", roleText),
    infoRow("机器人", u.bot ? "是" : ""),
    infoRow("用户编号", u.userId, "goqbot 给这个用户分配的编号，不是 QQ 号"),
    infoRow("用户 OpenID", u.openid));
  const name = u.card || u.name || `用户 ${u.userId}`;
  const body = h("div", { class: "profile" }, head(name, u.avatar), known(), h("p", { class: "profile-loading" }, "正在查询用户信息…"));
  openDialog("用户资料", body, { mid: true });
  try {
    const q = u.openid ? `openid=${encodeURIComponent(u.openid)}&user_id=${u.userId || 0}` : `user_id=${u.userId}`;
    const { info } = await api("GET", `/api/chats/user-info?${q}`);
    const apiName = info.nickname || info.username || "";
    const apiAvatar = typeof info.avatar === "string" ? info.avatar : "";
    const rows = rawRows(info, ["user_id", "user_openid", "sex", "age"]);
    fill(body,
      head(apiName || name, apiAvatar || u.avatar),
      known(),
      h("h3", { class: "profile-sub" }, "用户信息接口返回"),
      rows.length ? h("dl", { class: "profile-list" }, rows) : h("p", { class: "profile-empty" }, "接口没有返回其他信息"));
  } catch (e) {
    body.querySelector(".profile-loading")?.replaceWith(h("p", { class: "profile-error" }, friendlyError(e.message)));
  }
}

async function editRemark(c) {
  const hint = c.type === "group" ? "没有权限查群名时，可以给这个群起个名字，只在 WebUI 里显示" : "只在 WebUI 里显示";
  const v = await promptDialog(c.remark ? "修改备注" : "设置备注", hint, c.remark, chatTitle(c));
  if (v === null) return;
  try {
    const { chat } = await api("POST", "/api/chats/remark", { key: c.key, remark: v });
    onChat(chat);
    toast(v.trim() ? "备注已保存" : "备注已清除");
  } catch (e) {
    toast(e.message, "error");
  }
}

async function loadOlder() {
  const key = view.current;
  const first = view.messages[0];
  if (!first) return;
  view.loading = true;
  try {
    const r = await api("GET", `/api/chats/messages?key=${encodeURIComponent(key)}&before=${first.seq}&limit=${PAGE_SIZE}`);
    if (view?.current !== key) return;
    const oldHeight = view.scroller.scrollHeight;
    view.messages = r.messages.concat(view.messages);
    view.more = r.more;
    renderMessages();
    view.scroller.scrollTop = view.scroller.scrollHeight - oldHeight;
  } catch (e) {
    toast(e.message, "error");
  } finally {
    if (view) view.loading = false;
  }
}

// ---------- 消息列表 ----------

function messageCtx() {
  return {
    findReply(id) {
      const m = view.messages.find((x) => x.id === id);
      return m && { ...m, summary: summaryOf(m.message) };
    },
    nameOf(qq) {
      const m = view.messages.findLast((x) => String(x.user_id) === String(qq) && x.nickname);
      return m?.nickname;
    },
    nameOfOpenid(openid) {
      const m = view.messages.findLast((x) => x.user_openid === openid && x.nickname);
      return m?.nickname;
    },
    /** 按被引用消息的 msg_idx（REFIDX_...）找原消息 */
    findByRef(ref) {
      const key = "msg_idx=" + ref;
      return view.messages.findLast((x) => Array.isArray(x.event?.msg_scene_ext) && x.event.msg_scene_ext.includes(key));
    },
    /** 按正文找原消息（找不到 msg_idx 时用），只比对文字 */
    findByText(text) {
      const norm = (t) => stripFaces(t).replace(/<@[^>]+>/g, "").replace(/\s+/g, "");
      const want = norm(text ?? "");
      if (!want) return undefined;
      return view.messages.findLast((x) => norm(x.message.filter((s) => s.type === "text").map((s) => String(s.data?.text ?? "")).join("")) === want);
    },
    jumpTo(seq) {
      const el = view.scroller.querySelector(`.msg[data-seq="${seq}"]`);
      if (!el) return toast("被引用的消息不在已加载的记录里", "error");
      el.scrollIntoView({ block: "center", behavior: matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth" });
      el.classList.remove("flash");
      void el.offsetWidth;
      el.classList.add("flash");
    },
  };
}

function renderMessages() {
  const nodes = [];
  if (view.more) nodes.push(h("p", { class: "msgs-hint" }, "向上滚动加载更早的消息"));
  else if (view.messages.length) nodes.push(h("p", { class: "msgs-hint" }, "没有更早的消息了"));
  else nodes.push(h("p", { class: "msgs-hint" }, "这个会话还没有消息"));
  let prev = null;
  for (const m of view.messages) {
    if (!prev || dayLabel(prev.time) !== dayLabel(m.time)) nodes.push(h("div", { class: "day" }, h("span", null, dayLabel(m.time))));
    nodes.push(messageNode(m));
    prev = m;
  }
  fill(view.scroller, ...nodes);
}

function appendMessage(m, live) {
  const list = view.messages;
  const prev = list[list.length - 2];
  const nearBottom = view.scroller.scrollHeight - view.scroller.scrollTop - view.scroller.clientHeight < 120;
  view.scroller.querySelector(".msgs-hint:last-child")?.remove();
  if (!prev || dayLabel(prev.time) !== dayLabel(m.time)) view.scroller.append(h("div", { class: "day" }, h("span", null, dayLabel(m.time))));
  const node = messageNode(m);
  if (live) node.classList.add("arrive");
  view.scroller.append(node);
  if (nearBottom || m.self) view.scroller.scrollTop = view.scroller.scrollHeight;
}

/** 会话列表的预览：去掉表情标记，Markdown 链接只留文字 */
function preview(text) {
  return stripFaces(text).replace(/\[([^\]\n]*)\]\((?:https?|mqqapi):\/\/[^)\s]*\)/gi, "$1");
}

/** 消息气泡：只有图片（含表情图）时不加气泡，直接显示图片 */
function bubble(parts) {
  return h("div", { class: onlyImages(parts) ? "bubble bare" : "bubble" }, parts);
}

/** 两位、三位补零 */
const pad = (n, w = 2) => String(n).padStart(w, "0");

/** 收到消息的时间：年-月-日 时:分:秒.毫秒（旧记录只有秒） */
function fullTime(m) {
  const ms = Number(m.recv_ms) || 0;
  const d = new Date(ms || m.time * 1000);
  const base = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
  return ms ? `${base}.${pad(d.getMilliseconds(), 3)}` : base;
}

/** JSON 着色：键、字符串、数字、true/false/null 分别上色 */
function highlightJSON(text) {
  const out = [];
  const re = /("(?:\\.|[^"\\])*")(\s*:)?|\b(true|false|null)\b|(-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)/g;
  let last = 0;
  for (let m = re.exec(text); m; m = re.exec(text)) {
    if (m.index > last) out.push(text.slice(last, m.index));
    if (m[1]) {
      out.push(h("span", { class: m[2] ? "j-key" : "j-str" }, m[1]));
      if (m[2]) out.push(m[2]);
    } else if (m[3]) out.push(h("span", { class: "j-lit" }, m[3]));
    else out.push(h("span", { class: "j-num" }, m[4]));
    last = re.lastIndex;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    // 不是 https / 本机地址时浏览器不给用剪贴板接口，退回到选中复制
    const ta = h("textarea", { style: "position:fixed;left:-9999px" }, text);
    document.body.append(ta);
    ta.select();
    document.execCommand("copy");
    ta.remove();
  }
  toast("已复制");
}

/** 查看一条消息的原始数据（WebUI 收到的完整事件） */
function showRaw(m) {
  const data = m.event ?? (({ event, ...rest }) => rest)(m);
  const text = JSON.stringify(data, null, 2);
  openDialog("消息原始数据", h("div", { class: "raw-view" },
    h("div", { class: "raw-head" },
      h("span", null, "收到时间 ", h("b", null, fullTime(m))),
      m.id ? h("span", null, "消息编号 ", h("b", null, String(m.id))) : null,
      h("button", { class: "btn small", type: "button", onclick: () => copyText(text) }, "复制 JSON")),
    m.event ? null : h("p", { class: "raw-note" }, "这条消息是旧版 WebUI 保存的，没有留下完整事件，下面是当时保存的内容。"),
    h("pre", { class: "raw-json" }, highlightJSON(text))), { wide: true });
}

function messageNode(m) {
  const c = chatState.chats.get(view.current);
  const name = m.self ? chatState.botName : m.nickname || `用户 ${m.user_id}`;
  const role = m.role === "owner" ? "群主" : m.role === "admin" ? "管理员" : "";
  const openid = m.user_openid || (c?.type === "private" ? c.openid : "");
  const av = m.self
    ? monogram(chatState.botName, "self", "sm bot")
    : h("button", {
      class: "avatar-btn", type: "button", title: `查看 ${name} 的资料`,
      onclick: () => showUserInfo({ openid, userId: m.user_id, name: m.event?.sender?.nickname || m.nickname, card: m.event?.sender?.card || "", avatar: m.avatar, role: m.role, bot: m.bot }),
    }, avatar(openid, name, "p" + m.user_id, "sm", m.avatar));
  const open = () => showRaw(m);
  return h("div", { class: "msg" + (m.self ? " self" : ""), dataset: { seq: m.seq } },
    av,
    h("div", { class: "msg-main" },
      h("div", { class: "msg-meta" },
        h("b", null, name),
        role ? h("span", { class: "role" }, role) : null,
        h("button", { class: "msg-stamp", type: "button", title: "查看这条消息的原始数据", onclick: open }, fullTime(m))),
      bubble(renderSegments(m.message, { ...messageCtx(), bot: !!m.bot, event: m.event }))));
}

/** 路由：同一页里切换会话（不重建页面，保留搜索内容） */
export function selectChat(type, key) {
  if (!view || view.type !== type) return false;
  if (!key) {
    view.current = "";
    view.root.querySelector(".chat")?.classList.remove("has-current");
    renderEmptyPanel();
    renderList();
  } else if (key !== view.current && chatState.chats.has(key)) {
    openChat(key);
  }
  return true;
}
