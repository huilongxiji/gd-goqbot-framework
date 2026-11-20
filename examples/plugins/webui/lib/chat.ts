// 聊天记录：收到的消息和机器人发出的消息按会话保存。
// 私聊以用户为单位（private:<user_id>），群聊以群为单位（group:<group_id>）。
//
// 文件都在 plugin-data/webui/chat/ 下：
//   chats.json          会话列表（名称、备注、最后一条消息、未读数）
//   <会话>.jsonl         这个会话的消息，一行一条，只追加；超过上限时裁掉最早的一半
import { appendFile, mkdir, readFile, rename, stat, writeFile } from "node:fs/promises";
import { join } from "node:path";
import type { OneBotEvent } from "../../goqbot.js";

export type ChatType = "private" | "group";

/** 消息段，和 OneBot v11 的数组格式一致 */
export interface Segment {
  type: string;
  data: Record<string, unknown>;
}

/** 保存下来的一条消息 */
export interface ChatMessage {
  seq: number; // 会话内的序号，从 1 开始递增
  id: number; // OneBot message_id
  time: number; // 秒
  recv_ms?: number; // WebUI 收到这条消息的时间（毫秒），旧记录没有
  self: boolean; // 机器人自己发的
  user_id: number;
  user_openid?: string;
  nickname: string;
  role?: string;
  message: Segment[];
  raw: string;
  event?: OneBotEvent; // 收到的完整事件，供页面查看原始数据；旧记录没有
}

/** 会话列表里的一项 */
export interface Chat {
  key: string; // private:<id> / group:<id>
  type: ChatType;
  id: number;
  name: string; // 私聊为对方昵称；群聊官方接口不提供群名，为空
  remark: string; // 自己设置的备注
  openid?: string; // 私聊对方的 user_openid / 群的 group_openid
  last_time: number;
  last_text: string;
  last_sender: string;
  unread: number;
  seq: number; // 最后一条消息的序号
}

const FILE_LIMIT = 8 * 1024 * 1024; // 单个会话文件超过 8 MB 时裁掉最早的一半

/** 把 CQ 码字符串转成消息段 */
export function parseCQ(text: string): Segment[] {
  const unescape = (s: string) => s.replace(/&#91;/g, "[").replace(/&#93;/g, "]").replace(/&#44;/g, ",").replace(/&amp;/g, "&");
  const out: Segment[] = [];
  const re = /\[CQ:([a-zA-Z0-9_.-]+)((?:,[^\]]*)?)\]/g;
  let last = 0;
  for (let m = re.exec(text); m; m = re.exec(text)) {
    if (m.index > last) {
      out.push({ type: "text", data: { text: unescape(text.slice(last, m.index)) } });
    }
    const data: Record<string, unknown> = {};
    for (const kv of m[2].split(",").slice(1)) {
      const i = kv.indexOf("=");
      if (i > 0) {
        data[kv.slice(0, i)] = unescape(kv.slice(i + 1));
      }
    }
    out.push({ type: m[1], data });
    last = re.lastIndex;
  }
  if (last < text.length) {
    out.push({ type: "text", data: { text: unescape(text.slice(last)) } });
  }
  return out;
}

/** 统一成消息段数组 */
function toSegments(message: unknown, raw: string): Segment[] {
  if (Array.isArray(message)) {
    return message
      .filter((s): s is { type: string; data?: Record<string, unknown> } => !!s && typeof s === "object" && typeof (s as Segment).type === "string")
      .map((s) => ({ type: s.type, data: s.data && typeof s.data === "object" ? s.data : {} }));
  }
  if (typeof message === "string") {
    return parseCQ(message);
  }
  return raw ? parseCQ(raw) : [];
}

/** 取出 markdown 段的正文和按钮行。data.data 可以是对象、JSON 字符串或 base64://JSON */
export function markdownOf(data: Record<string, unknown>): { content: string; rows: unknown[] } {
  let inner: unknown = data.data;
  try {
    if (typeof inner === "string") {
      inner = JSON.parse(inner.startsWith("base64://") ? Buffer.from(inner.slice(9), "base64").toString("utf8") : inner);
    }
  } catch {
    inner = undefined;
  }
  const o = (inner && typeof inner === "object" ? inner : {}) as Record<string, any>;
  const content = typeof o.markdown?.content === "string" ? o.markdown.content : typeof data.content === "string" ? data.content : "";
  const rows = o.keyboard?.content?.rows ?? o.rows ?? [];
  return { content, rows: Array.isArray(rows) ? rows : [] };
}

// QQ 表情标记：<faceType=6,faceId="0",ext="base64(JSON)">，ext 里的 text 是表情的说明（大表情为空）
const FACE_RE = /<faceType=([^,>]*)(?:,faceId="([^"]*)")?(?:,ext="([^"]*)")?[^>]*>/g;

/** 去掉表情标记，换成表情的说明文字 */
function stripFaces(text: string): string {
  return text.replace(FACE_RE, (_m, _t, _id, ext?: string) => {
    if (!ext) return "";
    try {
      const o = JSON.parse(Buffer.from(ext, "base64").toString("utf8"));
      return typeof o?.text === "string" ? o.text : "";
    } catch {
      return "";
    }
  });
}

/** 会话列表里显示的一行摘要 */
export function summary(segs: Segment[]): string {
  const names: Record<string, string> = {
    image: "[图片]", face: "[表情]", record: "[语音]", video: "[视频]", file: "[文件]",
    markdown: "[Markdown]", keyboard: "[按钮]", forward: "[聊天记录]", share: "[分享]", ark: "[卡片]", json: "[卡片]",
  };
  let text = "";
  let sticker = false; // 刚出现过大表情标记：紧跟的图片就是这个表情
  for (const s of segs) {
    if (s.type === "image" && sticker) {
      text += "[表情]";
      sticker = false;
      continue;
    }
    sticker = false;
    if (s.type === "text") {
      const raw = String(s.data.text ?? "");
      text += stripFaces(raw);
      sticker = /<faceType=6\b/.test(raw);
    } else if (s.type === "at") {
      text += "@" + String(s.data.name ?? s.data.qq ?? "");
    } else if (s.type === "face" && s.data.text) {
      text += String(s.data.text);
    } else if (s.type === "reply") {
      continue;
    } else if (s.type === "markdown") {
      const c = markdownOf(s.data).content;
      text += c ? c.replace(/!\[[^\]]*\]\([^)]*\)/g, "[图片]").replace(/\[([^\]]*)\]\([^)]*\)/g, "$1").replace(/[#*_`>]/g, "").slice(0, 60) : names.markdown;
    } else {
      text += names[s.type] ?? `[${s.type}]`;
    }
    if (text.length > 80) {
      break;
    }
  }
  // 其他机器人发的 Markdown 以文字形式收到，链接只留文字
  text = text.replace(/\[([^\]\n]*)\]\((?:https?|mqqapi):\/\/[^)\s]*\)/gi, "$1");
  return text.replace(/\s+/g, " ").trim().slice(0, 80);
}

export class ChatStore {
  readonly dir: string;
  private chats = new Map<string, Chat>();
  private saveTimer: NodeJS.Timeout | undefined;
  private writing = new Map<string, Promise<void>>(); // 每个会话的写入按顺序进行
  private listeners = new Set<(ev: { chat: Chat; message: ChatMessage }) => void>();

  constructor(dataDir: string) {
    this.dir = join(dataDir, "chat");
  }

  async load(): Promise<void> {
    await mkdir(this.dir, { recursive: true });
    try {
      const list = JSON.parse(await readFile(join(this.dir, "chats.json"), "utf8")) as Chat[];
      for (const c of list) {
        if (c && typeof c.key === "string") {
          this.chats.set(c.key, c);
        }
      }
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code !== "ENOENT") {
        throw new Error(`会话列表 ${join(this.dir, "chats.json")} 无法读取：${(e as Error).message}`);
      }
    }
  }

  /** 有新消息时通知（给页面的实时推送用） */
  subscribe(fn: (ev: { chat: Chat; message: ChatMessage }) => void): () => void {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  list(type?: ChatType): Chat[] {
    const all = [...this.chats.values()].filter((c) => !type || c.type === type);
    return all.sort((a, b) => b.last_time - a.last_time || b.seq - a.seq);
  }

  get(key: string): Chat | undefined {
    return this.chats.get(key);
  }

  /** 处理 message.* 和 message_sent.* 事件 */
  async add(ev: OneBotEvent): Promise<void> {
    const sent = ev.post_type === "message_sent";
    const type = ev.message_type === "group" ? "group" : ev.message_type === "private" ? "private" : undefined;
    if (!type || (ev.post_type !== "message" && !sent)) {
      return;
    }
    const num = (v: unknown) => (typeof v === "number" ? v : Number(v) || 0);
    const peer = type === "group" ? num(ev.group_id) : sent ? num(ev.target_id) : num(ev.user_id);
    if (!peer) {
      return;
    }
    const key = `${type}:${peer}`;
    const sender = (ev.sender && typeof ev.sender === "object" ? ev.sender : {}) as Record<string, unknown>;
    const raw = typeof ev.raw_message === "string" ? ev.raw_message : "";
    const segs = toSegments(ev.message, raw);
    const nickname = String(sender.card || sender.nickname || "");

    let chat = this.chats.get(key);
    if (!chat) {
      chat = { key, type, id: peer, name: "", remark: "", last_time: 0, last_text: "", last_sender: "", unread: 0, seq: 0 };
      this.chats.set(key, chat);
    }
    const openid = type === "group" ? ev.group_openid : sent ? undefined : ev.user_openid;
    if (typeof openid === "string" && openid) {
      chat.openid = openid;
    }
    if (type === "private" && !sent && nickname) {
      chat.name = nickname;
    }
    const msg: ChatMessage = {
      seq: ++chat.seq,
      id: num(ev.message_id),
      time: num(ev.time) || Math.floor(Date.now() / 1000),
      recv_ms: Date.now(),
      self: sent,
      user_id: num(ev.user_id),
      nickname,
      message: segs,
      raw,
      event: ev,
    };
    if (typeof ev.user_openid === "string" && ev.user_openid && !sent) {
      msg.user_openid = ev.user_openid;
    }
    if (typeof sender.role === "string" && sender.role) {
      msg.role = sender.role;
    }
    chat.last_time = msg.time;
    chat.last_text = summary(segs);
    chat.last_sender = sent ? "我" : nickname;
    if (!sent) {
      chat.unread++;
    }
    this.scheduleSave();
    await this.append(key, msg);
    for (const fn of this.listeners) {
      try {
        fn({ chat, message: msg });
      } catch {
        // 页面连接出错不影响保存
      }
    }
  }

  private fileOf(key: string): string {
    return join(this.dir, key.replace(":", "-") + ".jsonl");
  }

  private append(key: string, msg: ChatMessage): Promise<void> {
    const prev = this.writing.get(key) ?? Promise.resolve();
    const next = prev.then(async () => {
      const file = this.fileOf(key);
      await appendFile(file, JSON.stringify(msg) + "\n");
      const st = await stat(file);
      if (st.size > FILE_LIMIT) {
        const lines = (await readFile(file, "utf8")).split("\n").filter(Boolean);
        const keep = lines.slice(Math.floor(lines.length / 2));
        await writeFile(file + ".tmp", keep.join("\n") + "\n");
        await rename(file + ".tmp", file);
      }
    });
    const done = next.catch(() => {});
    this.writing.set(key, done);
    return next;
  }

  /** 读取消息：before 为序号，返回比它早的最多 limit 条（按时间正序） */
  async messages(key: string, before: number, limit: number): Promise<{ messages: ChatMessage[]; more: boolean }> {
    if (!this.chats.has(key)) {
      return { messages: [], more: false };
    }
    await this.writing.get(key);
    let text: string;
    try {
      text = await readFile(this.fileOf(key), "utf8");
    } catch {
      return { messages: [], more: false };
    }
    const all: ChatMessage[] = [];
    for (const line of text.split("\n")) {
      if (!line) {
        continue;
      }
      try {
        const m = JSON.parse(line) as ChatMessage;
        if (!before || m.seq < before) {
          all.push(m);
        }
      } catch {
        // 跳过写坏的行
      }
    }
    return { messages: all.slice(-limit), more: all.length > limit };
  }

  markRead(key: string): void {
    const chat = this.chats.get(key);
    if (chat && chat.unread) {
      chat.unread = 0;
      this.scheduleSave();
    }
  }

  setRemark(key: string, remark: string): Chat | undefined {
    const chat = this.chats.get(key);
    if (chat) {
      chat.remark = remark.trim().slice(0, 40);
      this.scheduleSave();
    }
    return chat;
  }

  private scheduleSave(): void {
    if (this.saveTimer) {
      return;
    }
    this.saveTimer = setTimeout(() => {
      this.saveTimer = undefined;
      void this.save();
    }, 1000);
  }

  async save(): Promise<void> {
    if (this.saveTimer) {
      clearTimeout(this.saveTimer);
      this.saveTimer = undefined;
    }
    const file = join(this.dir, "chats.json");
    await writeFile(file + ".tmp", JSON.stringify([...this.chats.values()]));
    await rename(file + ".tmp", file);
  }
}
