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

/** 行内：图片、链接、粗体、斜体、删除线、行内代码 */
function inline(text) {
  const out = [];
  const re = /!\[([^\]]*)\]\(([^)\s]+)\)|\[([^\]]+)\]\(([^)\s]+)\)|`([^`]+)`|\*\*([^*]+)\*\*|__([^_]+)__|~~([^~]+)~~|\*([^*]+)\*|_([^_]+)_/g;
  let last = 0;
  for (let m = re.exec(text); m; m = re.exec(text)) {
    if (m.index > last) out.push(text.slice(last, m.index));
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
  if (last < text.length) out.push(text.slice(last));
  return out;
}

export function renderMarkdown(src) {
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

function mediaImage(url, w = 0, hgt = 0) {
  const img = h("img", { src: url, loading: "lazy", alt: "图片", referrerpolicy: "no-referrer" });
  if (w && hgt) {
    img.width = Math.min(w, 320);
    img.height = Math.round((img.width / w) * hgt);
  }
  img.addEventListener("error", () => img.replaceWith(h("span", { class: "seg-tag" }, "[图片已过期]")), { once: true });
  return h("button", { class: "seg-image", type: "button", onclick: () => openViewer(url) }, img);
}

/**
 * 渲染一条消息的全部段。
 * ctx.findReply(id) 返回被回复的消息（找不到时为空）；ctx.nameOf(qq) 返回 @ 的显示名
 */
export function renderSegments(segs, ctx) {
  const parts = [];
  for (const s of segs) {
    const d = s.data ?? {};
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
        const r = ctx.findReply(Number(d.id));
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
        if (md.content) parts.push(renderMarkdown(md.content));
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
        parts.push(h("span", { class: "seg-tag" }, "[聊天记录]"));
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

/** 渲染结果是否只有图片（只有图片时不套气泡） */
export function onlyImages(parts) {
  return parts.length > 0 && parts.every((el) => el.classList?.contains("seg-image"));
}

/** 一行摘要（回复引用里用） */
export function summaryOf(segs) {
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
    else if (s.type !== "reply") t += `[${s.type}]`;
  }
  t = t.replace(/\s+/g, " ").trim();
  return t.length > 40 ? t.slice(0, 40) + "…" : t;
}
