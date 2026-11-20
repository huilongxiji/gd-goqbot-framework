// 私聊 / 群聊页面：左边会话列表，右边消息窗口。当前阶段只接收消息
import { h, fill, icon, api, toast, promptDialog, confirmDialog, openDialog, shortTime, dayLabel, hue } from "./core.js";
import { renderSegments, renderMarkdown, markdownSourceOf, summaryOf, setImageViewer, setForwardViewer, forwardLine, stripFaces, onlyImages } from "./msg.js";

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
    // 新消息会刷新被动回复凭证、机器人发消息会用掉次数：稍后重新查一下额度
    if (view.quota) refreshQuotaSoon();
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

/** 实时推送：旧版数据合并到了精确编号的会话，key 变了 */
export function onChatRenamed({ from, chat }) {
  if (!chat) return;
  chatState.chats.delete(from);
  chatState.chats.set(chat.key, chat);
  if (!view || view.type !== chat.type) return;
  if (view.current === from) location.replace(`#/${chat.type}/${encodeURIComponent(chat.key)}`);
  renderList();
}

/** 实时推送：会话被删除（这个页面或别的页面删的） */
export function onChatRemoved({ key }) {
  if (!chatState.chats.delete(key)) return;
  chatState.onUnreadChange();
  if (!view) return;
  if (view.current === key) location.hash = `#/${view.type}`;
  renderList();
}

// ---------- 会话列表的右键菜单 ----------

let closeMenu = () => {};

function openChatMenu(c, x, y) {
  openMenu(chatTitle(c), [
    { label: c.remark ? "修改备注" : "设置备注", run: () => editRemark(c) },
    c.unread ? { label: "标记为已读", run: () => markRead(c) } : null,
    { sep: true },
    { label: "删除聊天记录", danger: true, run: () => deleteChat(c) },
  ], x, y, view?.list);
}

/**
 * 右键菜单。items 每项 { label, run, danger?, disabled?, hint? } 或 { sep: true }；
 * disabled 的项显示成灰色，hint 是鼠标停留时的说明
 */
function openMenu(label, list, x, y, scroller) {
  closeMenu();
  const items = list.filter(Boolean);
  const menu = h("div", { class: "ctx-menu", role: "menu", "aria-label": label },
    items.map((it) => it.sep ? h("div", { class: "ctx-sep", role: "separator" }) : h("button", {
      class: "ctx-item" + (it.danger ? " danger" : ""), type: "button", role: "menuitem",
      disabled: !!it.disabled, title: it.hint || undefined,
      onclick: () => { closeMenu(); it.run(); },
    }, it.label, it.disabled && it.hint ? h("small", null, it.hint) : null)));
  document.body.append(menu);
  // 放在鼠标位置，超出窗口时往回收
  const r = menu.getBoundingClientRect();
  menu.style.left = Math.max(8, Math.min(x, innerWidth - r.width - 8)) + "px";
  menu.style.top = Math.max(8, Math.min(y, innerHeight - r.height - 8)) + "px";
  const buttons = [...menu.querySelectorAll("button:not(:disabled)")];
  buttons[0]?.focus();
  const onKey = (e) => {
    if (e.key === "Escape") { e.preventDefault(); closeMenu(); return; }
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      const i = buttons.indexOf(document.activeElement);
      buttons[(i + (e.key === "ArrowDown" ? 1 : buttons.length - 1)) % buttons.length].focus();
    }
  };
  const onDown = (e) => { if (!menu.contains(e.target)) closeMenu(); };
  const onAway = () => closeMenu();
  document.addEventListener("keydown", onKey, true);
  document.addEventListener("pointerdown", onDown, true);
  addEventListener("resize", onAway);
  addEventListener("blur", onAway);
  // 用户自己滚动列表时关掉（不用 scroll 事件：新消息自动滚到底时也会触发）
  scroller?.addEventListener("wheel", onAway, { once: true, passive: true });
  scroller?.addEventListener("touchmove", onAway, { once: true, passive: true });
  closeMenu = () => {
    closeMenu = () => {};
    menu.remove();
    document.removeEventListener("keydown", onKey, true);
    document.removeEventListener("pointerdown", onDown, true);
    removeEventListener("resize", onAway);
    removeEventListener("blur", onAway);
  };
}

function markRead(c) {
  c.unread = 0;
  chatState.onUnreadChange();
  renderList();
  api("POST", "/api/chats/read", { key: c.key }).catch(() => {});
}

async function deleteChat(c) {
  const who = c.type === "group" ? "这个群再有新消息时" : "这个用户再发消息时";
  const ok = await confirmDialog("删除聊天记录", `删除「${chatTitle(c)}」的全部聊天记录？删除后无法恢复，${who}会重新出现在列表里。`, "删除", true);
  if (!ok) return;
  try {
    await api("POST", "/api/chats/delete", { key: c.key });
    onChatRemoved({ key: c.key });
    toast("聊天记录已删除");
  } catch (e) {
    toast(e.message, "error");
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
  return () => { clearInterval(view?.quotaTimer); view = null; };
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
    h("a", {
      class: "chat-item" + (c.key === view.current ? " active" : ""), href: `#/${c.type}/${encodeURIComponent(c.key)}`, role: "listitem",
      title: "右键可以设置备注或删除聊天记录",
      oncontextmenu: (e) => { e.preventDefault(); const r = e.currentTarget.getBoundingClientRect(); openChatMenu(c, e.clientX || r.left + 24, e.clientY || r.bottom - 8); },
    },
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
  view.draft = { reply: null, mentions: [] };
  view.botRole = chat.type === "group" ? null : "";
  view.composer = renderComposer(chat);
  if (chat.type === "group") loadBotRole(key);
  clearInterval(view.quotaTimer);
  view.quota = renderQuotaCard();
  fill(view.panel, view.head, h("div", { class: "msgs-wrap" }, view.scroller, view.quota?.root), view.composer.root);
  if (view.quota) startQuota(key);
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
      const m = view.messages.find((x) => String(x.id) === String(id));
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
      // 整行底色闪一下，1.8 秒后去掉
      clearTimeout(el._flashTimer);
      el.classList.remove("flash");
      void el.offsetWidth;
      el.classList.add("flash");
      el._flashTimer = setTimeout(() => el.classList.remove("flash"), 1800);
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
  // 旧记录的卡片消息：「[卡片消息] 卡片名 摘要: [QQ小程序]标题 字段: …」
  const ark = /^\[卡片消息\]\s*(\S+)\s*(?:摘要:\s*(\[[^\]]*\])?(.*?)(?=\s+[A-Za-z_]+:|$))?/.exec(String(text ?? ""));
  if (ark) return ark[1] === "小程序" || ark[2] === "[QQ小程序]" ? `[QQ小程序] ${ark[3] ?? ""}`.trim() : `[卡片] ${ark[3] || ark[1]}`.trim();
  return stripFaces(text).replace(/\[([^\]\n]*)\]\((?:https?|mqqapi):\/\/[^)\s]*\)/gi, "$1");
}

/** 消息气泡：只有图片（含表情图）或聊天记录卡片时不加气泡，直接显示 */
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
  return h("div", {
    class: "msg" + (m.self ? " self" : ""), dataset: { seq: m.seq },
    oncontextmenu: (e) => {
      if (e.target.closest("a[href]")) return; // 链接保留浏览器自己的菜单
      e.preventDefault();
      openMessageMenu(m, e.clientX, e.clientY);
    },
  },
    av,
    h("div", { class: "msg-main" },
      h("div", { class: "msg-meta" },
        h("b", null, name),
        role ? h("span", { class: "role" }, role) : null,
        h("button", { class: "msg-stamp", type: "button", title: "查看这条消息的原始数据", onclick: open }, fullTime(m))),
      bubble(renderSegments(m.message, { ...messageCtx(), bot: !!m.bot, event: m.event }))));
}

// ---------- 消息额度（群聊右上角） ----------
// 数据来自 goqbot 的 get_message_quota：被动回复凭证（最近一条消息）还剩多久、已回复几次，今天已发的主动消息条数

const QUOTA_COLLAPSED = "webui.quota.collapsed";

function renderQuotaCard() {
  let collapsed = false;
  try { collapsed = localStorage.getItem(QUOTA_COLLAPSED) === "1"; } catch { /* 浏览器不让用本地存储时按展开处理 */ }
  const body = h("div", { class: "quota-body" }, h("p", { class: "quota-note" }, "正在查询…"));
  const mini = h("span", { class: "quota-mini" });
  const toggle = h("button", { class: "quota-toggle", type: "button" });
  const root = h("aside", { class: "quota-card", "aria-label": "消息额度" },
    h("div", { class: "quota-head" }, h("b", null, "消息额度"), mini, toggle), body);
  const apply = () => {
    root.classList.toggle("collapsed", collapsed);
    toggle.textContent = collapsed ? "展开" : "收起";
    toggle.setAttribute("aria-expanded", String(!collapsed));
  };
  toggle.addEventListener("click", () => {
    collapsed = !collapsed;
    try { localStorage.setItem(QUOTA_COLLAPSED, collapsed ? "1" : "0"); } catch { /* 忽略 */ }
    apply();
  });
  apply();
  return { root, body, mini, data: null, offset: 0 };
}

function startQuota(key) {
  loadQuota(key);
  let ticks = 0;
  view.quotaTimer = setInterval(() => {
    if (!view || view.current !== key || !view.quota) return;
    if (++ticks % 30 === 0) loadQuota(key); // 每 30 秒重新查一次
    paintQuota();
  }, 1000);
}

let quotaSoon;
function refreshQuotaSoon() {
  clearTimeout(quotaSoon);
  const key = view.current;
  quotaSoon = setTimeout(() => { if (view?.current === key) loadQuota(key); }, 800);
}

async function loadQuota(key) {
  try {
    const d = await api("GET", `/api/chats/quota?key=${encodeURIComponent(key)}`);
    if (view?.current !== key || !view.quota) return;
    view.quota.data = d;
    view.quota.offset = (Number(d.now) || Date.now() / 1000) - Date.now() / 1000;
    view.quota.error = "";
  } catch (e) {
    if (view?.current !== key || !view.quota) return;
    view.quota.error = /get_message_quota|未知|unknown/i.test(e.message) ? "当前 goqbot 版本查不到额度，请先更新 goqbot" : e.message;
  }
  paintQuota();
}

function mmss(sec) {
  const s = Math.max(0, Math.floor(sec));
  return `${String(Math.floor(s / 60)).padStart(2, "0")}:${String(s % 60).padStart(2, "0")}`;
}

function paintQuota() {
  const q = view.quota;
  if (!q) return;
  if (q.error && !q.data) {
    fill(q.body, h("p", { class: "quota-note warn" }, q.error));
    q.mini.textContent = "查不到";
    return;
  }
  const d = q.data;
  if (!d) return;
  const now = Date.now() / 1000 + q.offset;
  const p = d.passive ?? {};
  const a = d.active ?? {};
  const c2c = d.scene === "c2c";
  const left = (Number(p.expires_at) || 0) - now;
  const used = Number(p.replies_used) || 0;
  const limit = Number(p.replies_limit) || (c2c ? 4 : 5);
  const ttlMin = Math.round((Number(p.ttl) || (c2c ? 3600 : 300)) / 60);
  let passiveMain, passiveCls = "";
  if (!p.received_at) { passiveMain = "还没有可回复的消息"; passiveCls = "muted"; }
  else if (left <= 0) { passiveMain = "已过期"; passiveCls = "muted"; }
  else if (used >= limit) { passiveMain = "次数已用完"; passiveCls = "warn"; }
  else { passiveMain = `还剩 ${mmss(left)}`; passiveCls = left < 60 ? "warn" : "ok"; }
  const allow = d.allow_proactive_msg;
  const sent = a.sent_today ?? 0;
  const daily = a.daily_limit ?? 1000;
  // 私聊：没有主动消息额度，要对方打开机器人的主动消息开关才能发；每个好友每天最多收 1000 条
  const activeRow = c2c
    ? h("div", { class: "quota-row" },
      h("span", { class: "quota-label" }, "主动消息"),
      allow === true ? h("span", { class: "quota-value ok" }, "对方已开启主动消息")
        : allow === false ? h("span", { class: "quota-value warn" }, "对方关闭了主动消息")
          : h("span", { class: "quota-value muted" }, "不确定对方是否开启主动消息"),
      h("span", { class: "quota-sub" }, `今日已发 ${sent} 条，每个好友每天最多收 ${daily} 条`),
      h("span", { class: "quota-rule" }, "对方在机器人资料卡里打开主动消息开关后才能主动发"))
    : h("div", { class: "quota-row" },
      h("span", { class: "quota-label" }, "主动消息"),
      allow === false
        ? h("span", { class: "quota-value warn" }, "群主没有开启机器人主动发言")
        : h("span", { class: "quota-value" }, `剩余 ${a.remaining ?? "—"} 条`),
      h("span", { class: "quota-sub" }, `今日已发 ${sent} / ${daily} 条`));
  const rule = `有效期 ${ttlMin} 分钟，每条最多 ${limit} 次`;
  fill(q.body,
    h("div", { class: "quota-row" },
      h("span", { class: "quota-label" }, "被动回复"),
      h("span", { class: "quota-value " + passiveCls }, passiveMain),
      p.received_at ? h("span", { class: "quota-sub" }, `已回复 ${used} / ${limit} 次`) : null,
      h("span", { class: "quota-rule" }, c2c ? rule : `${p.boosted ? "开发者是群主或管理员：" : ""}${rule}，私聊回复发送者也算次数`)),
    activeRow,
    q.error ? h("p", { class: "quota-note warn" }, q.error) : null);
  // 收起时只显示一行：被动回复能用时显示剩余时间，否则显示主动消息的情况
  const passiveOk = p.received_at && left > 0 && used < limit;
  q.mini.textContent = passiveOk ? `被动回复还剩 ${mmss(left)}`
    : c2c ? (allow === false ? "对方关闭了主动消息" : `今日主动已发 ${sent} 条`)
      : allow === false ? "主动发言未开启" : `主动消息剩余 ${a.remaining ?? "—"} 条`;
}

// ---------- 输入框 ----------

/** 旧版 WebUI 存的会话编号丢了精度，不能发消息 */
function legacyChat(c) {
  return typeof c.id === "number" && !Number.isSafeInteger(c.id);
}

/** 这条消息发送者的精确编号；旧记录里不准时，用同一个人较新的消息补上 */
function exactUserId(m) {
  if (typeof m.user_id === "string" && m.user_id) return m.user_id;
  if (Number.isSafeInteger(m.user_id) && m.user_id) return String(m.user_id);
  if (m.user_openid) {
    const n = view.messages.findLast((x) => x.user_openid === m.user_openid && typeof x.user_id === "string" && x.user_id);
    if (n) return n.user_id;
  }
  return "";
}

/** 这条消息的精确编号；旧记录不准时为空 */
function exactMsgId(m) {
  if (typeof m.id === "string" && m.id) return m.id;
  return Number.isSafeInteger(m.id) && m.id ? String(m.id) : "";
}

function senderName(m) {
  return m.self ? chatState.botName : m.nickname || `用户 ${m.user_id}`;
}

function renderComposer(c) {
  const input = h("textarea", {
    rows: 1, maxlength: 2000, "aria-label": "输入消息",
    placeholder: c.type === "group" ? "输入消息，Enter 发送，Shift+Enter 换行" : "输入消息，Enter 发送，Shift+Enter 换行",
  });
  const sendBtn = h("button", { class: "btn primary", type: "button", onclick: () => sendDraft() }, "发送");
  const replyBar = h("div", { class: "composer-reply", hidden: true });
  // 输入框上方的快捷按钮：Markdown 消息、发本地图片
  const imgInput = h("input", { type: "file", accept: "image/png,image/jpeg,image/gif,image/webp,image/bmp", hidden: true,
    onchange: () => { const f = imgInput.files?.[0]; imgInput.value = ""; if (f) sendLocalImage(f, imgBtn); } });
  const mdBtn = h("button", { class: "tool-chip", type: "button", title: "用 Markdown 写一条消息", onclick: () => openMarkdownBuilder() }, icon("markdown"), "Markdown");
  const imgBtn = h("button", { class: "tool-chip", type: "button", title: "选一张本地图片发送", onclick: () => imgInput.click() }, icon("image"), h("span", null, "图片"));
  const tools = h("div", { class: "composer-tools" }, mdBtn, imgBtn, imgInput);
  const root = h("footer", { class: "composer" }, tools, replyBar, h("div", { class: "composer-row" }, input, sendBtn));
  const grow = () => { input.style.height = "auto"; input.style.height = Math.min(input.scrollHeight, 150) + "px"; };
  input.addEventListener("input", grow);
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing && e.keyCode !== 229) {
      e.preventDefault();
      sendDraft();
    }
  });
  if (legacyChat(c)) {
    input.disabled = true;
    sendBtn.disabled = true;
    mdBtn.disabled = true;
    imgBtn.disabled = true;
    input.placeholder = `这个会话是旧版 WebUI 保存的，编号不完整，等${c.type === "group" ? "这个群" : "对方"}发来新消息后就能发送了`;
  }
  return { root, input, sendBtn, replyBar, grow };
}

// ---------- 发本地图片 ----------

const IMAGE_LIMIT = 20 * 1024 * 1024;

async function sendLocalImage(file, btn) {
  if (file.size > IMAGE_LIMIT) return toast("图片不能超过 20 MB", "error");
  const key = view.current;
  const label = btn.querySelector("span");
  btn.disabled = true;
  label.textContent = "发送中…";
  try {
    await api("POST", `/api/chats/send-image?key=${encodeURIComponent(key)}`, file);
    if (view?.current === key && view.quota) refreshQuotaSoon();
  } catch (e) {
    toast(friendlyError(e.message), "error");
  } finally {
    btn.disabled = false;
    label.textContent = "图片";
  }
}

// ---------- Markdown 消息 ----------
// 弹窗只能用底部的「取消」「发送」关闭：点四周、按 Esc 都不会关，避免写了一半丢掉

const mdDrafts = new Map(); // 会话 key -> 没发出去的 Markdown 草稿
const mediaPreview = new Map(); // qqbot-media:// 链接 -> 预览图地址（只用于弹窗里的预览）

/** 预览用：把 @ 标记换成 @名字，把素材链接换成能显示的预览图 */
function previewSource(src) {
  return src
    .replace(/<qqbot-at-user\s+id="([^"]+)"\s*\/?>/g, (_, id) => "@" + (messageCtx().nameOfOpenid(id) || "某人"))
    .replace(/<qqbot-at-everyone\s*\/?>/g, "@全体成员")
    .replace(/\]\((qqbot-media:\/\/[^)\s]+)\)/g, (m, url) => mediaPreview.has(url) ? `](${mediaPreview.get(url)})` : m);
}

/** 读出图片尺寸，按最长边 400 等比缩小，作为 Markdown 图片的显示尺寸；src 是网址或本地文件 */
async function imageSize(src) {
  try {
    let w, hgt;
    if (typeof src === "string") {
      const img = new Image();
      img.referrerPolicy = "no-referrer";
      img.src = src;
      await img.decode();
      [w, hgt] = [img.naturalWidth, img.naturalHeight];
    } else {
      const bmp = await createImageBitmap(src); // 本地文件直接解码（页面的安全策略不允许 blob: 图片）
      [w, hgt] = [bmp.width, bmp.height];
      bmp.close();
    }
    const scale = Math.min(1, 400 / Math.max(w, hgt));
    return [Math.round(w * scale), Math.round(hgt * scale)];
  } catch {
    return [0, 0];
  }
}

function openMarkdownBuilder() {
  const key = view.current;
  const c = chatState.chats.get(key);
  if (!c) return;
  const input = h("textarea", { class: "md-input", spellcheck: "false", "aria-label": "Markdown 内容", placeholder: "在这里写 Markdown，右边会实时预览" });
  input.value = mdDrafts.get(key) ?? "";
  const preview = h("div", { class: "md-preview" });
  const ask = h("div", { class: "md-ask", hidden: true });
  const sendBtn = h("button", { class: "btn primary", type: "button" }, "发送");
  const cancelBtn = h("button", { class: "btn", type: "button" }, "取消");
  const update = () => {
    mdDrafts.set(key, input.value);
    const src = input.value.trim();
    fill(preview, src ? renderMarkdown(previewSource(input.value)) : h("p", { class: "md-empty" }, "预览会显示在这里"));
    sendBtn.disabled = !src;
  };
  input.addEventListener("input", update);

  // ---- 插入内容 ----
  const insert = (text, selectFrom = text.length, selectTo = selectFrom) => {
    const s = input.selectionStart, e = input.selectionEnd;
    input.setRangeText(text, s, e, "end");
    input.selectionStart = s + selectFrom;
    input.selectionEnd = s + selectTo;
    input.focus();
    update();
  };
  const lineStart = () => input.value.lastIndexOf("\n", input.selectionStart - 1) + 1;
  const heading = () => {
    const ls = lineStart();
    const line = input.value.slice(ls).split("\n")[0];
    const m = /^(#{1,5}) /.exec(line);
    // 已经是标题时再点一次变成下一级（# → ## → …），否则加 #
    input.setRangeText(m ? "#".repeat(m[1].length + 1) + " " : "# ", ls, ls + (m ? m[0].length : 0), "end");
    input.focus();
    update();
  };
  const quote = () => {
    const ls = lineStart();
    const end = input.selectionEnd;
    const block = input.value.slice(ls, end) || "";
    const quoted = (block || "").split("\n").map((l) => "> " + l).join("\n");
    input.setRangeText(quoted || "> ", ls, end, "end");
    input.focus();
    update();
  };
  const code = () => {
    const sel = input.value.slice(input.selectionStart, input.selectionEnd);
    const before = input.selectionStart > 0 && input.value[input.selectionStart - 1] !== "\n" ? "\n" : "";
    const text = `${before}\`\`\`\n${sel}\n\`\`\`\n`;
    insert(text, before.length + 4, before.length + 4 + sel.length);
  };

  /** 在编辑框上方弹出一行输入，返回填写的内容（取消时为 null） */
  const askInline = (label, placeholder, chips = []) => new Promise((resolve) => {
    const field = h("input", { type: "text", placeholder, spellcheck: "false" });
    const done = (v) => { ask.hidden = true; ask.replaceChildren(); input.focus(); resolve(v); };
    field.addEventListener("keydown", (e) => {
      if (e.key === "Enter") { e.preventDefault(); done(field.value.trim() || null); }
      if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); done(null); }
    });
    fill(ask,
      h("label", null, h("span", null, label), field),
      chips.length ? h("div", { class: "md-chips" }, chips.map(([text, value]) => h("button", { class: "md-chip", type: "button", title: value, onclick: () => done(value) }, text))) : null,
      h("div", { class: "md-ask-actions" },
        h("button", { class: "btn small", type: "button", onclick: () => done(null) }, "取消"),
        h("button", { class: "btn small primary", type: "button", onclick: () => done(field.value.trim() || null) }, "插入")));
    ask.hidden = false;
    field.focus();
  });

  const mention = async () => {
    // 最近在这个会话里说过话的人，点一下就能填上 TA 的 OpenID
    const seen = new Map();
    for (const m of [...view.messages].reverse()) {
      if (!m.self && m.user_openid && !seen.has(m.user_openid)) seen.set(m.user_openid, m.nickname || "用户");
      if (seen.size >= 12) break;
    }
    const id = await askInline("艾特用户：填写 TA 的 OpenID", "例如 0123456789ABCDEF0123456789ABCDEF", [...seen].map(([oid, name]) => [name, oid]));
    if (!id) return;
    if (!/^[0-9A-Za-z_-]{8,64}$/.test(id)) return toast("OpenID 格式不对", "error");
    insert(`<qqbot-at-user id="${id}" /> `);
  };

  const fileInput = h("input", { type: "file", accept: "image/png,image/jpeg,image/gif,image/webp,image/bmp", hidden: true });
  const localImage = () => fileInput.click();
  fileInput.addEventListener("change", async () => {
    const f = fileInput.files?.[0];
    fileInput.value = "";
    if (!f) return;
    if (f.size > IMAGE_LIMIT) return toast("图片不能超过 20 MB", "error");
    const placeholder = `![上传中…](uploading)`;
    insert(placeholder);
    const at = input.value.lastIndexOf(placeholder);
    try {
      const [w, hgt] = await imageSize(f);
      const r = await api("POST", `/api/media/upload?name=${encodeURIComponent(f.name)}`, f);
      if (r.preview_url) mediaPreview.set(r.url, r.preview_url);
      const md = `![图片${w ? ` #${w}px #${hgt}px` : ""}](${r.url})`;
      const i = input.value.indexOf(placeholder, Math.max(0, at - 50));
      if (i >= 0) input.setRangeText(md, i, i + placeholder.length, "end");
      update();
    } catch (e) {
      const i = input.value.indexOf(placeholder);
      if (i >= 0) input.setRangeText("", i, i + placeholder.length, "end");
      update();
      toast("上传图片失败：" + friendlyError(e.message), "error");
    }
  });
  const imageLink = async () => {
    const url = await askInline("图片链接", "https://example.com/a.png");
    if (!url) return;
    if (!/^https?:\/\//i.test(url)) return toast("请填写 http:// 或 https:// 开头的图片地址", "error");
    const [w, hgt] = await imageSize(url);
    insert(`![图片${w ? ` #${w}px #${hgt}px` : ""}](${url})`);
  };

  const tool = (name, label, hint, run) => h("button", { class: "md-tool", type: "button", title: hint, onclick: run }, icon(name), h("span", null, label));
  const dlg = h("dialog", { class: "md-modal", "aria-label": "Markdown 消息" },
    h("header", { class: "md-head" }, h("h2", null, "Markdown 消息"), h("span", null, `发到「${chatTitle(c)}」`)),
    h("div", { class: "md-main" },
      h("nav", { class: "md-tools", "aria-label": "快速插入" },
        tool("heading", "标题", "在这一行前面加 #，再点一次变成下一级标题", heading),
        tool("quote", "引用", "在这一行前面加 >", quote),
        tool("code", "代码块", "插入 ``` 代码块（选中文字时包起来）", code),
        c.type === "group" ? tool("at", "艾特", "艾特一个用户（需要填写 TA 的 OpenID）", mention) : null,
        tool("image", "本地图片", "上传一张本地图片并插入", localImage),
        tool("imglink", "图片链接", "用网址插入一张图片", imageLink),
        fileInput),
      h("div", { class: "md-edit" }, ask, input),
      h("div", { class: "md-preview-wrap" }, h("span", { class: "md-preview-label" }, "预览"), preview)),
    h("footer", { class: "md-foot" }, h("small", null, "只能点「取消」或「发送」关闭；没发出去的内容会留着，下次打开还在"), cancelBtn, sendBtn));
  // 按 Esc 不关闭；点四周也不会关闭（dialog 默认就不会）
  dlg.addEventListener("cancel", (e) => e.preventDefault());
  const close = () => { dlg.close(); dlg.remove(); view?.composer.input.focus(); };
  cancelBtn.addEventListener("click", close);
  sendBtn.addEventListener("click", async () => {
    const content = input.value.trim();
    if (!content) return;
    if (/\]\(uploading\)/.test(content)) return toast("图片还在上传，请稍等", "error");
    sendBtn.disabled = true;
    sendBtn.textContent = "发送中…";
    const ok = await sendMessage(key, [{ type: "markdown", data: { data: { markdown: { content } } } }]);
    sendBtn.textContent = "发送";
    sendBtn.disabled = false;
    if (ok) {
      mdDrafts.delete(key);
      close();
      toast("Markdown 消息已发送");
    }
  });
  document.body.append(dlg);
  dlg.showModal();
  update();
  input.focus();
}

function renderReplyBar() {
  const { replyBar } = view.composer;
  const r = view.draft.reply;
  replyBar.hidden = !r;
  if (!r) return replyBar.replaceChildren();
  fill(replyBar,
    h("span", null, "回复 ", h("b", null, senderName(r)), "：", summaryOf(r.message) || "[消息]"),
    h("button", { class: "composer-reply-x", type: "button", "aria-label": "取消回复", title: "取消回复", onclick: () => { view.draft.reply = null; renderReplyBar(); view.composer.input.focus(); } }, "×"));
}

/** 在输入框光标处插入 @名字 */
function mentionUser(m) {
  const userId = exactUserId(m);
  const name = senderName(m).replace(/\s+/g, " ").trim();
  if (!userId || !name) return;
  const { input, grow } = view.composer;
  if (!view.draft.mentions.some((x) => x.name === name)) view.draft.mentions.push({ name, userId });
  const at = `@${name} `;
  const start = input.selectionStart ?? input.value.length;
  const end = input.selectionEnd ?? start;
  input.value = input.value.slice(0, start) + at + input.value.slice(end);
  input.focus();
  input.selectionStart = input.selectionEnd = start + at.length;
  grow();
}

function replyTo(m) {
  view.draft.reply = m;
  renderReplyBar();
  view.composer.input.focus();
}

/** 把输入框里的文字和 @ 拼成消息段 */
function buildDraft(text) {
  const segs = [];
  const r = view.draft.reply;
  if (r) segs.push({ type: "reply", data: { id: exactMsgId(r) } });
  const mentions = view.draft.mentions.filter((x) => text.includes("@" + x.name)).sort((a, b) => b.name.length - a.name.length);
  if (!mentions.length) {
    if (text) segs.push({ type: "text", data: { text } });
    return segs;
  }
  const esc = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const re = new RegExp(`@(${mentions.map((x) => esc(x.name)).join("|")})`, "g");
  let last = 0;
  for (const mt of text.matchAll(re)) {
    if (mt.index > last) segs.push({ type: "text", data: { text: text.slice(last, mt.index) } });
    const who = mentions.find((x) => x.name === mt[1]);
    segs.push({ type: "at", data: { qq: who.userId, name: who.name } });
    last = mt.index + mt[0].length;
  }
  if (last < text.length) segs.push({ type: "text", data: { text: text.slice(last) } });
  return segs;
}

async function sendDraft() {
  const { input, sendBtn, grow } = view.composer;
  const text = input.value.replace(/\s+$/, "");
  if (!text.trim() || sendBtn.disabled) return;
  const key = view.current;
  sendBtn.disabled = true;
  try {
    await sendMessage(key, buildDraft(text));
    if (view?.current !== key) return;
    input.value = "";
    grow();
    view.draft = { reply: null, mentions: [] };
    renderReplyBar();
  } finally {
    if (view?.current === key) { sendBtn.disabled = false; input.focus(); }
  }
}

/** 发出一条消息；成功后它会以 message_sent 事件回到聊天记录里 */
async function sendMessage(key, message) {
  try {
    await api("POST", "/api/chats/send", { key, message });
    if (view?.current === key && view.quota) refreshQuotaSoon();
    return true;
  } catch (e) {
    toast(friendlyError(e.message), "error");
    return false;
  }
}

// ---------- 消息的右键菜单 ----------

/** 复读：把这条消息转成能再发一次的消息段；不能复读时返回 null */
function repeatSegments(m) {
  if (m.message.some((s) => ["forward", "file", "share", "card", "json", "xml", "ark", "keyboard"].includes(s.type))) return null;
  if (m.message.length === 1 && m.message[0].type === "text" && /^\[卡片消息\]/.test(m.message[0].data?.text ?? "")) return null;
  const out = [];
  for (const s of m.message) {
    const d = s.data ?? {};
    if (s.type === "text") {
      const t = stripFaces(d.text);
      if (t.trim()) out.push({ type: "text", data: { text: t } });
    } else if (s.type === "at") {
      if (typeof d.qq === "string" && /^\d+$/.test(d.qq) || d.qq === "all") out.push({ type: "at", data: { qq: d.qq } });
    } else if (s.type === "image" || s.type === "record" || s.type === "video") {
      const url = /^https?:\/\//i.test(d.url ?? "") ? d.url : /^https?:\/\//i.test(d.file ?? "") ? d.file : "";
      if (url) out.push({ type: s.type, data: { file: url } });
    } else if (s.type === "markdown" || s.type === "face") {
      out.push({ type: s.type, data: d });
    }
  }
  return out.length ? out : null;
}

/** 查机器人在这个群里的身份：owner / admin / member；查不到时为 "unknown" */
async function loadBotRole(key) {
  try {
    const r = await api("GET", `/api/chats/bot-role?key=${encodeURIComponent(key)}`);
    if (view?.current === key) view.botRole = r.role || "unknown";
  } catch {
    if (view?.current === key) view.botRole = "unknown";
  }
}

/**
 * 机器人能不能管这个人（禁言、撤回 TA 的消息）。
 * 机器人要是群主或管理员；不能管群主；机器人是管理员时不能管其他管理员
 */
function canManage(m) {
  const role = view.botRole;
  if (role === "owner" || role === "admin") {
    if (m.role === "owner") return { ok: false, hint: "不能对群主操作" };
    if (m.role === "admin" && role !== "owner") return { ok: false, hint: "机器人是管理员，不能对其他管理员操作" };
    return { ok: true, hint: "" };
  }
  if (role === "member") return { ok: false, hint: "机器人不是群主或管理员，没有这个权限" };
  // 查不到机器人的身份（接口没有权限或还在查询）：不拦着，失败时会提示原因
  return { ok: true, hint: role === null ? "正在查询机器人在群里的身份" : "查不到机器人在群里的身份，操作可能会因为没有权限而失败" };
}

function openMessageMenu(m, x, y) {
  const c = chatState.chats.get(view.current);
  if (!c) return;
  const group = c.type === "group";
  const legacy = legacyChat(c);
  const userId = m.self ? "" : exactUserId(m);
  const msgId = exactMsgId(m);
  const repeat = repeatSegments(m);
  const name = senderName(m);
  const oldHint = "这条是旧版 WebUI 保存的记录，编号不完整，不能操作";
  const manage = group && !m.self ? canManage(m) : { ok: true, hint: "" };
  const mdSource = markdownSourceOf(m.message, !!m.bot);
  const copyable = mdSource || messageText(m).text;
  // 机器人自己的消息只能在发出后 2 分钟内撤回
  const sentAt = Number(m.recv_ms) || m.time * 1000;
  const tooOld = m.self && Date.now() - sentAt > 2 * 60_000;
  openMenu("消息操作", [
    group && !m.self ? { label: `艾特 ${name}`, run: () => mentionUser(m), disabled: !userId || legacy, hint: !userId ? oldHint : "" } : null,
    { label: "回复", run: () => replyTo(m), disabled: !msgId || legacy, hint: !msgId ? oldHint : "" },
    group && !m.self && !m.bot ? { label: `私聊回复 ${name}…`, run: () => privateReply(c, m, userId, msgId), disabled: !msgId || !userId || legacy,
      hint: !msgId || !userId ? oldHint : "群消息发出后 60 分钟内可以私聊回复 TA，对方要加了机器人好友" } : null,
    { label: "复读", run: () => repeatMessage(m, repeat), disabled: !repeat || legacy, hint: !repeat ? "这类消息不能复读" : "" },
    { label: mdSource ? "复制到 Markdown 编辑框" : "复制到输入框", run: () => copyToComposer(m, mdSource), disabled: !copyable || legacy, hint: !copyable ? "这条消息没有可以复制的文字" : "" },
    mdSource ? { label: "复制 Markdown 源码", run: () => copyText(mdSource) } : null,
    { sep: true },
    group && !m.self ? { label: `禁言 ${name}…`, run: () => banUser(c, m, userId), disabled: !userId || legacy || !manage.ok, hint: !userId ? oldHint : manage.hint } : null,
    { label: "撤回消息", danger: true, run: () => recallMessage(m, msgId), disabled: !msgId || (!group && !m.self) || !manage.ok || tooOld,
      hint: !msgId ? oldHint : tooOld ? "发出超过 2 分钟，不能撤回了" : !group && !m.self ? "私聊里只能撤回机器人自己发的消息" : manage.hint },
    { sep: true },
    { label: "查看原始数据", run: () => showRaw(m) },
  ], x, y, view.scroller);
}

/** 消息里的文字（表情标记换成说明文字）和其中 @ 的人 */
function messageText(m) {
  let text = "";
  const mentions = [];
  for (const s of m.message) {
    const d = s.data ?? {};
    if (s.type === "text") text += stripFaces(d.text);
    else if (s.type === "at" && d.qq !== "all") {
      const name = String(d.name || messageCtx().nameOf(d.qq) || d.qq);
      text += "@" + name;
      if (typeof d.qq === "string" && /^\d+$/.test(d.qq)) mentions.push({ name, userId: d.qq });
    }
  }
  return { text: text.replace(/^\s+/, ""), mentions };
}

/** 把消息内容放进输入框（Markdown 消息放进 Markdown 编辑框），可以改了再发 */
function copyToComposer(m, mdSource) {
  if (mdSource) {
    const prev = mdDrafts.get(view.current) ?? "";
    mdDrafts.set(view.current, prev ? prev + "\n\n" + mdSource : mdSource);
    openMarkdownBuilder();
    return;
  }
  const { text, mentions } = messageText(m);
  const { input, grow } = view.composer;
  for (const x of mentions) if (!view.draft.mentions.some((y) => y.name === x.name)) view.draft.mentions.push(x);
  const s = input.selectionStart ?? input.value.length;
  input.setRangeText(text, s, input.selectionEnd ?? s, "end");
  input.focus();
  grow();
}

async function repeatMessage(m, segs) {
  if (await sendMessage(view.current, segs)) toast("已复读");
}

async function recallMessage(m, msgId) {
  const ok = await confirmDialog("撤回消息", `撤回 ${senderName(m)} 的这条消息？`, "撤回", true);
  if (!ok) return;
  try {
    await api("POST", "/api/chats/recall", { message_id: msgId });
    toast("已撤回");
    // 在这条消息的名字旁标上「已撤回」（只在当前页面，刷新后不保留）
    const row = view?.scroller.querySelector(`.msg[data-seq="${m.seq}"]`);
    if (row && !row.classList.contains("recalled")) {
      row.classList.add("recalled");
      row.querySelector(".msg-meta")?.append(h("span", { class: "recalled-tag" }, "已撤回"));
    }
  } catch (e) {
    toast(friendlyError(e.message), "error");
  }
}

function banUser(c, m, userId) {
  const name = senderName(m);
  const choices = [["10 分钟", 600], ["1 小时", 3600], ["12 小时", 43200], ["1 天", 86400], ["7 天", 604800]];
  let close;
  const run = async (sec, label) => {
    close();
    try {
      await api("POST", "/api/chats/ban", { key: c.key, user_id: userId, duration: sec });
      toast(sec ? `已禁言 ${name} ${label}` : `已解除 ${name} 的禁言`);
    } catch (e) {
      toast(friendlyError(e.message), "error");
    }
  };
  close = openDialog(`禁言 ${name}`, h("div", { class: "ban-pick" },
    h("p", null, "选择禁言时长。"),
    h("div", { class: "ban-choices" }, choices.map(([label, sec]) => h("button", { class: "btn", type: "button", onclick: () => run(sec, label) }, label))),
    h("button", { class: "btn ghost small", type: "button", onclick: () => run(0, "") }, "解除禁言")));
}

/** 群消息私聊回复：用这条群消息的 msg_id 私聊回复发送者（60 分钟内有效，次数按群消息规则，和群内回复共用） */
function privateReply(c, m, userId, msgId) {
  const name = senderName(m);
  const input = h("textarea", { rows: 4, maxlength: 2000, placeholder: "要私聊发给 TA 的内容" });
  const sendBtn = h("button", { class: "btn primary", type: "submit" }, "发送");
  let close;
  const form = h("form", {
    class: "confirm private-reply",
    onsubmit: async (e) => {
      e.preventDefault();
      const text = input.value.trim();
      if (!text) return input.focus();
      sendBtn.disabled = true;
      try {
        await api("POST", "/api/chats/private-reply", { key: c.key, user_id: userId, message_id: msgId, text });
        close();
        toast(`已私聊回复 ${name}`);
        if (view?.current === c.key && view.quota) refreshQuotaSoon();
      } catch (err) {
        sendBtn.disabled = false;
        toast(privateReplyError(err.message), "error");
      }
    },
  },
  h("p", { class: "private-reply-quote" }, h("b", null, name), "：", summaryOf(m.message) || "[消息]"),
  h("label", { class: "field" }, h("span", null, "私聊内容"), input),
  h("small", { class: "muted" }, "对方要加了机器人好友才能收到。群消息发出后 60 分钟内可以私聊回复，回复次数和群里的回复共用。"),
  h("div", { class: "actions" },
    h("button", { class: "btn", type: "button", onclick: () => close() }, "取消"),
    sendBtn));
  close = openDialog(`私聊回复 ${name}`, form);
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) form.requestSubmit();
  });
  input.focus();
}

function privateReplyError(msg) {
  const s = String(msg ?? "");
  if (/1460|被动凭证|passive/i.test(s)) return "这条群消息已经超过 60 分钟或回复次数用完了，不能再私聊回复";
  if (/好友|friend/i.test(s)) return "对方还不是机器人的好友，不能私聊回复";
  return friendlyError(s);
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
