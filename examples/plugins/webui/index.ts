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
import { ChatStore, type ChatType } from "./lib/chat.ts";
import { checkRelease, download, stage } from "./lib/update.ts";

/** WebUI 自己的设置，保存在 plugin-data/webui/config.json */
interface Settings {
  host: string;
  port: number;
  token: string;
  github_proxy: string; // 下载更新时加在 GitHub 地址前的代理，空=直连
  commands: Commands;
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
    const settings: Settings = { host: "127.0.0.1", port: 6099, token: randomBytes(12).toString("base64url"), github_proxy: "", commands: readCommands(undefined) };
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
  "onebot.message_format": "string",
  "onebot.debug": "bool",
  "onebot.downtime_message": "string",
  "idmap.disabled": "bool",
  "idmap.path": "string",
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
      const data = cfg.data ?? {};
      const onebot = { ...(data.onebot ?? {}) };
      delete onebot.servers;
      return send(res, 200, {
        path: cfg.path,
        goqbot: { qq: data.qq ?? {}, onebot, idmap: data.idmap ?? {}, log: data.log ?? {}, plugins: data.plugins ?? {} },
        webui: { host: settings.host, port: settings.port },
        commands: settings.commands,
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
      await saveSettings(ctx, settings);
      return send(res, 200, { restart_needed: restartNeeded, commands: settings.commands });
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
    } catch (e) {
      ctx.log.error("WebUI 没有启动：", (e as Error).message);
      return;
    }

    // 收到的消息和机器人发出的消息都存下来，并推给打开着的页面
    store.subscribe(({ chat, message }) => push("message", { chat, message }));
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
    await store?.save().catch(() => {});
    server?.close();
    server?.closeAllConnections();
  },
};

export default plugin;
