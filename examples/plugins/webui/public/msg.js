// 消息段渲染：文本、@、图片、语音、视频、文件、回复、Markdown、按钮等
import { h } from "./core.js";

/** 只放行 http(s) 地址 */
function safeUrl(u) {
  const s = String(u ?? "");
  return /^https?:\/\//i.test(s) ? s : "";
}

// ---------- QQ 表情标记 ----------
// 官方接口把表情写在文本里：<faceType=6,faceId="0",ext="base64(JSON)">。
// ext 解出来是 {"text":"[呲牙]"} 这样的说明；大表情（faceType=6）的 text 为空，图片另在 image 段里
const FACE_RE = /<faceType=([^,>]*)(?:,faceId="([^"]*)")?(?:,ext="([^"]*)")?[^>]*>/g;

function faceText(ext) {
  if (!ext) return "";
  try {
    const bin = atob(ext);
    const o = JSON.parse(new TextDecoder().decode(Uint8Array.from(bin, (c) => c.charCodeAt(0))));
    return typeof o?.text === "string" ? o.text : "";
  } catch { return ""; }
}

/** 把文本拆成普通文字和表情：返回 [字符串 | { face: 说明文字 }] */
function splitFaces(text) {
  const out = [];
  let last = 0;
  for (const m of text.matchAll(FACE_RE)) {
    if (m.index > last) out.push(text.slice(last, m.index));
    out.push({ face: faceText(m[3]) });
    last = m.index + m[0].length;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

/** 去掉表情标记，换成表情的说明文字（没有说明的大表情直接去掉） */
export function stripFaces(text) {
  return String(text ?? "").replace(FACE_RE, (_, _t, _id, ext) => faceText(ext));
}

/** 取出 markdown 段的正文和按钮。data.data 可以是对象、JSON 字符串或 base64://JSON */
export function markdownOf(data) {
  let inner = data.data;
  try {
    if (typeof inner === "string") {
      if (inner.startsWith("base64://")) {
        const bin = atob(inner.slice(9));
        inner = new TextDecoder().decode(Uint8Array.from(bin, (c) => c.charCodeAt(0)));
      }
      inner = JSON.parse(inner);
    }
  } catch { inner = undefined; }
  const o = inner && typeof inner === "object" ? inner : {};
  const content = typeof o.markdown?.content === "string" ? o.markdown.content : typeof data.content === "string" ? data.content : "";
  const rows = o.keyboard?.content?.rows ?? o.rows ?? [];
  const template = o.markdown && !content && (o.markdown.custom_template_id || o.markdown.template_id);
  return { content, rows: Array.isArray(rows) ? rows : [], template };
}

// ---------- Markdown：只支持 QQ 官方 markdown 常用的写法 ----------

// 其他机器人发的 Markdown 会以纯文字的形式收到，里面夹着 @。
// 渲染前把 @ 换成占位符 \uE000序号\uE001，渲染时再换回 @ 标签
let mdTokens = [];

function withTokens(text) {
  if (!mdTokens.length || !text.includes("\uE000")) return [text];
  return text.split(/\uE000(\d+)\uE001/).map((p, i) => (i % 2 ? mdTokens[Number(p)] ?? "" : p)).filter((p) => p !== "");
}

/** 行内：图片、链接、粗体、斜体、删除线、行内代码 */
function inline(text) {
  const out = [];
  const re = /!\[([^\]]*)\]\(([^)\s]+)\)|\[([^\]]+)\]\(([^)\s]+)\)|`([^`]+)`|\*\*([^*]+)\*\*|__([^_]+)__|~~([^~]+)~~|\*([^*]+)\*|_([^_]+)_/g;
  let last = 0;
  for (let m = re.exec(text); m; m = re.exec(text)) {
    if (m.index > last) out.push(...withTokens(text.slice(last, m.index)));
    if (m[2] !== undefined) {
      const url = safeUrl(m[2]);
      // QQ 的写法：![描述 #200px #100px](地址)
      const size = /#(\d+)px\s+#(\d+)px/.exec(m[1]);
      out.push(url ? mediaImage(url, size ? +size[1] : 0, size ? +size[2] : 0) : `[图片]`);
    } else if (m[4] !== undefined) {
      const url = safeUrl(m[4]);
      out.push(url ? h("a", { href: url, target: "_blank", rel: "noopener noreferrer" }, m[3]) : h("span", { class: "md-cmd" }, m[3]));
    } else if (m[5] !== undefined) out.push(h("code", null, m[5]));
    else if (m[6] !== undefined || m[7] !== undefined) out.push(h("strong", null, inline(m[6] ?? m[7])));
    else if (m[8] !== undefined) out.push(h("del", null, inline(m[8])));
    else out.push(h("em", null, inline(m[9] ?? m[10])));
    last = re.lastIndex;
  }
  if (last < text.length) out.push(...withTokens(text.slice(last)));
  return out;
}

export function renderMarkdown(src, tokens = []) {
  mdTokens = tokens;
  try {
    return renderMarkdownInner(src);
  } finally {
    mdTokens = [];
  }
}

function renderMarkdownInner(src) {
  const root = h("div", { class: "md" });
  const lines = src.replace(/\r\n?/g, "\n").split("\n");
  let list = null, para = [];
  const flush = () => {
    if (para.length) {
      const p = h("p");
      para.forEach((l, i) => { if (i) p.append(h("br")); p.append(...inline(l)); });
      root.append(p);
      para = [];
    }
  };
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (line.startsWith("```")) {
      flush(); list = null;
      const code = [];
      for (i++; i < lines.length && !lines[i].startsWith("```"); i++) code.push(lines[i]);
      root.append(h("pre", null, h("code", null, code.join("\n"))));
      continue;
    }
    let m;
    if ((m = /^(#{1,6})\s+(.*)$/.exec(line))) {
      flush(); list = null;
      root.append(h("h" + Math.min(m[1].length + 2, 6), null, inline(m[2])));
    } else if ((m = /^>\s?(.*)$/.exec(line))) {
      flush(); list = null;
      root.append(h("blockquote", null, inline(m[1])));
    } else if ((m = /^\s*(?:[-*+]|(\d+)\.)\s+(.*)$/.exec(line))) {
      flush();
      const tag = m[1] ? "ol" : "ul";
      if (!list || list.tagName.toLowerCase() !== tag) { list = h(tag); root.append(list); }
      list.append(h("li", null, inline(m[2])));
    } else if (/^\s*(\*\*\*|---|___)\s*$/.test(line)) {
      flush(); list = null;
      root.append(h("hr"));
    } else if (!line.trim()) {
      flush(); list = null;
    } else {
      list = null;
      para.push(line);
    }
  }
  flush();
  return root;
}

/** 按钮：只显示，不能点 */
function renderKeyboard(rows) {
  const box = h("div", { class: "kb" });
  for (const row of rows) {
    const btns = Array.isArray(row?.buttons) ? row.buttons : [];
    if (!btns.length) continue;
    box.append(h("div", { class: "kb-row" }, btns.map((b) =>
      h("span", { class: "kb-btn", title: String(b?.action?.data ?? "") }, String(b?.render_data?.label ?? b?.render_data?.visited_label ?? "按钮")))));
  }
  return box.childElementCount ? box : null;
}

// ---------- 媒体 ----------

let openViewer = () => {};
/** 由页面注入：点图片时放大查看 */
export function setImageViewer(fn) { openViewer = fn; }

let openForward = () => {};
/** 由页面注入：点聊天记录卡片时展开全部消息 */
export function setForwardViewer(fn) { openForward = fn; }

// 聊天里图片的最大显示尺寸：按长宽比缩放到这个框里，不裁剪、不拉伸
const IMG_MAX_W = 320;
const IMG_MAX_H = 360;

/** 按长宽比缩放到最大框内，返回显示尺寸 */
function fitSize(w, hgt) {
  const scale = Math.min(IMG_MAX_W / w, IMG_MAX_H / hgt, 1);
  return [Math.max(1, Math.round(w * scale)), Math.max(1, Math.round(hgt * scale))];
}

function mediaImage(url, w = 0, hgt = 0) {
  const img = h("img", { src: url, loading: "lazy", alt: "图片", referrerpolicy: "no-referrer" });
  const btn = h("button", { class: "seg-image", type: "button", title: "查看大图", onclick: () => openViewer(url) }, img);
  const apply = (nw, nh) => {
    [img.width, img.height] = fitSize(nw, nh);
    btn.classList.toggle("long", nh / nw > 2.5); // 长图：标一下，点开看全图
  };
  // 消息里带了尺寸时先按它占位，加载完再按图片的真实尺寸校正（消息里的尺寸可能不准）
  if (w > 0 && hgt > 0) apply(w, hgt);
  img.addEventListener("load", () => { if (img.naturalWidth && img.naturalHeight) apply(img.naturalWidth, img.naturalHeight); }, { once: true });
  img.addEventListener("error", () => {
    // 图片打不开时换成文字，并恢复气泡（只有图片的消息原本不带气泡）
    btn.closest(".bubble")?.classList.remove("bare");
    btn.replaceWith(h("span", { class: "seg-tag" }, "[图片已过期]"));
  }, { once: true });
  return btn;
}

/**
 * 渲染一条消息的全部段。
 * ctx.findReply(id) 返回被回复的消息（找不到时为空）；ctx.nameOf(qq) 返回 @ 的显示名
 */
export function renderSegments(segs, ctx) {
  const ark = arkOf(segs, ctx.event);
  if (ark) return [renderArk(ark)];
  const parts = [];
  const quote = quoteOf(segs, ctx.event);
  if (quote) parts.push(renderQuote(quote, ctx));
  const asMd = textMarkdown(segs, ctx);
  let mdPlaced = false;
  for (const s of segs) {
    const d = s.data ?? {};
    if (asMd && (s.type === "text" || s.type === "at" || s.type === "face")) {
      // 文字和 @ 合成一段 Markdown，放在第一段文字的位置
      if (!mdPlaced) { parts.push(renderMarkdown(asMd.src, asMd.tokens)); mdPlaced = true; }
      continue;
    }
    switch (s.type) {
      case "text": {
        const text = String(d.text ?? "");
        if (!text.includes("<faceType=")) {
          if (text !== "") parts.push(h("span", { class: "seg-text" }, text));
          break;
        }
        // 带表情标记：普通文字照常显示，表情显示说明文字，只剩空白的部分不显示
        for (const p of splitFaces(text)) {
          if (typeof p === "string") {
            if (p.trim()) parts.push(h("span", { class: "seg-text" }, p));
          } else if (p.face) {
            parts.push(h("span", { class: "seg-face", title: "QQ 表情" }, p.face));
          }
        }
        break;
      }
      case "at":
        parts.push(h("span", { class: "seg-at" }, "@" + (d.qq === "all" ? "全体成员" : d.name || ctx.nameOf(d.qq) || d.qq)));
        break;
      case "reply": {
        const r = ctx.findReply(d.id);
        parts.unshift(h("div", { class: "seg-reply" },
          r ? [h("b", null, r.self ? "机器人" : r.nickname || "用户 " + r.user_id), "：", r.summary] : "回复了一条更早的消息"));
        break;
      }
      case "image": {
        const url = safeUrl(d.url) || safeUrl(d.file);
        parts.push(url ? mediaImage(url, Number(d.width) || 0, Number(d.height) || 0) : h("span", { class: "seg-tag" }, "[图片]"));
        break;
      }
      case "record": case "voice": case "audio": {
        const url = safeUrl(d.url) || safeUrl(d.file);
        parts.push(url ? h("audio", { class: "seg-audio", controls: true, preload: "none", src: url }) : h("span", { class: "seg-tag" }, "[语音]"));
        break;
      }
      case "video": {
        const url = safeUrl(d.url) || safeUrl(d.file);
        parts.push(url ? h("video", { class: "seg-video", controls: true, preload: "metadata", src: url }) : h("span", { class: "seg-tag" }, "[视频]"));
        break;
      }
      case "file": {
        const url = safeUrl(d.url) || safeUrl(d.file);
        const name = String(d.name || d.file_name || "文件");
        parts.push(h(url ? "a" : "span", { class: "seg-file", href: url || undefined, target: "_blank", rel: "noopener noreferrer" }, h("b", null, name), h("small", null, url ? "点击下载" : "文件")));
        break;
      }
      case "markdown": {
        const md = markdownOf(d);
        // <@openid> / <qqbot-at-user id="…" /> 显示成 @名字
        if (md.content) parts.push(renderMarkdown(mentionText(md.content, ctx)));
        else parts.push(h("span", { class: "seg-tag" }, md.template ? "[模板 Markdown]" : "[Markdown]"));
        const kb = renderKeyboard(md.rows);
        if (kb) parts.push(kb);
        break;
      }
      case "keyboard": {
        const kb = renderKeyboard(d.content?.rows ?? d.rows ?? []);
        if (kb) parts.push(kb);
        break;
      }
      case "share": case "card": {
        const url = safeUrl(d.url);
        const pic = safeUrl(d.image) || safeUrl(d.pic_url);
        parts.push(h(url ? "a" : "div", { class: "seg-card", href: url || undefined, target: "_blank", rel: "noopener noreferrer" },
          pic ? h("img", { src: pic, alt: "", loading: "lazy", referrerpolicy: "no-referrer" }) : null,
          h("div", null, h("b", null, String(d.title ?? "分享")), h("small", null, String(d.content ?? d.description ?? "")))));
        break;
      }
      case "forward":
        // 引用消息被 goqbot 转成了 forward 段，已经在上面按引用显示
        if (!quote) parts.push(renderForward(d, ctx));
        break;
      case "face":
        parts.push(h("span", { class: "seg-face", title: "QQ 表情" }, d.text ? String(d.text) : "[表情]"));
        break;
      case "stream":
        if (d.content) parts.push(h("span", { class: "seg-text" }, String(d.content)));
        break;
      default:
        parts.push(h("span", { class: "seg-tag" }, `[${s.type}]`));
    }
  }
  return parts.length ? parts : [h("span", { class: "seg-tag" }, "[空消息]")];
}

// ---------- 引用消息（QQ message_type=103） ----------
// 被引用的是普通消息时，msg_elements[0] 带着它的 msg_idx（REFIDX_...）、正文和附件；
// 被引用的消息本身也是引用时，content 是「=== 消息 1 ===」格式的摘要，[关联消息] 后面是更早的引用，不显示。

const IMAGE_FILE_RE = /\.(?:jpe?g|png|gif|webp|bmp)$/i;

function httpsUrl(u) {
  const s = String(u ?? "");
  if (/^https?:\/\//i.test(s)) return s;
  return s && !s.includes("://") ? "https://" + s : "";
}

function attachmentImages(atts) {
  return (Array.isArray(atts) ? atts : [])
    .filter((a) => a && (/^image\//i.test(a.content_type ?? "") || IMAGE_FILE_RE.test(a.filename ?? "")))
    .map((a) => httpsUrl(a.url))
    .filter(Boolean);
}

/** 解析「=== 消息 1 ===」摘要里的第一条：发送者、正文、图片附件 */
function parseQuoteSummary(content) {
  const lines = String(content).replace(/\r/g, "").split("\n");
  const start = lines.findIndex((l) => /^=== 消息 \d+ ===/.test(l.trim()));
  const out = { sender: "", text: "", images: [] };
  let inText = false;
  for (const line of lines.slice(start + 1)) {
    if (/^=== 消息 \d+ ===/.test(line.trim())) break;
    if (/^\s/.test(line) || line.startsWith("[关联消息]") || line.startsWith("---")) { inText = false; continue; }
    let m;
    if ((m = /^\[消息内容\]\s?(.*)$/.exec(line))) { out.text = m[1]; inText = true; }
    else if ((m = /^\[发送者\]\s*(.*)$/.exec(line))) { out.sender = m[1].trim(); inText = false; }
    else if ((m = /^\[附件\d+\]\s*类型:(\S+).*?URL:(\S+)/.exec(line))) {
      if (/图/.test(m[1])) out.images.push(httpsUrl(m[2]));
      inText = false;
    } else if (line.startsWith("[")) inText = false;
    else if (inText) out.text += "\n" + line;
  }
  return out;
}

/** 旧记录里由引用转成的 forward 段：节点文字带「[消息类型] 引用消息」 */
function quoteNodeText(seg) {
  const node = Array.isArray(seg?.data?.content) ? seg.data.content[0] : null;
  const segs = Array.isArray(node?.data?.content) ? node.data.content : [];
  const text = segs.filter((s) => s.type === "text").map((s) => String(s.data?.text ?? "")).join("");
  return /\[消息类型\] 引用消息/.test(text) ? { text, segs } : null;
}

function isQuoteForward(seg) {
  return !!quoteNodeText(seg);
}

/** 取出这条消息引用的内容；不是引用消息时返回 null */
function quoteOf(segs, ev) {
  if (ev && Number(ev.qq_message_type) === 103 && Array.isArray(ev.msg_elements)) {
    const el = ev.msg_elements.find((e) => e && (e.content || (Array.isArray(e.attachments) && e.attachments.length)));
    if (el) {
      if (/^\s*=== 消息 \d+ ===/.test(el.content ?? "")) return parseQuoteSummary(el.content);
      return { sender: "", text: String(el.content ?? ""), images: attachmentImages(el.attachments), refIdx: String(el.msg_idx ?? "") };
    }
  }
  const fwd = segs.find((s) => s.type === "forward" && isQuoteForward(s));
  if (fwd) {
    const { text, segs: nodeSegs } = quoteNodeText(fwd);
    const nested = text.includes("[关联消息]");
    const images = nested ? [] : nodeSegs.filter((s) => s.type === "image").map((s) => safeUrl(s.data?.url) || safeUrl(s.data?.file)).filter(Boolean);
    return { sender: "", text: text.split("\n[消息类型]")[0], images };
  }
  return null;
}

/** <@openid> 换成 @名字 */
function mentionText(text, ctx) {
  return text
    .replace(/<@!?([0-9A-Za-z_-]+)>/g, (_, id) => "@" + (ctx.nameOfOpenid?.(id) || "某人"))
    .replace(/<qqbot-at-user\s+[^>]*?id=["']?([^"'\s>/]+)["']?[^>]*>/g, (_, id) => "@" + (ctx.nameOfOpenid?.(id) || "某人"))
    .replace(/<@all>|<qqbot-at-everyone\s*\/?>/g, "@全体成员");
}

function renderQuote(q, ctx) {
  // 找到被引用的原消息时，显示原消息的发送者，点引用可以跳过去
  const orig = (q.refIdx && ctx.findByRef?.(q.refIdx)) || ctx.findByText?.(q.text) || null;
  const sender = q.sender || (orig ? (orig.self ? "机器人" : orig.nickname || "") : "");
  let text = mentionText(stripFaces(q.text), ctx).trim();
  if (!text && !q.images.length) text = /<faceType=6\b/.test(q.text) ? "[表情]" : orig ? summaryOf(orig.message) : "[消息]";
  const imgs = q.images.length ? h("span", { class: "quote-imgs" }, q.images.slice(0, 4).map((url) => {
    const img = h("img", { src: url, alt: "图片", loading: "lazy", referrerpolicy: "no-referrer" });
    const btn = h("button", { class: "quote-img", type: "button", title: "查看大图", onclick: (e) => { e.stopPropagation(); openViewer(url); } }, img);
    img.addEventListener("error", () => btn.replaceWith(h("span", { class: "seg-tag" }, "[图片已过期]")), { once: true });
    return btn;
  })) : null;
  const box = h(orig ? "button" : "div", {
    class: "seg-reply quote" + (orig ? " jump" : ""), type: orig ? "button" : undefined,
    title: orig ? "跳到被引用的消息" : undefined, onclick: orig ? () => ctx.jumpTo?.(orig.seq) : undefined,
  }, h("b", null, sender || "引用"), text ? h("span", { class: "quote-text" }, text) : null, imgs);
  return box;
}

// ---------- 聊天记录（合并转发） ----------

function forwardNodes(d) {
  const list = Array.isArray(d.content) ? d.content : Array.isArray(d.message) ? d.message : [];
  return list.filter((n) => n && typeof n === "object").map((n) => n.data ?? n);
}

/** 节点的发送者和一行摘要 */
export function forwardLine(nd) {
  const name = String(nd.nickname ?? nd.name ?? "").trim();
  const content = Array.isArray(nd.content) ? summaryOf(nd.content) : stripFaces(String(nd.content ?? ""));
  return { name, content: content || "[消息]" };
}

function renderForward(d, ctx) {
  const nodes = forwardNodes(d);
  if (!nodes.length) return h("span", { class: "seg-tag" }, "[聊天记录]");
  const title = String(ctx.event?.forward_title || "聊天记录");
  return h("button", { class: "seg-forward", type: "button", onclick: () => openForward(title, nodes) },
    h("b", null, title),
    nodes.slice(0, 4).map((nd) => {
      const { name, content } = forwardLine(nd);
      return h("span", null, name ? `${name}：${content}` : content);
    }),
    h("small", null, `查看 ${nodes.length} 条消息`));
}

/** 看起来是 Markdown：有链接、引用、标题、粗体、代码块或图片写法 */
function looksLikeMarkdown(src) {
  return /\[[^\]\n]+\]\((?:https?|mqqapi):\/\/[^)\s]+\)/i.test(src)
    || /!\[[^\]\n]*\]\(https?:\/\/[^)\s]+\)/i.test(src)
    || /^\s{0,3}>\s?\S/m.test(src)
    || /^#{1,6}\s+\S/m.test(src)
    || /\*\*[^*\n]+\*\*/.test(src)
    || /^```/m.test(src);
}

/**
 * 其他机器人发的 Markdown 消息，官方接口只给出 Markdown 原文，goqbot 转成了文字段和 @ 段。
 * 文字看起来是 Markdown 时，把文字和 @ 合起来按 Markdown 渲染；返回 null 表示按普通文字显示
 */
function textMarkdown(segs, ctx) {
  if (segs.some((s) => s.type === "markdown")) return null;
  let src = "";
  const tokens = [];
  for (const s of segs) {
    const d = s.data ?? {};
    if (s.type === "text") src += stripFaces(d.text);
    else if (s.type === "face") src += d.text ? String(d.text) : "";
    else if (s.type === "at") {
      tokens.push(h("span", { class: "seg-at" }, "@" + (d.qq === "all" ? "全体成员" : d.name || ctx.nameOf(d.qq) || d.qq)));
      src += `\uE000${tokens.length - 1}\uE001`;
    }
  }
  // 机器人发的文字一律按 Markdown 显示；其他人的文字看起来像 Markdown 时才按 Markdown 显示
  if (!src.trim()) return null;
  return ctx.bot || looksLikeMarkdown(src) ? { src, tokens } : null;
}

/** 渲染结果是否只有图片或聊天记录卡片（这时不套气泡，它们自己就是一块） */
export function onlyImages(parts) {
  return parts.length > 0 && parts.every((el) => ["seg-image", "seg-forward", "seg-ark"].some((c) => el.classList?.contains(c)));
}

// ---------- 卡片消息（QQ 类型 3，ark） ----------
// 官方接口把卡片写成文字推过来：第一行「[卡片消息] 卡片名」，然后「摘要: …」和「字段名: 值」。
// QQ 小程序卡片只简单说明是什么；机器人能发的卡片（图文H5 等）照常显示成卡片。

/** 解析卡片：优先用事件里的 ark_data，没有时解析文字；不是卡片消息时返回 null */
export function arkOf(segs, ev) {
  if (ev?.ark_data && typeof ev.ark_data === "object") {
    const a = ev.ark_data;
    return { name: String(a.ark_name ?? ""), type: String(a.ark_type ?? ""), prompt: String(a.prompt ?? ""), fields: a.fields && typeof a.fields === "object" ? a.fields : {} };
  }
  if (segs.length !== 1 || segs[0].type !== "text") return null;
  const text = String(segs[0].data?.text ?? "");
  const head = /^\[卡片消息\][ \t]*(.*)/.exec(text);
  if (!head) return null;
  const out = { name: head[1].trim(), type: "", prompt: "", fields: {} };
  let last = "";
  for (const line of text.split("\n").slice(1)) {
    const m = /^(摘要|[A-Za-z_][\w]*):[ \t]?(.*)$/.exec(line);
    if (m) {
      last = m[1];
      if (last === "摘要") out.prompt = m[2];
      else out.fields[last] = m[2];
    } else if (last && last !== "摘要") out.fields[last] += "\n" + line;
    else if (last === "摘要") out.prompt += "\n" + line;
  }
  return out;
}

export function isMiniapp(ark) {
  return ark.type === "miniapp" || ark.name === "小程序" || /^\[QQ小程序\]/.test(ark.prompt);
}

/** 卡片的一行说明（会话列表、引用里用） */
export function arkSummary(ark) {
  const f = ark.fields;
  const title = String(f.title ?? "").trim() || ark.prompt.replace(/^\[[^\]]*\]/, "").trim();
  if (isMiniapp(ark)) return `[QQ小程序] ${title}`.trim();
  return `[卡片] ${title || ark.name}`.trim();
}

function renderArk(ark) {
  const f = ark.fields;
  const str = (k) => String(f[k] ?? "").trim();
  if (isMiniapp(ark)) {
    // QQ 小程序：只说明是哪个小程序分享了什么
    const source = str("source");
    const title = str("title") || ark.prompt.replace(/^\[QQ小程序\]/, "").trim();
    return h("span", { class: "seg-ark-brief" }, h("span", { class: "ark-label" }, "QQ小程序"), source ? `${source}：${title}` : title || "小程序分享");
  }
  const link = safeUrl(f.jump_url) || safeUrl(f.url) || safeUrl(f.link);
  const img = ["img", "image", "cover", "pic", "preview", "thumb"].map((k) => safeUrl(f[k])).find(Boolean);
  const title = str("title") || ark.prompt.replace(/^\[[^\]]*\]/, "").trim() || ark.name || "卡片";
  const desc = str("desc") || str("description") || str("summary");
  const tag = str("tag") || str("source") || ark.name;
  return h(link ? "a" : "div", { class: "seg-ark", href: link || undefined, target: link ? "_blank" : undefined, rel: link ? "noopener noreferrer" : undefined, title: link || undefined },
    img ? h("img", { src: img, alt: "", loading: "lazy", referrerpolicy: "no-referrer" }) : null,
    h("span", { class: "ark-body" },
      h("b", null, title),
      desc ? h("span", null, desc) : null,
      tag ? h("small", null, tag) : null));
}

/** 一行摘要（回复引用里用） */
export function summaryOf(segs) {
  const ark = arkOf(segs);
  if (ark) return arkSummary(ark);
  let t = "";
  let sticker = false; // 大表情标记后面紧跟的图片就是这个表情
  for (const s of segs) {
    const d = s.data ?? {};
    const wasSticker = sticker;
    sticker = s.type === "text" && /<faceType=6\b/.test(String(d.text ?? ""));
    if (s.type === "text") t += stripFaces(d.text);
    else if (s.type === "at") t += "@" + (d.name || d.qq);
    else if (s.type === "image") t += wasSticker ? "[表情]" : "[图片]";
    else if (s.type === "face") t += d.text || "[表情]";
    else if (s.type === "markdown") t += markdownOf(d).content.slice(0, 40) || "[Markdown]";
    else if (s.type === "forward") t += isQuoteForward(s) ? "" : "[聊天记录]";
    else if (s.type !== "reply") t += `[${s.type}]`;
  }
  t = t.replace(/\s+/g, " ").trim();
  return t.length > 40 ? t.slice(0, 40) + "…" : t;
}
