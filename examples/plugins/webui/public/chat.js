// 私聊 / 群聊页面：左边会话列表，右边消息窗口。当前阶段只接收消息
import { h, fill, icon, api, toast, promptDialog, openDialog, shortTime, clock, dayLabel, hue } from "./core.js";
import { renderSegments, summaryOf, setImageViewer } from "./msg.js";

const PAGE_SIZE = 50;

/** 页面共享的状态 */
export const chatState = {
  appId: "",
  botName: "机器人",
  chats: new Map(), // key -> chat
  onUnreadChange: () => {},
};

let view = null; // 当前打开的聊天页 { type, root, current, messages, ... }

setImageViewer((url) => {
  openDialog("查看图片", h("div", { class: "viewer" },
    h("img", { src: url, alt: "图片", referrerpolicy: "no-referrer" }),
    h("a", { class: "btn small", href: url, target: "_blank", rel: "noopener noreferrer" }, "在新标签页打开")), { wide: true });
});

// ---------- 名称与头像 ----------

export function chatTitle(c) {
  if (c.remark) return c.remark;
  if (c.type === "private") return c.name || `用户 ${c.id}`;
  return `群 ${c.id}`;
}

/** 首字色块：没有头像时用，颜色按编号固定 */
function monogram(text, seed, cls) {
  const t = cls.includes("digits") ? String(text) : [...String(text || "?")][0];
  const el = h("span", { class: "avatar mono " + cls, "aria-hidden": "true" }, t);
  el.style.setProperty("--h", hue(seed));
  return el;
}

function avatar(openid, fallbackText, seed, cls = "") {
  const fallback = monogram(fallbackText, seed, cls);
  if (!openid || !chatState.appId) return fallback;
  const img = h("img", {
    class: "avatar " + cls, alt: "", loading: "lazy", referrerpolicy: "no-referrer",
    src: `https://q.qlogo.cn/qqapp/${encodeURIComponent(chatState.appId)}/${encodeURIComponent(openid)}/100`,
  });
  img.addEventListener("error", () => img.replaceWith(fallback), { once: true });
  return img;
}

function chatAvatar(c, cls) {
  if (c.type === "group") {
    // 没有备注时显示群编号的后两位，方便区分
    return monogram(c.remark ? c.remark : String(c.id).slice(-2), "g" + c.id, cls + " square" + (c.remark ? "" : " digits"));
  }
  return avatar(c.openid, chatTitle(c), "p" + c.id, cls);
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
          h("span", { class: "chat-item-text" }, c.type === "group" && c.last_sender ? `${c.last_sender}：${c.last_text}` : c.last_sender === "我" ? `我：${c.last_text}` : c.last_text),
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
    h("div", { class: "panel-title" }, h("h2", null, chatTitle(c)), h("p", null, sub)),
    h("button", { class: "btn ghost small", type: "button", onclick: () => editRemark(c) }, icon("edit"), c.remark ? "修改备注" : "设置备注"));
}

async function editRemark(c) {
  const hint = c.type === "group" ? "官方接口不提供群名，可以给这个群起个名字，只在 WebUI 里显示" : "只在 WebUI 里显示";
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
  };
}

/** 同一个人 3 分钟内连续发的消息合在一起，不重复显示头像和名字 */
function continues(prev, m) {
  return prev && prev.self === m.self && prev.user_id === m.user_id && m.time - prev.time < 180 && dayLabel(prev.time) === dayLabel(m.time);
}

function renderMessages() {
  const nodes = [];
  if (view.more) nodes.push(h("p", { class: "msgs-hint" }, "向上滚动加载更早的消息"));
  else if (view.messages.length) nodes.push(h("p", { class: "msgs-hint" }, "没有更早的消息了"));
  else nodes.push(h("p", { class: "msgs-hint" }, "这个会话还没有消息"));
  let prev = null;
  for (const m of view.messages) {
    if (!prev || dayLabel(prev.time) !== dayLabel(m.time)) nodes.push(h("div", { class: "day" }, h("span", null, dayLabel(m.time))));
    nodes.push(messageNode(m, continues(prev, m)));
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
  const node = messageNode(m, continues(prev, m));
  if (live) node.classList.add("arrive");
  view.scroller.append(node);
  if (nearBottom || m.self) view.scroller.scrollTop = view.scroller.scrollHeight;
}

function messageNode(m, cont) {
  const c = chatState.chats.get(view.current);
  const name = m.self ? chatState.botName : m.nickname || `用户 ${m.user_id}`;
  const role = m.role === "owner" ? "群主" : m.role === "admin" ? "管理员" : "";
  const av = m.self
    ? monogram(chatState.botName, "self", "sm bot")
    : avatar(m.user_openid || (c?.type === "private" ? c.openid : ""), name, "p" + m.user_id, "sm");
  return h("div", { class: "msg" + (m.self ? " self" : "") + (cont ? " cont" : ""), dataset: { seq: m.seq } },
    cont ? h("span", { class: "avatar-gap" }) : av,
    h("div", { class: "msg-main" },
      cont ? null : h("div", { class: "msg-meta" },
        h("b", null, name),
        role ? h("span", { class: "role" }, role) : null,
        c?.type === "group" && !m.self ? h("span", { class: "uid" }, String(m.user_id)) : null),
      h("div", { class: "bubble" }, renderSegments(m.message, messageCtx())),
      h("time", { class: "msg-time", title: new Date(m.time * 1000).toLocaleString("zh-CN") }, clock(m.time))));
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
