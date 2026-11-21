// 聊天记录：收到的消息和机器人发出的消息按会话保存。
// 私聊以用户为单位（private:<user_id>），群聊以群为单位（group:<group_id>）。
//
// 文件都在 plugin-data/webui/chat/ 下：
//   chats.json                  会话列表（名称、备注、最后一条消息、未读数）
//   <会话>/<年-月-日>.jsonl       这个会话当天的消息，一行一条，只追加
//
// 按天分文件：超过保留天数时直接删掉那几天的文件；总大小超过上限时从最早的一天删起。
// 旧版本是每个会话一个 <会话>.jsonl，启动时会自动拆成按天的文件。
import { appendFile, mkdir, open, readdir, readFile, rename, rm, stat, writeFile } from "node:fs/promises";
import { join } from "node:path";
import type { OneBotEvent } from "../../goqbot.js";
import type { MediaCache } from "./media.ts";

export type ChatType = "private" | "group";

/**
 * 编号：goqbot 的 group_id、user_id、message_id 有 19 位，超过 JS 数字的精度，统一存成字符串。
 * 旧版 WebUI 存的是数字（最后几位不准），读到时原样保留。
 */
export type ID = string | number;

/** 转成编号字符串；没有时为空字符串 */
export function idOf(v: unknown): string {
  if (typeof v === "string") {
    return v.trim();
  }
  if (typeof v === "number" && Number.isFinite(v) && v !== 0) {
    return String(v);
  }
  if (typeof v === "bigint") {
    return v.toString();
  }
  return "";
}

/** 消息段，和 OneBot v11 的数组格式一致 */
export interface Segment {
  type: string;
  data: Record<string, unknown>;
}

/** 保存下来的一条消息 */
export interface ChatMessage {
  seq: number; // 会话内的序号，从 1 开始递增
  id: ID; // OneBot message_id（新记录是精确的字符串；旧记录是数字，19 位时最后几位不准）
  time: number; // 秒
  recv_ms?: number; // WebUI 收到这条消息的时间（毫秒），旧记录没有
  self: boolean; // 机器人自己发的
  user_id: ID;
  user_openid?: string;
  nickname: string; // 群里优先用群昵称（sender.card），没有时用 QQ 昵称
  role?: string;
  avatar?: string; // 发送者头像地址（事件里的 sender.avatar），旧记录没有
  bot?: boolean; // 发送者是机器人
  message: Segment[];
  raw: string;
  event?: OneBotEvent; // 收到的完整事件，供页面查看原始数据；旧记录没有
  notice?: SendNotice; // 有这一项时是一条灰字提示（发消息失败），不是聊天消息
  send_notes?: string[]; // 机器人发出的消息里没生效的部分（例如引用不了），页面在消息下面显示灰字提示
}

/** 灰字提示：goqbot 发消息失败 */
export interface SendNotice {
  level: "failed";
  error: string; // 失败原因
  notes: string[]; // 其他没生效的部分
  action: string; // 发送用的动作名
  reply_to: string; // 这次发送在回复哪条消息（reply 段里的 message_id），没有时为空
  qq_msg_id: string; // 用到的被动凭证（对应收到的消息事件里的 qq_msg_id），没有时为空
  preview: string; // 想发的内容（摘要）
}

/** 会话列表里的一项 */
export interface Chat {
  key: string; // private:<id> / group:<id>
  type: ChatType;
  id: ID; // 群编号 / 用户编号（新会话是精确的字符串）
  name: string; // 私聊为对方昵称；群聊为群基本信息接口查到的群名（查不到时为空）
  remark: string; // 自己设置的备注
  avatar?: string; // 私聊对方的头像地址
  openid?: string; // 私聊对方的 user_openid / 群的 group_openid
  last_time: number;
  last_text: string;
  last_sender: string;
  unread: number;
  seq: number; // 最后一条消息的序号
}

/** 聊天记录的保留设置 */
export interface HistoryLimits {
  keepDays: number; // 保留天数，0=不按天数删除
  maxBytes: number; // 聊天记录总大小上限（字节），0=不限制
}

const DAY_FILE_RE = /^(\d{4}-\d{2}-\d{2})\.jsonl$/;

/** 本地时间的日期 年-月-日 */
function dayOf(ms: number): string {
  const d = new Date(ms);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
}

/** 从文件末尾往前一行一行读（不把整个文件读进内存），fn 返回 false 时停下 */
async function readLinesReverse(file: string, fn: (line: string) => boolean): Promise<void> {
  let fh;
  try {
    fh = await open(file, "r");
  } catch {
    return;
  }
  try {
    let pos = (await fh.stat()).size;
    let rest = Buffer.alloc(0);
    while (pos > 0) {
      const len = Math.min(64 * 1024, pos);
      pos -= len;
      const buf = Buffer.alloc(len);
      await fh.read(buf, 0, len, pos);
      const data = rest.length ? Buffer.concat([buf, rest]) : buf;
      let end = data.length;
      for (let i = data.length - 1; i >= 0; i--) {
        if (data[i] === 10) {
          if (i + 1 < end && !fn(data.toString("utf8", i + 1, end))) {
            return;
          }
          end = i;
        }
      }
      rest = data.subarray(0, end);
    }
    if (rest.length) {
      fn(rest.toString("utf8"));
    }
  } finally {
    await fh.close();
  }
}

/** 文件第一行（不读整个文件） */
async function firstLine(file: string): Promise<string> {
  let fh;
  try {
    fh = await open(file, "r");
  } catch {
    return "";
  }
  try {
    const parts: Buffer[] = [];
    for (let pos = 0; pos < 8 * 1024 * 1024; pos += 64 * 1024) {
      const buf = Buffer.alloc(64 * 1024);
      const { bytesRead } = await fh.read(buf, 0, buf.length, pos);
      const nl = buf.subarray(0, bytesRead).indexOf(10);
      if (nl >= 0) {
        parts.push(buf.subarray(0, nl));
        break;
      }
      parts.push(buf.subarray(0, bytesRead));
      if (bytesRead < buf.length) {
        break;
      }
    }
    return Buffer.concat(parts).toString("utf8");
  } finally {
    await fh.close();
  }
}

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
/** 卡片消息（官方把卡片写成「[卡片消息] 卡片名\n摘要: …\ntitle: …」的文字）的一行说明 */
function arkSummary(text: string): string | undefined {
  const head = /^\[卡片消息\][ \t]*(.*)/.exec(text);
  if (!head) {
    return undefined;
  }
  const field = (k: string) => new RegExp(`^${k}:[ \\t]?(.*)$`, "m").exec(text)?.[1]?.trim() ?? "";
  const prompt = field("摘要");
  const title = field("title") || prompt.replace(/^\[[^\]]*\]/, "").trim();
  const miniapp = head[1].trim() === "小程序" || prompt.startsWith("[QQ小程序]");
  return miniapp ? `[QQ小程序] ${title}`.trim() : `[卡片] ${title || head[1].trim()}`.trim();
}

export function summary(segs: Segment[], quote = false): string {
  if (segs.length === 1 && segs[0].type === "text") {
    const ark = arkSummary(String(segs[0].data.text ?? ""));
    if (ark) {
      return ark.slice(0, 80);
    }
  }
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
    } else if (s.type === "reply" || (s.type === "forward" && quote)) {
      // 引用消息（QQ 类型 103）里的 forward 段是被引用的内容，不算进摘要
      continue;
    } else if (s.type === "markdown") {
      const c = markdownOf(s.data).content;
      text += c ? c.replace(/<qqbot-at-user[^>]*>|<@!?[^>]+>/g, "@").replace(/<qqbot-at-everyone\s*\/?>|<@all>/g, "@全体成员").replace(/!\[[^\]]*\]\([^)]*\)/g, "[图片]").replace(/\[([^\]]*)\]\([^)]*\)/g, "$1").replace(/[#*_`>]/g, "").slice(0, 60) : names.markdown;
    } else {
      text += names[s.type] ?? `[${s.type}]`;
    }
    if (text.length > 80) {
      break;
    }
  }
  // 其他机器人发的 Markdown 以文字形式收到，链接只留文字
  text = text.replace(/\[([^\]\n]*)\]\((?:https?|mqqapi):\/\/[^)\s]*\)/gi, "$1").replace(/\*\*([^*\n]+)\*\*/g, "$1");
  return text.replace(/\s+/g, " ").trim().slice(0, 80);
}

export class ChatStore {
  readonly dir: string;
  private chats = new Map<string, Chat>();
  private saveTimer: NodeJS.Timeout | undefined;
  private writing = new Map<string, Promise<unknown>>(); // 每个会话的读写按顺序进行
  private limits: HistoryLimits = { keepDays: 0, maxBytes: 0 };
  private totalBytes = 0; // 聊天记录总大小（启动和每次清理时重新统计，中间按写入的字节累加）
  private firstSeq = new Map<string, number>(); // 每个按天文件第一条消息的序号，翻历史时跳过整天用
  private migrated = new Set<string>(); // 已经检查过旧版文件的会话
  private maintaining: Promise<void> | undefined;
  private lastMaintain = 0;
  private maintainDay = "";
  private maintainTimer: NodeJS.Timeout | undefined;
  private listeners = new Set<(ev: { chat: Chat; message: ChatMessage }) => void>();
  private renameListeners = new Set<(from: string, to: string) => void>();
  private media: MediaCache | undefined;

  /** 设置图片缓存：收到的图片会下载到本地，地址过期后还能看 */
  setMedia(media: MediaCache): void {
    this.media = media;
  }

  constructor(dataDir: string) {
    this.dir = join(dataDir, "chat");
  }

  /** 设置聊天记录的保留天数和总大小上限，立即按新设置清理一次 */
  setLimits(limits: HistoryLimits): Promise<void> {
    this.limits = { keepDays: Math.max(0, Math.floor(limits.keepDays) || 0), maxBytes: Math.max(0, Math.floor(limits.maxBytes) || 0) };
    return this.maintain();
  }

  /** 聊天记录现在占的空间（字节） */
  usage(): number {
    return this.totalBytes;
  }

  /** 停止定时清理（插件退出时） */
  stop(): void {
    clearInterval(this.maintainTimer);
  }

  async load(): Promise<void> {
    await mkdir(this.dir, { recursive: true });
    // 每 10 分钟看一次：日期变了按保留天数清理，或者总大小超了上限
    this.maintainTimer = setInterval(() => {
      const over = this.limits.maxBytes > 0 && this.totalBytes > this.limits.maxBytes;
      if (over || dayOf(Date.now()) !== this.maintainDay) {
        void this.maintain();
      }
    }, 10 * 60 * 1000);
    this.maintainTimer.unref?.();
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
    const peer = idOf(type === "group" ? ev.group_id : sent ? ev.target_id : ev.user_id);
    if (!peer || peer === "0") {
      return;
    }
    const key = `${type}:${peer}`;
    await this.migrateLegacy(type, peer);
    const sender = (ev.sender && typeof ev.sender === "object" ? ev.sender : {}) as Record<string, unknown>;
    const raw = typeof ev.raw_message === "string" ? ev.raw_message : "";
    const segs = toSegments(ev.message, raw);
    // 图片地址会过期：先记下本地缓存地址（data.local），后台下载
    this.media?.attach(segs as unknown as Record<string, unknown>[], ev as Record<string, unknown>);
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
    const avatar = typeof sender.avatar === "string" ? sender.avatar : "";
    if (type === "private" && !sent && nickname) {
      chat.name = nickname;
    }
    if (type === "private" && !sent && avatar) {
      chat.avatar = avatar;
    }
    const msg: ChatMessage = {
      seq: ++chat.seq,
      id: idOf(ev.message_id),
      time: num(ev.time) || Math.floor(Date.now() / 1000),
      recv_ms: Date.now(),
      self: sent,
      user_id: idOf(ev.user_id),
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
    if (avatar && !sent) {
      msg.avatar = avatar;
    }
    if (sender.bot === true && !sent) {
      msg.bot = true;
    }
    if (sent && Array.isArray(ev.send_notes) && ev.send_notes.length) {
      msg.send_notes = ev.send_notes.map(String);
    }
    chat.last_time = msg.time;
    chat.last_text = summary(segs, Number(ev.qq_message_type) === 103);
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

  /**
   * 旧版 WebUI 按丢了精度的数字存会话（如 group:6943781100563829000）。
   * 收到这个会话的新消息、拿到精确编号后，把旧会话和聊天记录合并到新的 key 下
   */
  private async migrateLegacy(type: ChatType, peer: string): Promise<void> {
    const key = `${type}:${peer}`;
    if (this.chats.has(key) || !/^\d{17,}$/.test(peer)) {
      return;
    }
    const legacyKey = `${type}:${String(Number(peer))}`;
    const legacy = this.chats.get(legacyKey);
    if (legacyKey === key || !legacy) {
      return;
    }
    await this.writing.get(legacyKey);
    this.writing.delete(legacyKey);
    this.chats.delete(legacyKey);
    legacy.key = key;
    legacy.id = peer;
    this.chats.set(key, legacy);
    for (const [from, to] of [[this.dirOf(legacyKey), this.dirOf(key)], [this.legacyFileOf(legacyKey), this.legacyFileOf(key)]]) {
      try {
        await rename(from, to);
      } catch {
        // 旧会话没有聊天记录
      }
    }
    this.migrated.delete(legacyKey);
    this.scheduleSave();
    for (const fn of this.renameListeners) {
      fn(legacyKey, key);
    }
  }

  /** 会话换了 key 时通知（旧版数据合并） */
  onRename(fn: (from: string, to: string) => void): void {
    this.renameListeners.add(fn);
  }

  /** 会话的聊天记录文件夹 */
  private dirOf(key: string): string {
    return join(this.dir, key.replace(":", "-"));
  }

  /** 旧版本的单个聊天记录文件 */
  private legacyFileOf(key: string): string {
    return this.dirOf(key) + ".jsonl";
  }

  /** 同一个会话的读写按顺序进行 */
  private queue<T>(key: string, fn: () => Promise<T>): Promise<T> {
    const prev = this.writing.get(key) ?? Promise.resolve();
    const next = prev.then(fn);
    const done = next.catch(() => {});
    this.writing.set(key, done);
    // 做完后如果没有排在后面的任务，就从表里去掉，会话很多时不留一堆已完成的任务
    void done.then(() => {
      if (this.writing.get(key) === done) {
        this.writing.delete(key);
      }
    });
    return next;
  }

  /** 旧版本的 <会话>.jsonl 拆成按天的文件（每个会话只检查一次）。要在这个会话的队列里调用 */
  private async migrateFile(key: string): Promise<void> {
    if (this.migrated.has(key)) {
      return;
    }
    this.migrated.add(key);
    const legacy = this.legacyFileOf(key);
    let text: string;
    try {
      text = await readFile(legacy, "utf8");
    } catch {
      return;
    }
    const days = new Map<string, string[]>();
    for (const line of text.split("\n")) {
      if (!line) {
        continue;
      }
      let day = "";
      try {
        const m = JSON.parse(line) as ChatMessage;
        day = dayOf(m.recv_ms || m.time * 1000 || Date.now());
      } catch {
        continue; // 跳过写坏的行
      }
      if (!days.has(day)) {
        days.set(day, []);
      }
      days.get(day)!.push(line);
    }
    const dir = this.dirOf(key);
    await mkdir(dir, { recursive: true });
    for (const day of [...days.keys()].sort()) {
      await appendFile(join(dir, `${day}.jsonl`), days.get(day)!.join("\n") + "\n");
    }
    await rm(legacy, { force: true });
  }

  /**
   * 记下一条发送失败的灰字提示（goqbot 的 notice.send_failed 事件）：放在对应会话里，不算未读，也不改会话列表的预览。
   * 会话还不存在时不记（例如往一个从没收到过消息的群发消息）
   */
  async addNotice(ev: OneBotEvent): Promise<void> {
    if (ev.post_type !== "notice" || ev.notice_type !== "send_failed") {
      return;
    }
    const type = ev.chat_type === "group" ? "group" : ev.chat_type === "private" ? "private" : undefined;
    const peer = idOf(type === "group" ? ev.group_id : ev.user_id);
    if (!type || !peer || peer === "0") {
      return;
    }
    const key = `${type}:${peer}`;
    const chat = this.chats.get(key);
    if (!chat) {
      return;
    }
    const raw = typeof ev.raw_message === "string" ? ev.raw_message : "";
    const segs = toSegments(ev.message, raw);
    const str = (v: unknown) => (typeof v === "string" ? v : "");
    const msg: ChatMessage = {
      seq: ++chat.seq,
      id: "",
      time: Number(ev.time) || Math.floor(Date.now() / 1000),
      recv_ms: Date.now(),
      self: true,
      user_id: "",
      nickname: "",
      message: [],
      raw: "",
      event: ev,
      notice: {
        level: "failed",
        error: str(ev.error),
        notes: Array.isArray(ev.notes) ? ev.notes.map(String) : [],
        action: str(ev.action),
        reply_to: ev.reply_to ? idOf(ev.reply_to) : "",
        qq_msg_id: str(ev.qq_msg_id),
        preview: segs.length ? summary(segs, false) : raw,
      },
    };
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

  private append(key: string, msg: ChatMessage): Promise<void> {
    return this.queue(key, async () => {
      await this.migrateFile(key);
      const dir = this.dirOf(key);
      await mkdir(dir, { recursive: true });
      const line = JSON.stringify(msg) + "\n";
      await appendFile(join(dir, `${dayOf(msg.recv_ms || Date.now())}.jsonl`), line);
      this.totalBytes += Buffer.byteLength(line);
      // 超过总大小上限时清理（最多一分钟一次）
      if (this.limits.maxBytes > 0 && this.totalBytes > this.limits.maxBytes && Date.now() - this.lastMaintain > 60_000) {
        void this.maintain();
      }
    });
  }

  /**
   * 按设置清理聊天记录：删掉超过保留天数的按天文件；总大小超过上限时从最早的一天删起，删到上限的 90%。
   * 顺便把旧版本的单个文件拆成按天的文件，并重新统计总大小
   */
  maintain(): Promise<void> {
    if (this.maintaining) {
      return this.maintaining;
    }
    this.lastMaintain = Date.now();
    // 清理出错（例如 Windows 上文件正被打开删不掉）不影响运行，下次再试
    this.maintaining = this.doMaintain()
      .catch(() => {})
      .finally(() => {
        this.maintaining = undefined;
      });
    return this.maintaining;
  }

  private async doMaintain(): Promise<void> {
    const now = Date.now();
    const cutoff = this.limits.keepDays > 0 ? dayOf(now - this.limits.keepDays * 86400_000) : "";
    let entries;
    try {
      entries = await readdir(this.dir, { withFileTypes: true });
    } catch {
      return;
    }
    const legacyRe = /^(private|group)-(.+)\.jsonl$/;
    for (const e of entries) {
      const m = e.isFile() ? legacyRe.exec(e.name) : null;
      if (m) {
        const key = `${m[1]}:${m[2]}`;
        await this.queue(key, () => this.migrateFile(key)).catch(() => {});
      }
    }
    const files: { path: string; day: string; size: number }[] = [];
    let total = 0;
    for (const e of await readdir(this.dir, { withFileTypes: true })) {
      if (!e.isDirectory()) {
        continue;
      }
      const dir = join(this.dir, e.name);
      let names: string[];
      try {
        names = await readdir(dir);
      } catch {
        continue;
      }
      for (const name of names) {
        const m = DAY_FILE_RE.exec(name);
        if (!m) {
          continue;
        }
        const path = join(dir, name);
        if (cutoff && m[1] < cutoff) {
          try {
            await rm(path, { force: true });
            this.firstSeq.delete(path);
            continue;
          } catch {
            // 删不掉（例如正被打开），按还在算，下次再删
          }
        }
        try {
          const st = await stat(path);
          files.push({ path, day: m[1], size: st.size });
          total += st.size;
        } catch {
          // 文件刚被删掉
        }
      }
    }
    if (this.limits.maxBytes > 0 && total > this.limits.maxBytes) {
      const target = this.limits.maxBytes * 0.9;
      files.sort((a, b) => a.day.localeCompare(b.day) || b.size - a.size);
      for (const f of files) {
        if (total <= target) {
          break;
        }
        try {
          await rm(f.path, { force: true });
          this.firstSeq.delete(f.path);
          total -= f.size;
        } catch {
          // 删不掉就跳过
        }
      }
    }
    this.totalBytes = total;
    this.maintainDay = dayOf(now);
  }

  /** 读取消息：before 为序号，返回比它早的最多 limit 条（按时间正序）。从最新的一天往前、每个文件从末尾往前读 */
  async messages(key: string, before: number, limit: number): Promise<{ messages: ChatMessage[]; more: boolean }> {
    if (!this.chats.has(key)) {
      return { messages: [], more: false };
    }
    return this.queue(key, async () => {
      await this.migrateFile(key);
      const dir = this.dirOf(key);
      let names: string[];
      try {
        names = (await readdir(dir)).filter((n) => DAY_FILE_RE.test(n)).sort().reverse();
      } catch {
        return { messages: [], more: false };
      }
      const got: ChatMessage[] = [];
      for (const name of names) {
        if (got.length > limit) {
          break;
        }
        const path = join(dir, name);
        // 这一天的第一条都不比 before 早，整天跳过
        if (before && (await this.firstSeqOf(path)) >= before) {
          continue;
        }
        await readLinesReverse(path, (line) => {
          try {
            const m = JSON.parse(line) as ChatMessage;
            if (!before || m.seq < before) {
              got.push(m);
            }
          } catch {
            // 跳过写坏的行
          }
          return got.length <= limit;
        });
      }
      const more = got.length > limit;
      return { messages: got.slice(0, limit).reverse(), more };
    });
  }

  /** 按天文件第一条消息的序号（第一行写下后不会变，算一次就记住） */
  private async firstSeqOf(path: string): Promise<number> {
    const cached = this.firstSeq.get(path);
    if (cached !== undefined) {
      return cached;
    }
    let seq = 0;
    try {
      seq = Number((JSON.parse(await firstLine(path)) as ChatMessage).seq) || 0;
    } catch {
      return 0; // 读不到时不跳过
    }
    this.firstSeq.set(path, seq);
    return seq;
  }

  markRead(key: string): void {
    const chat = this.chats.get(key);
    if (chat && chat.unread) {
      chat.unread = 0;
      this.scheduleSave();
    }
  }

  /** 删除一个会话和它的全部聊天记录；之后再有新消息时会重新出现 */
  async remove(key: string): Promise<boolean> {
    if (!this.chats.has(key)) {
      return false;
    }
    this.chats.delete(key);
    this.scheduleSave();
    await this.queue(key, async () => {
      await rm(this.dirOf(key), { recursive: true, force: true });
      await rm(this.legacyFileOf(key), { force: true });
    }).catch(() => {});
    for (const path of [...this.firstSeq.keys()]) {
      if (path.startsWith(this.dirOf(key) + "/") || path.startsWith(this.dirOf(key) + "\\")) {
        this.firstSeq.delete(path);
      }
    }
    return true;
  }

  /** 记下群基本信息接口查到的群名 */
  setGroupName(key: string, name: string): Chat | undefined {
    const chat = this.chats.get(key);
    if (chat && chat.type === "group" && name && chat.name !== name) {
      chat.name = name;
      this.scheduleSave();
    }
    return chat;
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
    // 会话列表整个写一遍（会话多时有好几 MB），消息多时最多 5 秒写一次；退出前 onShutdown 会再保存一次
    this.saveTimer = setTimeout(() => {
      this.saveTimer = undefined;
      void this.save();
    }, 5000);
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
