// goqbot WebUI：网页版 QQ 客户端 + goqbot 管理面板。
//
// 聊天：私聊按用户、群聊按群保存收发过的消息，页面实时显示（当前只收不发）。
// 管理：运行状态、下游连接（改完立即生效）、插件、日志、图形化配置、更新 goqbot。
// 内置指令：有人发 #gq（可在配置页修改或关闭）时，回复 goqbot 的运行平台和运行时长。
//
// 首次启动时在数据目录生成 config.json（监听地址、端口、登录口令），启动日志里会打印带口令的登录链接。
// 页面是 public/ 下的原生 HTML / CSS / JS，不依赖任何 npm 包，放进 plugins/ 即可运行。
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import { readFile, writeFile } from "node:fs/promises";
import { createHash, randomBytes, timingSafeEqual } from "node:crypto";
import { extname, join, resolve, sep } from "node:path";
import type { Context, GoqbotError, OneBotEvent, Plugin } from "../goqbot.js";
import { ChatStore, idOf, type ChatType } from "./lib/chat.ts";
import { checkRelease, download, stage } from "./lib/update.ts";
import { MediaCache } from "./lib/media.ts";

/** WebUI 自己的设置，保存在 plugin-data/webui/config.json */
interface Settings {
  host: string;
  port: number;
  token: string;
  github_proxy: string; // 下载更新时加在 GitHub 地址前的代理，空=直连
  commands: Commands;
  history: History;
}

/** 聊天记录和图片缓存的保留设置 */
interface History {
  keep_days: number; // 聊天记录（和缓存的图片）保留天数，0=不按天数删除
  max_mb: number; // 聊天记录总大小上限（MB），0=不限制
  media_max_mb: number; // 图片缓存总大小上限（MB），0=不限制
}

const DEFAULT_HISTORY: History = { keep_days: 0, max_mb: 2048, media_max_mb: 1024 };

/** 读取保存的保留设置，不合法的项用默认值 */
function readHistory(v: unknown): History {
  const o = (v && typeof v === "object" ? v : {}) as Record<string, unknown>;
  const int = (x: unknown, def: number, max: number) => (Number.isInteger(x) && (x as number) >= 0 && (x as number) <= max ? (x as number) : def);
  return {
    keep_days: int(o.keep_days, DEFAULT_HISTORY.keep_days, 36500),
    max_mb: int(o.max_mb, DEFAULT_HISTORY.max_mb, 1024 * 1024),
    media_max_mb: int(o.media_max_mb, DEFAULT_HISTORY.media_max_mb, 1024 * 1024),
  };
}

/** 按保留设置清理聊天记录和图片缓存 */
function applyHistory(h: History): void {
  void store.setLimits({ keepDays: h.keep_days, maxBytes: h.max_mb * 1024 * 1024 }).catch(() => {});
  void media.setLimits(h.media_max_mb * 1024 * 1024, h.keep_days).catch(() => {});
}

/** 内置指令 */
interface Commands {
  status: { enabled: boolean; keyword: string }; // 发 keyword 回复运行平台和运行时长
}

const DEFAULT_COMMANDS: Commands = { status: { enabled: true, keyword: "#gq" } };

function readCommands(v: unknown): Commands {
  const st = (v && typeof v === "object" ? (v as Record<string, any>).status : undefined) ?? {};
  return {
    status: {
      enabled: typeof st.enabled === "boolean" ? st.enabled : DEFAULT_COMMANDS.status.enabled,
      keyword: typeof st.keyword === "string" && st.keyword.trim() ? st.keyword.trim().slice(0, 20) : DEFAULT_COMMANDS.status.keyword,
    },
  };
}

const PUBLIC_DIR = resolve(import.meta.dirname, "public");
const MAX_BODY = 2 * 1024 * 1024;
const MAX_UPLOAD = 200 * 1024 * 1024;
const MAX_IMAGE = 20 * 1024 * 1024; // 网页发图片、上传素材的大小上限

/** 按文件头判断是不是图片（png / jpg / gif / webp / bmp） */
function isImage(b: Buffer): boolean {
  const hex = b.subarray(0, 12).toString("hex");
  return hex.startsWith("89504e47") || hex.startsWith("ffd8ff") || hex.startsWith("47494638") || hex.startsWith("424d")
    || (hex.startsWith("52494646") && b.subarray(8, 12).toString("ascii") === "WEBP");
}
const MIME: Record<string, string> = {
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".svg": "image/svg+xml",
  ".png": "image/png",
  ".ico": "image/x-icon",
};
const SECURITY_HEADERS = {
  // 消息里的图片、头像来自 QQ 的服务器，所以 img/media 允许 https
  "content-security-policy":
    "default-src 'self'; img-src 'self' data: http: https:; media-src http: https:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'",
  "x-content-type-options": "nosniff",
  "referrer-policy": "no-referrer",
  "cache-control": "no-store",
};

/** 读取设置；第一次运行时生成随机口令并保存。文件写坏了时报错，不覆盖 */
async function loadSettings(ctx: Context): Promise<{ settings: Settings; created: boolean }> {
  const file = join(ctx.dataDir, "config.json");
  let text: string;
  try {
    text = await readFile(file, "utf8");
  } catch (e) {
    if ((e as NodeJS.ErrnoException).code !== "ENOENT") {
      throw e;
    }
    const settings: Settings = { host: "127.0.0.1", port: 6099, token: randomBytes(12).toString("base64url"), github_proxy: "", commands: readCommands(undefined), history: readHistory(undefined) };
    await writeFile(file, JSON.stringify(settings, null, 2) + "\n", { mode: 0o600 });
    return { settings, created: true };
  }
  let saved: Partial<Settings>;
  try {
    saved = JSON.parse(text);
  } catch {
    throw new Error(`${file} 不是合法的 JSON，请改正或删除它（删除后会重新生成口令）`);
  }
  if (typeof saved.token !== "string" || saved.token.length < 8) {
    throw new Error(`${file} 里的 token 至少要 8 个字符`);
  }
  return {
    settings: {
      host: typeof saved.host === "string" && saved.host !== "" ? saved.host : "127.0.0.1",
      port: Number.isInteger(saved.port) ? Number(saved.port) : 6099,
      token: saved.token,
      github_proxy: typeof saved.github_proxy === "string" ? saved.github_proxy : "",
      commands: readCommands(saved.commands),
      history: readHistory(saved.history),
    },
    created: false,
  };
}

async function saveSettings(ctx: Context, settings: Settings): Promise<void> {
  await writeFile(join(ctx.dataDir, "config.json"), JSON.stringify(settings, null, 2) + "\n", { mode: 0o600 });
}

/** 定长比较口令，避免按耗时猜测 */
function tokenMatches(given: string, token: string): boolean {
  const a = createHash("sha256").update(given).digest();
  const b = createHash("sha256").update(token).digest();
  return timingSafeEqual(a, b);
}

/** 登录失败限流：每个 IP 每 5 分钟最多失败 10 次 */
const failures = new Map<string, { count: number; since: number }>();
function tooManyFailures(ip: string): boolean {
  const f = failures.get(ip);
  if (!f || Date.now() - f.since > 5 * 60_000) {
    failures.delete(ip);
    return false;
  }
  return f.count >= 10;
}
function recordFailure(ip: string): void {
  const f = failures.get(ip);
  if (!f || Date.now() - f.since > 5 * 60_000) {
    failures.set(ip, { count: 1, since: Date.now() });
  } else {
    f.count++;
  }
}

/** 请求处理中的错误：带上 HTTP 状态码和给页面看的中文说明 */
class HttpError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

function send(res: ServerResponse, status: number, body: unknown): void {
  res.writeHead(status, { ...SECURITY_HEADERS, "content-type": "application/json; charset=utf-8" });
  res.end(JSON.stringify(body));
}

async function readBody(req: IncomingMessage, limit: number): Promise<Buffer> {
  let size = 0;
  const chunks: Buffer[] = [];
  for await (const chunk of req) {
    size += (chunk as Buffer).length;
    if (size > limit) {
      throw new HttpError(413, "内容太大");
    }
    chunks.push(chunk as Buffer);
  }
  return Buffer.concat(chunks);
}

async function readJSON(req: IncomingMessage): Promise<Record<string, unknown>> {
  const buf = await readBody(req, MAX_BODY);
  if (buf.length === 0) {
    return {};
  }
  try {
    const value = JSON.parse(buf.toString("utf8"));
    return value && typeof value === "object" && !Array.isArray(value) ? value : {};
  } catch {
    throw new HttpError(400, "请求内容不是合法的 JSON");
  }
}

/** 调用 goqbot 的能力，失败时转成页面能看懂的错误 */
async function call<T extends Record<string, unknown>>(ctx: Context, name: string, args: Record<string, unknown> = {}): Promise<T> {
  try {
    return await ctx.call<T>(name, args);
  } catch (e) {
    const err = e as GoqbotError;
    const message = err.data?.detail ?? err.data?.wording ?? err.message ?? "调用失败";
    throw new HttpError(err.code === -32602 ? 400 : 502, String(message));
  }
}

/** 旧版 WebUI 存的会话编号丢了精度，不能用来发消息 */
function assertExactId(chat: { type: string; id: unknown }): void {
  if (typeof chat.id === "number" && !Number.isSafeInteger(chat.id)) {
    const who = chat.type === "group" ? "这个群" : "这个用户";
    throw new HttpError(409, `这个会话是旧版 WebUI 保存的，编号不完整。等${who}发来新消息后就能操作了`);
  }
}

/** 只保留连接配置里认识的字段，防止页面写进别的东西 */
function cleanServers(v: unknown): unknown[] {
  if (!Array.isArray(v)) {
    throw new HttpError(400, "servers 应该是列表");
  }
  // 名称有填才写；开关只在打开时写进配置，保持 config.yml 简洁
  const flags = (out: Record<string, unknown>, src: Record<string, unknown>) => {
    const name = typeof src.name === "string" ? src.name.trim().slice(0, 40) : "";
    if (name) {
      out.name = name;
    }
    if (src.disabled === true) {
      out.disabled = true;
    }
    if (src["allow-sensitive-plugins"] === true) {
      out["allow-sensitive-plugins"] = true;
    }
    // 口令和心跳每条连接单独保存（空口令也写上，表示这条连接不校验，不再沿用全局的 onebot.access_token）
    out["access-token"] = typeof src["access-token"] === "string" ? src["access-token"].trim() : "";
    const hb = Number(src["heartbeat-interval"]);
    out["heartbeat-interval"] = Number.isInteger(hb) ? hb : 5000;
  };
  const str = (x: unknown) => (typeof x === "string" ? x.trim() : "");
  return v.map((item, i) => {
    const it = (item && typeof item === "object" ? item : {}) as Record<string, Record<string, unknown> | undefined>;
    if (it.ws) {
      const address = str(it.ws.address);
      if (!address) {
        throw new HttpError(400, `第 ${i + 1} 条正向 WS 没有填监听地址`);
      }
      const ws: Record<string, unknown> = {};
      flags(ws, it.ws);
      ws.address = address;
      return { ws };
    }
    if (it["ws-reverse"]) {
      const r = it["ws-reverse"];
      const out: Record<string, unknown> = {};
      flags(out, r);
      for (const k of ["universal", "api", "event"]) {
        if (str(r[k])) {
          out[k] = str(r[k]);
        }
      }
      if (!out.universal && !out.api && !out.event) {
        throw new HttpError(400, `第 ${i + 1} 条反向 WS 没有填连接地址`);
      }
      const interval = Number(r["reconnect-interval"]);
      out["reconnect-interval"] = Number.isInteger(interval) && interval > 0 ? interval : 3000;
      return { "ws-reverse": out };
    }
    throw new HttpError(400, `第 ${i + 1} 条连接类型不对`);
  });
}

// ---------- 内置指令 ----------

/** linux-amd64 → Linux x86_64 */
function platformName(p: string): string {
  const [os = "", arch = ""] = p.split("-");
  const osName: Record<string, string> = { linux: "Linux", darwin: "macOS", windows: "Windows", freebsd: "FreeBSD" };
  const archName: Record<string, string> = { amd64: "x86_64", arm64: "ARM64", "386": "x86", arm: "ARM" };
  return `${osName[os] ?? os} ${archName[arch] ?? arch}`.trim();
}

function durationText(sec: number): string {
  const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
  if (d) return `${d} 天 ${h} 小时 ${m} 分`;
  if (h) return `${h} 小时 ${m} 分`;
  if (m) return `${m} 分 ${sec % 60} 秒`;
  return `${sec} 秒`;
}

/** #gq 回复的内容 */
async function statusText(ctx: Context): Promise<string> {
  const info = await call<{ version: string; platform: string; uptime_seconds: number }>(ctx, "host.info");
  return `goqbot ${info.version} 运行中\n运行平台：${platformName(info.platform)}\n运行时长：${durationText(info.uptime_seconds)}`;
}

/** 取出消息里的文字（去掉 @ 等其他段） */
function plainText(ev: OneBotEvent): string {
  if (Array.isArray(ev.message)) {
    return (ev.message as { type?: string; data?: { text?: unknown } }[])
      .filter((s) => s?.type === "text")
      .map((s) => String(s.data?.text ?? ""))
      .join("")
      .trim();
  }
  return String(ev.raw_message ?? "").replace(/\[CQ:[^\]]*\]/g, "").trim();
}

const lastReply = new Map<string, number>(); // 每个会话 3 秒内只回复一次，防止刷屏

/** 收到消息时检查是不是内置指令 */
async function handleCommand(ctx: Context, ev: OneBotEvent): Promise<void> {
  const st = settings.commands.status;
  if (!st.enabled || ev.post_type !== "message") {
    return;
  }
  if (plainText(ev).toLowerCase() !== st.keyword.toLowerCase()) {
    return;
  }
  const group = ev.message_type === "group";
  const key = group ? `g${ev.group_id}` : `p${ev.user_id}`;
  if (Date.now() - (lastReply.get(key) ?? 0) < 3000) {
    return;
  }
  lastReply.set(key, Date.now());
  const message = [
    { type: "reply", data: { id: String(ev.message_id) } },
    { type: "text", data: { text: await statusText(ctx) } },
  ];
  // 引用对方的消息回复：goqbot 会自动用这条消息的被动回复凭证
  if (group) {
    await ctx.call("send_group_msg", { group_id: ev.group_id, message });
  } else {
    await ctx.call("send_private_msg", { user_id: ev.user_id, message });
  }
}

// ---------- 图形化配置 ----------

/** 配置页能改的 config.yml 项和类型（下游连接在「连接」页改） */
const GOQBOT_FIELDS: Record<string, "string" | "bool" | "int" | "strings"> = {
  "qq.app_id": "string",
  "qq.client_secret": "string",
  "qq.token": "string",
  "qq.uin": "int",
  "qq.use_uin": "bool",
  "qq.sandbox": "bool",
  "qq.shard_count": "int",
  "qq.shard_id": "int",
  "qq.mention_mode": "string",
  "qq.ip_reject_action": "string",
  "qq.ip_reject_retry_interval": "int",
  "qq.text_intent": "strings",
  "qq.developer_openid": "string",
  "onebot.message_format": "string",
  "onebot.debug": "bool",
  "onebot.downtime_message": "string",
  "idmap.disabled": "bool",
  "idmap.path": "string",
  "idmap.keep_days": "int",
  "idmap.max_size_mb": "int",
  "log.enabled": "bool",
  "log.path": "string",
  "plugins.path": "string",
  "plugins.node": "string",
};

function cleanGoqbotValues(v: unknown): Record<string, unknown> {
  if (!v || typeof v !== "object" || Array.isArray(v)) {
    throw new HttpError(400, "values 应该是对象");
  }
  const out: Record<string, unknown> = {};
  for (const [path, value] of Object.entries(v)) {
    const type = GOQBOT_FIELDS[path];
    if (!type) {
      throw new HttpError(400, `不能在这里修改 ${path}`);
    }
    const ok =
      (type === "string" && typeof value === "string") ||
      (type === "bool" && typeof value === "boolean") ||
      (type === "int" && Number.isInteger(value)) ||
      (type === "strings" && Array.isArray(value) && value.every((x) => typeof x === "string"));
    if (!ok) {
      throw new HttpError(400, `${path} 的值类型不对`);
    }
    out[path] = type === "string" ? (value as string).trim() : value;
  }
  return out;
}

/** 正在打开的实时推送连接 */
const streams = new Set<ServerResponse>();

function push(event: string, data: unknown): void {
  const frame = `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`;
  for (const res of streams) {
    res.write(frame);
  }
}

let settings: Settings;
let store: ChatStore;
let media: MediaCache;
/** 机器人在各个群里的状态（身份、是否允许主动发言），缓存 5 分钟 */
const botRoles = new Map<string, { role: string; allow: boolean | null; error: string; at: number }>();

/** 查机器人在群里的状态；官方接口需要白名单，查不到时 role 为空、allow 为 null */
async function botState(ctx: Context, chat: { key: string; id: unknown; openid?: string }) {
  const cached = botRoles.get(chat.key);
  if (cached && Date.now() - cached.at < 5 * 60_000) {
    return cached;
  }
  const entry = { role: "", allow: null as boolean | null, error: "", at: Date.now() };
  try {
    const st = await call<Record<string, unknown>>(ctx, "get_group_bot_state", chat.openid ? { group_openid: chat.openid } : { group_id: idOf(chat.id) });
    entry.role = String(st.member_role ?? "");
    entry.allow = typeof st.allow_proactive_msg === "boolean" ? st.allow_proactive_msg : null;
  } catch (e) {
    entry.error = (e as Error).message;
  }
  botRoles.set(chat.key, entry);
  return entry;
}

/** /api/* 接口 */
async function handleApi(ctx: Context, req: IncomingMessage, res: ServerResponse, url: URL): Promise<void> {
  const route = `${req.method} ${url.pathname}`;
  const q = url.searchParams;
  const lines = Math.min(Math.max(Number(q.get("lines")) || 300, 1), 2000);
  switch (route) {
    // ---------- 聊天 ----------
    case "GET /api/chats": {
      const type = q.get("type");
      return send(res, 200, { chats: store.list(type === "private" || type === "group" ? (type as ChatType) : undefined) });
    }
    case "GET /api/chats/messages": {
      const key = q.get("key") ?? "";
      const limit = Math.min(Math.max(Number(q.get("limit")) || 50, 1), 200);
      return send(res, 200, await store.messages(key, Number(q.get("before")) || 0, limit));
    }
    case "POST /api/chats/read": {
      const body = await readJSON(req);
      store.markRead(String(body.key ?? ""));
      return send(res, 200, {});
    }
    case "POST /api/chats/remark": {
      const body = await readJSON(req);
      const chat = store.setRemark(String(body.key ?? ""), String(body.remark ?? ""));
      if (!chat) {
        throw new HttpError(404, "没有这个会话");
      }
      push("chat", chat);
      return send(res, 200, { chat });
    }
    case "POST /api/chats/delete": {
      const body = await readJSON(req);
      const key = String(body.key ?? "");
      if (!(await store.remove(key))) {
        throw new HttpError(404, "没有这个会话");
      }
      push("chat_removed", { key });
      return send(res, 200, {});
    }
    case "POST /api/chats/send": {
      // 发消息：message 是 OneBot 消息段数组；发出去的消息会以 message_sent 事件回到聊天记录
      const body = await readJSON(req);
      const chat = store.get(String(body.key ?? ""));
      if (!chat) {
        throw new HttpError(404, "没有这个会话");
      }
      const message = Array.isArray(body.message) ? body.message : [];
      if (!message.length) {
        throw new HttpError(400, "消息是空的");
      }
      assertExactId(chat);
      const target = idOf(chat.id);
      const result = chat.type === "group"
        ? await call(ctx, "send_group_msg", { group_id: target, message })
        : await call(ctx, "send_private_msg", { user_id: target, message });
      return send(res, 200, { message_id: idOf(result.message_id) });
    }
    case "GET /api/chats/bot-role": {
      // 机器人在这个群里的身份（owner / admin / member），决定能不能禁言、撤回别人的消息。
      // 官方接口需要白名单；查不到时 role 为空，页面按「不确定」处理
      const chat = store.get(q.get("key") ?? "");
      if (!chat || chat.type !== "group") {
        throw new HttpError(404, "没有这个群");
      }
      const st = await botState(ctx, chat);
      return send(res, 200, { role: st.role, error: st.error });
    }
    case "GET /api/chats/quota": {
      // 发消息额度：被动回复还剩多久、已回复几次，今天发了几条主动消息（数据来自 goqbot 的记录）
      const chat = store.get(q.get("key") ?? "");
      if (!chat) {
        throw new HttpError(404, "没有这个会话");
      }
      const target = chat.type === "group"
        ? (chat.openid ? { group_openid: chat.openid } : { group_id: idOf(chat.id) })
        : (chat.openid ? { user_openid: chat.openid } : { user_id: idOf(chat.id) });
      const quota = await call<Record<string, unknown>>(ctx, "get_message_quota", target);
      // 群聊：群主有没有打开「允许机器人主动发言」；私聊：对方有没有打开机器人的主动消息开关（查不到时为 null）
      const allow = chat.type === "group" ? (await botState(ctx, chat)).allow : await c2cAllow(ctx, target);
      return send(res, 200, { ...quota, allow_proactive_msg: allow, now: Math.floor(Date.now() / 1000) });
    }
    case "POST /api/chats/private-reply": {
      // 群消息私聊回复：用群消息的 msg_id 私聊回复这条消息的发送者（对方要是机器人好友）。
      // 群消息发出后 60 分钟内有效，次数按群消息规则算、和群内回复共用；没有可用的被动凭证时不改发主动消息
      const body = await readJSON(req);
      const chat = store.get(String(body.key ?? ""));
      if (!chat || chat.type !== "group") {
        throw new HttpError(404, "没有这个群");
      }
      const userId = idOf(body.user_id);
      const msgId = idOf(body.message_id);
      const text = String(body.text ?? "").trim();
      if (!userId || !msgId) {
        throw new HttpError(400, "缺少用户编号或消息编号");
      }
      if (!text) {
        throw new HttpError(400, "消息是空的");
      }
      const message = [{ type: "reply", data: { id: msgId } }, { type: "text", data: { text } }];
      const result = await call(ctx, "send_private_msg", { user_id: userId, message, passive_only: true });
      return send(res, 200, { message_id: idOf(result.message_id) });
    }
    case "POST /api/chats/send-image": {
      // 发本地图片：请求体是图片文件本身
      const chat = store.get(q.get("key") ?? "");
      if (!chat) {
        throw new HttpError(404, "没有这个会话");
      }
      assertExactId(chat);
      const img = await readBody(req, MAX_IMAGE);
      if (!isImage(img)) {
        throw new HttpError(400, "不是图片文件（支持 PNG、JPG、GIF、WebP、BMP）");
      }
      const message = [{ type: "image", data: { file: "base64://" + img.toString("base64") } }];
      const target = idOf(chat.id);
      const result = chat.type === "group"
        ? await call(ctx, "send_group_msg", { group_id: target, message })
        : await call(ctx, "send_private_msg", { user_id: target, message });
      return send(res, 200, { message_id: idOf(result.message_id) });
    }
    case "POST /api/media/upload": {
      // 上传素材图片，返回 qqbot-media:// 链接，给 Markdown 消息引用
      const img = await readBody(req, MAX_IMAGE);
      if (!isImage(img)) {
        throw new HttpError(400, "不是图片文件（支持 PNG、JPG、GIF、WebP、BMP）");
      }
      const name = (q.get("name") ?? "").slice(0, 100);
      const info = await call<Record<string, unknown>>(ctx, "upload_media_image", { file: "base64://" + img.toString("base64"), name });
      return send(res, 200, { url: String(info.url ?? ""), preview_url: String(info.preview_url ?? ""), cached: !!info.cached });
    }
    case "POST /api/chats/recall": {
      const body = await readJSON(req);
      const id = idOf(body.message_id);
      if (!id) {
        throw new HttpError(400, "缺少消息编号");
      }
      await call(ctx, "delete_msg", { message_id: id });
      return send(res, 200, {});
    }
    case "POST /api/chats/ban": {
      // 禁言群成员：duration 为秒，0 表示解除
      const body = await readJSON(req);
      const chat = store.get(String(body.key ?? ""));
      if (!chat || chat.type !== "group") {
        throw new HttpError(404, "没有这个群");
      }
      const userId = idOf(body.user_id);
      const duration = Math.max(0, Math.min(Math.floor(Number(body.duration) || 0), 30 * 24 * 3600));
      if (!userId) {
        throw new HttpError(400, "缺少用户编号");
      }
      assertExactId(chat);
      await call(ctx, "set_group_ban", { group_id: idOf(chat.id), user_id: userId, duration });
      return send(res, 200, {});
    }
    case "GET /api/chats/group-info": {
      // 群基本信息（官方接口需要白名单），查到的群名记进会话列表
      const chat = store.get(q.get("key") ?? "");
      if (!chat || chat.type !== "group") {
        throw new HttpError(404, "没有这个群");
      }
      // 用 group_openid 查：group_id 超过 JS 数字精度，传过去会变
      const info = await call<Record<string, unknown>>(ctx, "get_group_info", chat.openid ? { group_openid: chat.openid } : { group_id: chat.id });
      const updated = store.setGroupName(chat.key, String(info.group_name ?? ""));
      if (updated) {
        push("chat", updated);
      }
      return send(res, 200, { info, chat: updated ?? chat });
    }
    case "GET /api/chats/user-info": {
      // 用户信息：/v2/users/{user_openid}/info
      const openid = q.get("openid") ?? "";
      const userId = q.get("user_id") ?? "";
      if (!openid && !userId) {
        throw new HttpError(400, "缺少用户");
      }
      const info = await call<Record<string, unknown>>(ctx, "get_user_info", openid ? { user_openid: openid } : { user_id: userId });
      return send(res, 200, { info });
    }
    case "GET /api/stream": {
      res.writeHead(200, { ...SECURITY_HEADERS, "content-type": "text/event-stream; charset=utf-8", connection: "keep-alive" });
      res.write(": ok\n\n");
      streams.add(res);
      const ping = setInterval(() => res.write(": ping\n\n"), 25_000);
      req.on("close", () => {
        clearInterval(ping);
        streams.delete(res);
      });
      return;
    }

    // ---------- 运行状态 ----------
    case "GET /api/info": {
      const [info, cfg] = await Promise.all([
        call(ctx, "host.info"),
        call<{ data: { qq?: { app_id?: unknown } } }>(ctx, "host.config.get"),
      ]);
      // app_id 用来拼用户头像地址 https://q.qlogo.cn/qqapp/<app_id>/<openid>/100
      return send(res, 200, { info, self: ctx.name, app_id: String(cfg.data.qq?.app_id ?? "") });
    }
    case "POST /api/restart":
      await call(ctx, "host.restart", { reason: "WebUI 请求重启" });
      return send(res, 200, {});

    // ---------- 下游连接 ----------
    case "GET /api/connections": {
      const [cfg, info] = await Promise.all([
        call<{ data: { onebot?: Record<string, unknown> } }>(ctx, "host.config.get"),
        call<{ connections: unknown[] }>(ctx, "host.info"),
      ]);
      const ob = cfg.data.onebot ?? {};
      return send(res, 200, {
        access_token: typeof ob.access_token === "string" ? ob.access_token : "",
        heartbeat_interval: typeof ob.heartbeat_interval === "number" ? ob.heartbeat_interval : 5000,
        servers: Array.isArray(ob.servers) ? ob.servers : [],
        status: info.connections,
      });
    }
    case "PUT /api/connections": {
      const body = await readJSON(req);
      const values: Record<string, unknown> = { "onebot.servers": cleanServers(body.servers) };
      if (typeof body.access_token === "string") {
        values["onebot.access_token"] = body.access_token.trim();
      }
      const hb = Number(body.heartbeat_interval);
      if (Number.isInteger(hb) && hb >= 0) {
        values["onebot.heartbeat_interval"] = hb;
      }
      const r = await call(ctx, "host.config.update", { values, restart: false });
      // 等新连接起来再返回状态
      await new Promise((ok) => setTimeout(ok, 300));
      const info = await call<{ connections: unknown[] }>(ctx, "host.info");
      return send(res, 200, { result: r, status: info.connections });
    }

    // ---------- 插件 ----------
    case "GET /api/plugins":
      return send(res, 200, await call(ctx, "plugins.list"));
    case "POST /api/plugins/restart": {
      const body = await readJSON(req);
      await call(ctx, "plugins.restart", { plugin: String(body.plugin ?? "") });
      return send(res, 200, {});
    }
    case "POST /api/plugins/enabled": {
      const body = await readJSON(req);
      return send(res, 200, await call(ctx, "plugins.set_enabled", { plugin: String(body.plugin ?? ""), enabled: body.enabled === true }));
    }
    case "POST /api/plugins/grants": {
      const body = await readJSON(req);
      const grants = body.grants === null ? null : Array.isArray(body.grants) ? body.grants.map(String) : undefined;
      if (grants === undefined) {
        throw new HttpError(400, "grants 应为字符串数组或 null");
      }
      return send(res, 200, await call(ctx, "plugins.set_grants", { plugin: String(body.plugin ?? ""), grants }));
    }
    case "GET /api/plugins/logs":
      return send(res, 200, await call(ctx, "plugins.logs", { plugin: q.get("plugin") ?? "", lines }));

    // ---------- 日志 ----------
    case "GET /api/logs": {
      const args: Record<string, unknown> = { lines };
      if (q.get("date")) {
        args.date = q.get("date");
      }
      return send(res, 200, await call(ctx, "host.logs", args));
    }

    // ---------- 图形化配置 ----------
    case "GET /api/settings": {
      const cfg = await call<{ data: Record<string, any>; path: string }>(ctx, "host.config.get");
      const info = await call<{ idmap?: Record<string, unknown> }>(ctx, "host.info").catch(() => ({}) as { idmap?: Record<string, unknown> });
      const data = cfg.data ?? {};
      const onebot = { ...(data.onebot ?? {}) };
      delete onebot.servers;
      return send(res, 200, {
        path: cfg.path,
        goqbot: { qq: data.qq ?? {}, onebot, idmap: data.idmap ?? {}, log: data.log ?? {}, plugins: data.plugins ?? {} },
        webui: { host: settings.host, port: settings.port },
        commands: settings.commands,
        history: settings.history,
        usage: { chat_bytes: store.usage(), media_bytes: media.usage(), idmap: info.idmap ?? null },
        preview: await statusText(ctx),
      });
    }
    case "PUT /api/settings/goqbot": {
      const body = await readJSON(req);
      const values = cleanGoqbotValues(body.values);
      if (Object.keys(values).length === 0) {
        return send(res, 200, { restart_required: [] });
      }
      return send(res, 200, await call(ctx, "host.config.update", { values, restart: false }));
    }
    case "PUT /api/settings/webui": {
      const body = await readJSON(req);
      let restartNeeded = false;
      if (body.host !== undefined || body.port !== undefined) {
        const host = String(body.host ?? settings.host).trim();
        const port = Number(body.port ?? settings.port);
        if (!host) {
          throw new HttpError(400, "监听地址不能为空");
        }
        if (!Number.isInteger(port) || port < 1 || port > 65535) {
          throw new HttpError(400, "端口要填 1 到 65535 之间的数字");
        }
        restartNeeded = host !== settings.host || port !== settings.port;
        settings.host = host;
        settings.port = port;
      }
      if (body.token !== undefined) {
        const token = String(body.token).trim();
        if (token.length < 8) {
          throw new HttpError(400, "登录口令至少要 8 个字符");
        }
        settings.token = token; // 立即生效
      }
      if (body.commands !== undefined) {
        settings.commands = readCommands(body.commands);
      }
      if (body.history !== undefined) {
        const h = body.history ?? {};
        const check = (v: unknown, name: string, max: number) => {
          if (!Number.isInteger(v) || (v as number) < 0 || (v as number) > max) {
            throw new HttpError(400, `${name}要填 0 到 ${max} 之间的整数`);
          }
          return v as number;
        };
        settings.history = {
          keep_days: check(h.keep_days, "聊天记录保留天数", 36500),
          max_mb: check(h.max_mb, "聊天记录大小上限", 1024 * 1024),
          media_max_mb: check(h.media_max_mb, "图片缓存大小上限", 1024 * 1024),
        };
        applyHistory(settings.history); // 立即生效
      }
      await saveSettings(ctx, settings);
      return send(res, 200, { restart_needed: restartNeeded, commands: settings.commands, history: settings.history });
    }

    // ---------- 更新 ----------
    case "GET /api/update": {
      const info = await call<{ version: string; platform: string }>(ctx, "host.info");
      return send(res, 200, { current: info.version, platform: info.platform, github_proxy: settings.github_proxy });
    }
    case "PUT /api/update/proxy": {
      const body = await readJSON(req);
      const proxy = String(body.github_proxy ?? "").trim();
      if (proxy && !/^https?:\/\/[^\s]+$/.test(proxy)) {
        throw new HttpError(400, "代理地址应以 http:// 或 https:// 开头");
      }
      settings.github_proxy = proxy;
      await saveSettings(ctx, settings);
      return send(res, 200, { github_proxy: proxy });
    }
    case "GET /api/update/check": {
      const info = await call<{ version: string; platform: string }>(ctx, "host.info");
      try {
        return send(res, 200, await checkRelease(info.version, info.platform, settings.github_proxy, process.env.GOQBOT_WEBUI_GITHUB_API));
      } catch (e) {
        throw new HttpError(502, (e as Error).message);
      }
    }
    case "POST /api/update/install": {
      const info = await call<{ version: string; platform: string }>(ctx, "host.info");
      let release;
      try {
        release = await checkRelease(info.version, info.platform, settings.github_proxy, process.env.GOQBOT_WEBUI_GITHUB_API);
      } catch (e) {
        throw new HttpError(502, (e as Error).message);
      }
      if (!release.has_update) {
        throw new HttpError(400, "已经是最新版本");
      }
      if (!release.asset) {
        throw new HttpError(404, `最新版本没有 ${info.platform} 平台的安装包`);
      }
      let data: Buffer;
      try {
        data = await download(release.asset.download_url);
      } catch (e) {
        throw new HttpError(502, (e as Error).message);
      }
      return send(res, 200, await install(ctx, data));
    }
    case "POST /api/update/upload": {
      const data = await readBody(req, MAX_UPLOAD);
      if (data.length === 0) {
        throw new HttpError(400, "没有收到文件");
      }
      return send(res, 200, await install(ctx, data));
    }
  }
  throw new HttpError(404, "没有这个接口");
}

/** 校验并安装新程序；成功后 goqbot 会自动重启 */
async function install(ctx: Context, data: Buffer): Promise<Record<string, unknown>> {
  let staged;
  try {
    staged = await stage(ctx.dataDir, data);
  } catch (e) {
    throw new HttpError(400, (e as Error).message);
  }
  return await call(ctx, "host.update", staged);
}

/** 缓存的图片 /media/<文件名>?s=<签名> */
async function serveMedia(res: ServerResponse, url: URL): Promise<void> {
  const name = url.pathname.slice("/media/".length);
  if (!media || !media.verify(name, url.searchParams.get("s") ?? "")) {
    throw new HttpError(404, "没有这张图片");
  }
  const file = await media.read(name);
  if (!file) {
    throw new HttpError(404, "图片还没下载好或已被清理");
  }
  // 文件名就是内容的 md5，内容不会变，可以让浏览器长期缓存
  res.writeHead(200, { ...SECURITY_HEADERS, "content-type": file.type, "cache-control": "private, max-age=31536000, immutable" });
  res.end(file.data);
}

/** 私聊对象有没有打开机器人的主动消息开关；没收到过开关事件或查不到时为 null */
async function c2cAllow(ctx: Context, target: Record<string, unknown>): Promise<boolean | null> {
  try {
    const st = await call<Record<string, unknown>>(ctx, "get_c2c_msg_state", target);
    return typeof st.allow_proactive_msg === "boolean" ? st.allow_proactive_msg : null;
  } catch {
    return null;
  }
}

/** public/ 下的静态文件 */
async function serveStatic(res: ServerResponse, pathname: string): Promise<void> {
  let rel: string;
  try {
    rel = decodeURIComponent(pathname === "/" ? "/index.html" : pathname);
  } catch {
    throw new HttpError(404, "没有这个页面");
  }
  const file = resolve(PUBLIC_DIR, "." + rel);
  if (!file.startsWith(PUBLIC_DIR + sep)) {
    throw new HttpError(404, "没有这个页面");
  }
  let data: Buffer;
  try {
    data = await readFile(file);
  } catch {
    throw new HttpError(404, "没有这个页面");
  }
  res.writeHead(200, { ...SECURITY_HEADERS, "content-type": MIME[extname(file)] ?? "application/octet-stream" });
  res.end(data);
}

let server: ReturnType<typeof createServer> | undefined;

const plugin: Plugin = {
  requires: [
    "events.subscribe",
    "send_group_msg",
    "send_private_msg",
    "get_group_info",
    "get_user_info",
    "delete_msg",
    "set_group_ban",
    "get_group_bot_state",
    "get_message_quota",
    "get_c2c_msg_state",
    "upload_media_image",
    "host.info",
    "host.logs",
    "host.config.get",
    "host.config.update",
    "host.restart",
    "host.update",
    "plugins.list",
    "plugins.set_enabled",
    "plugins.set_grants",
    "plugins.restart",
    "plugins.logs",
  ],

  async onReady(ctx) {
    let created: boolean;
    try {
      ({ settings, created } = await loadSettings(ctx));
      store = new ChatStore(ctx.dataDir);
      await store.load();
      media = new MediaCache(ctx.dataDir);
      await media.init();
      store.setMedia(media);
      applyHistory(settings.history); // 后台按保留设置清理（旧版聊天记录也在这时拆成按天的文件）
    } catch (e) {
      ctx.log.error("WebUI 没有启动：", (e as Error).message);
      return;
    }

    // 收到的消息和机器人发出的消息都存下来，并推给打开着的页面
    store.subscribe(({ chat, message }) => push("message", { chat, message }));
    // 旧版数据合并到精确编号的会话时，通知页面换 key
    store.onRename((from, to) => push("chat_renamed", { from, chat: store.get(to) }));
    // 新出现的群自动查一次群名（官方接口需要白名单；没有权限时不再查）
    const askedGroups = new Set<string>();
    let groupInfoDenied = false;
    store.subscribe(({ chat }) => {
      if (chat.type !== "group" || chat.name || groupInfoDenied || askedGroups.has(chat.key)) {
        return;
      }
      askedGroups.add(chat.key);
      ctx.call<Record<string, unknown>>("get_group_info", chat.openid ? { group_openid: chat.openid } : { group_id: chat.id }).then((info) => {
        const updated = store.setGroupName(chat.key, String(info.group_name ?? ""));
        if (updated?.name) {
          push("chat", updated);
        }
      }).catch((e: GoqbotError) => {
        const text = String(e.data?.detail ?? e.data?.wording ?? e.message ?? "");
        if (/11253|权限|白名单/.test(text)) {
          groupInfoDenied = true;
          ctx.log.info("机器人没有群基本信息接口的权限（错误码 11253），群聊列表显示群编号；可以在群聊页给群设置备注");
        }
      });
    });
    // goqbot 发消息失败（或部分没生效）时的通知：在对应会话里显示一条灰字提示
    await ctx.on(["notice.send_failed"], async (ev) => {
      try {
        await store.addNotice(ev);
      } catch (e) {
        ctx.log.error("保存发送失败提示出错：", (e as Error).message);
      }
    });
    await ctx.on(["message.*", "message_sent.*"], async (ev) => {
      try {
        await store.add(ev);
      } catch (e) {
        ctx.log.error("保存消息失败：", (e as Error).message);
      }
      try {
        await handleCommand(ctx, ev);
      } catch (e) {
        const err = e as GoqbotError;
        ctx.log.warn("回复内置指令失败：", err.data?.wording ?? err.data?.message ?? err.message);
      }
    });

    server = createServer(async (req, res) => {
      const url = new URL(req.url ?? "/", "http://localhost");
      const ip = req.socket.remoteAddress ?? "";
      try {
        if (url.pathname.startsWith("/media/")) {
          // 缓存的图片：<img> 带不了登录口令，用地址里的签名校验
          return await serveMedia(res, url);
        }
        if (!url.pathname.startsWith("/api/")) {
          return await serveStatic(res, url.pathname);
        }
        if (tooManyFailures(ip)) {
          throw new HttpError(429, "登录失败次数太多，请 5 分钟后再试");
        }
        const auth = req.headers.authorization ?? "";
        const given = auth.startsWith("Bearer ") ? auth.slice(7) : "";
        if (!tokenMatches(given, settings.token)) {
          recordFailure(ip);
          throw new HttpError(401, "口令不正确");
        }
        failures.delete(ip);
        if (url.pathname === "/api/login") {
          return send(res, 200, {});
        }
        await handleApi(ctx, req, res, url);
      } catch (e) {
        if (res.headersSent) {
          res.end();
          return;
        }
        if (e instanceof HttpError) {
          return send(res, e.status, { error: e.message });
        }
        ctx.log.error("WebUI 处理请求出错：", e);
        send(res, 500, { error: "服务出错，详情见 goqbot 日志" });
      }
    });
    server.requestTimeout = 15 * 60_000; // 上传新程序可能比较慢

    server.on("error", (e: NodeJS.ErrnoException) => {
      if (e.code === "EADDRINUSE") {
        ctx.log.error(`WebUI 端口 ${settings.port} 已被占用，请修改 ${join(ctx.dataDir, "config.json")} 里的 port 后重启 goqbot`);
      } else {
        ctx.log.error("WebUI 启动失败：", e.message);
      }
    });

    server.listen(settings.port, settings.host, () => {
      const shown = settings.host === "0.0.0.0" || settings.host === "::" ? "127.0.0.1" : settings.host;
      ctx.log.info(`WebUI 已启动：http://${shown}:${settings.port}`);
      if (created) {
        ctx.log.info(`首次登录请打开：http://${shown}:${settings.port}/#token=${settings.token}`);
        ctx.log.info(`登录口令、监听地址和端口保存在 ${join(ctx.dataDir, "config.json")}，可以自行修改后重启 goqbot`);
      }
    });
  },

  async onShutdown() {
    for (const res of streams) {
      res.end();
    }
    store?.stop();
    await store?.save().catch(() => {});
    server?.close();
    server?.closeAllConnections();
  },
};

export default plugin;
