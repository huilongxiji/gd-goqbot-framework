// goqbot 内置的 JS / TS 插件启动器。
//
// 由 goqbot 编进程序、启动时自动写出（plugin-data/.goqbot/goqbot-runner.mjs），修改这个写出的文件没有用。
// goqbot 这样启动每个 JS / TS 插件：node goqbot-runner.mjs <插件入口文件>，工作目录为插件文件夹。
// 启动器负责 goqbot-plugin/2 协议（标准输入输出上按行分隔的 JSON-RPC 2.0），插件只需默认导出一个插件对象，
// 写法见 examples/plugins/hello_ts 和 goqbot.d.ts。
import { createInterface } from "node:readline";
import { pathToFileURL } from "node:url";
import { readFileSync } from "node:fs";
import { basename, join } from "node:path";
import { format } from "node:util";

const MAX_FRAME = 2 * 1024 * 1024 - 1; // 单条消息上限（不含换行）
const ACTION_NAME = /^[a-z][a-z0-9_]{0,63}$/; // 能力名 / 事件名，与 goqbot 的规则一致
const PLUGIN_NAME = /^[a-z0-9_]{1,64}$/;
const GRANT_NAME = /^[a-z0-9_.*-]{1,128}$/;
const entry = process.argv[2];

// ---------------- 输出 ----------------

// 标准输出只用来和 goqbot 通信：先留住原始的写函数，再把插件的输出都转进日志
const rawWrite = process.stdout.write.bind(process.stdout);

/** 写出一条协议消息；超过大小上限时返回 false */
function send(msg, callback) {
  let line;
  try {
    line = JSON.stringify(msg);
  } catch {
    return false;
  }
  if (Buffer.byteLength(line) > MAX_FRAME) {
    return false;
  }
  rawWrite(line + "\n", callback);
  return true;
}

/** 写一条日志到 goqbot（goqbot 会加上 [plugin:<插件名>] 前缀） */
function log(level, args, callback) {
  let message = format(...args);
  if (message.length > 4096) {
    message = message.slice(0, 4096) + "…";
  }
  if (!send({ jsonrpc: "2.0", method: "notifications/log", params: { level, message } }, callback) && callback) {
    callback();
  }
}

console.log = console.info = console.debug = (...args) => log("info", args);
console.warn = (...args) => log("warning", args);
console.error = console.trace = (...args) => log("error", args);

// 插件直接写 process.stdout 的内容按行转进日志
let stdoutBuffer = "";
process.stdout.write = (chunk, encoding, callback) => {
  if (typeof encoding === "function") {
    callback = encoding;
    encoding = undefined;
  }
  stdoutBuffer += typeof chunk === "string" ? chunk : Buffer.from(chunk).toString(typeof encoding === "string" ? encoding : "utf8");
  let i;
  while ((i = stdoutBuffer.indexOf("\n")) >= 0) {
    log("info", [stdoutBuffer.slice(0, i)]);
    stdoutBuffer = stdoutBuffer.slice(i + 1);
  }
  if (callback) {
    callback();
  }
  return true;
};

/** 写完日志后退出（macOS 上管道是异步写的，直接退出可能丢掉最后的日志） */
function fatal(what, error) {
  log("error", [what, error], () => process.exit(1));
  setTimeout(() => process.exit(1), 1000).unref();
}

process.on("uncaughtException", (error) => fatal("插件出现没有捕获的异常，即将退出（goqbot 会自动重启它）：", error));
process.on("unhandledRejection", (error) => fatal("插件有没有处理的 Promise 错误，即将退出（goqbot 会自动重启它）：", error));

// ---------------- 调用 goqbot ----------------

/** ctx.call 失败时抛出的错误 */
class GoqbotError extends Error {
  constructor(code, message, data) {
    super(message);
    this.name = "GoqbotError";
    this.code = code;
    this.data = data;
  }
}

/** 返回给 goqbot 的协议错误 */
class RpcError extends Error {
  constructor(code, message) {
    super(message);
    this.code = code;
  }
}

let nextId = 0;
const pending = new Map();

/** 向 goqbot 发请求并等待结果 */
function request(method, params) {
  return new Promise((resolve, reject) => {
    const id = ++nextId;
    pending.set(id, { resolve, reject });
    if (!send({ jsonrpc: "2.0", id, method, params })) {
      pending.delete(id);
      reject(new GoqbotError(-32602, "invalid_arguments", { code: "invalid_arguments", detail: "参数无法编码成 JSON，或超过 2 MiB" }));
    }
  });
}

// ---------------- 插件 ----------------

let plugin = null; // 插件对象
let ctx = null; // 交给插件的上下文
let capabilityList = []; // 握手时声明的能力
const abort = new AbortController();
const subscriptions = new Map(); // 订阅号 -> 处理函数
const earlyEvents = new Map(); // 订阅号还没登记时先到的事件（订阅的回复和第一个事件可能交错）

/** 读取 package.json 里的 name、version */
function readPackage(dir) {
  try {
    return JSON.parse(readFileSync(join(dir, "package.json"), "utf8"));
  } catch {
    return {};
  }
}

/** 插件对象写错时给出的说明 */
function invalid(message) {
  return new Error("插件对象写法不对：" + message);
}

/** 检查插件对象，整理成握手要声明的内容 */
function describePlugin(p) {
  if (!p || typeof p !== "object") {
    throw invalid("入口文件需要用 export default（CommonJS 为 module.exports =）导出一个插件对象");
  }
  const caps = p.capabilities ?? {};
  if (typeof caps !== "object" || Array.isArray(caps)) {
    throw invalid("capabilities 应该是对象：键是能力名，值是 { description, handler }");
  }
  const list = [];
  for (const [name, cap] of Object.entries(caps)) {
    if (!ACTION_NAME.test(name)) {
      throw invalid(`能力名 ${name} 只能由小写字母、数字、下划线组成，以字母开头，最长 64 个字符`);
    }
    if (!cap || typeof cap.handler !== "function") {
      throw invalid(`能力 ${name} 缺少 handler 函数`);
    }
    if (typeof cap.description !== "string" || cap.description.length > 1024) {
      throw invalid(`能力 ${name} 的 description 必须是字符串，且不超过 1024 个字符`);
    }
    for (const key of ["inputSchema", "outputSchema"]) {
      if (cap[key] !== undefined && (typeof cap[key] !== "object" || cap[key] === null || Array.isArray(cap[key]))) {
        throw invalid(`能力 ${name} 的 ${key} 必须是对象`);
      }
    }
    const item = { name, description: cap.description, input_schema: cap.inputSchema ?? { type: "object" }, sensitive: cap.sensitive === true };
    if (cap.outputSchema) {
      item.output_schema = cap.outputSchema;
    }
    list.push(item);
  }
  if (list.length > 128) {
    throw invalid("能力最多 128 个");
  }
  const requires = p.requires ?? [];
  if (!Array.isArray(requires) || requires.length > 256 || !requires.every((r) => typeof r === "string" && GRANT_NAME.test(r))) {
    throw invalid("requires 应该是能力名组成的数组（可以带 *），最多 256 项");
  }
  const events = p.events ?? [];
  if (!Array.isArray(events) || events.length > 64 ||
      !events.every((e) => e && ACTION_NAME.test(e.name) && typeof e.description === "string" && e.description.length <= 1024)) {
    throw invalid("events 应该是 [{ name, description }] 数组，最多 64 项，name 只能由小写字母、数字、下划线组成");
  }
  const concurrency = p.maxConcurrency ?? 4;
  if (!Number.isInteger(concurrency) || concurrency < 1 || concurrency > 64) {
    throw invalid("maxConcurrency 必须是 1～64 的整数");
  }
  for (const hook of ["onReady", "onShutdown"]) {
    if (p[hook] !== undefined && typeof p[hook] !== "function") {
      throw invalid(`${hook} 必须是函数`);
    }
  }
  return { capabilities: list, requires: [...requires], events: events.map((e) => ({ name: e.name, description: e.description })), concurrency };
}

/** 创建交给插件的 ctx */
function createContext(params) {
  const info = params.plugin ?? {};
  const host = params.host ?? {};
  const context = {
    name: info.name ?? basename(process.cwd()),
    dir: info.dir ?? process.cwd(),
    dataDir: info.data_dir ?? process.cwd(),
    host: Object.freeze({ name: host.name ?? "goqbot", version: host.version ?? "", selfId: host.self_id ?? 0 }),
    signal: abort.signal,
    call(name, args = {}) {
      if (typeof name !== "string" || name === "") {
        return Promise.reject(new TypeError("ctx.call 的第一个参数必须是能力名"));
      }
      return request("capabilities/call", { name, arguments: args ?? {} });
    },
    async on(types, handler) {
      if (typeof handler !== "function") {
        throw new TypeError("ctx.on 的第二个参数必须是函数");
      }
      const list = Array.isArray(types) ? types : [types];
      const result = await context.call("events.subscribe", { types: list });
      const id = result.subscription_id;
      subscriptions.set(id, handler);
      for (const event of earlyEvents.get(id) ?? []) {
        deliver(handler, event);
      }
      earlyEvents.delete(id);
      return async () => {
        if (subscriptions.delete(id)) {
          await context.call("events.unsubscribe", { subscription_id: id });
        }
      };
    },
    async emit(event, data = {}) {
      const result = await context.call("events.emit", { event, data });
      return result.delivered;
    },
    async capabilities() {
      const result = await request("capabilities/list", {});
      return result.capabilities;
    },
    log: Object.freeze({
      info: (...args) => log("info", args),
      warn: (...args) => log("warning", args),
      error: (...args) => log("error", args),
    }),
  };
  return Object.freeze(context);
}

/** 把事件交给订阅它的处理函数，出错只记日志 */
function deliver(handler, event) {
  Promise.resolve()
    .then(() => handler(event))
    .catch((error) => log("error", ["处理事件时出错：", error]));
}

/** 能力的返回值整理成对象 */
function normalizeResult(value) {
  if (value === undefined || value === null) {
    return {};
  }
  if (typeof value === "object" && !Array.isArray(value)) {
    return value;
  }
  return { value };
}

// ---------------- 处理 goqbot 发来的消息 ----------------

async function dispatch(method, params) {
  switch (method) {
    case "initialize": {
      if (params.protocol_version !== "2") {
        throw new RpcError(-32602, "unsupported_protocol");
      }
      let mod;
      try {
        mod = await import(pathToFileURL(entry).href);
      } catch (error) {
        log("error", ["载入插件失败（入口文件 " + entry + "）：", error]);
        throw new RpcError(-32000, "load_failed");
      }
      plugin = mod.default;
      let described;
      try {
        described = describePlugin(plugin);
      } catch (error) {
        log("error", [error.message]);
        throw new RpcError(-32000, "invalid_plugin");
      }
      ctx = createContext(params);
      capabilityList = described.capabilities;
      const pkg = readPackage(ctx.dir);
      const name = typeof pkg.name === "string" && PLUGIN_NAME.test(pkg.name) ? pkg.name : PLUGIN_NAME.test(ctx.name) ? ctx.name : "plugin";
      const version = typeof pkg.version === "string" && pkg.version !== "" && pkg.version.length <= 64 ? pkg.version : "0.0.0";
      return {
        protocol_version: "2",
        name,
        version,
        max_concurrency: described.concurrency,
        capabilities: capabilityList,
        requires: described.requires,
        events: described.events,
      };
    }
    case "capabilities/list":
      return { capabilities: capabilityList };
    case "capabilities/call": {
      const capability = plugin?.capabilities?.[params.name];
      if (!capability || !capabilityList.some((c) => c.name === params.name)) {
        throw new RpcError(-32002, "capability_not_found");
      }
      try {
        return normalizeResult(await capability.handler(params.arguments ?? {}, ctx));
      } catch (error) {
        // 错误原文只写进日志，不传给调用方
        log("error", [`能力 ${params.name} 出错：`, error]);
        throw new RpcError(-32000, "capability_error");
      }
    }
    case "ping":
      return {};
    case "shutdown":
      abort.abort();
      if (plugin?.onShutdown) {
        await Promise.race([
          Promise.resolve().then(() => plugin.onShutdown(ctx)).catch((error) => log("error", ["onShutdown 出错：", error])),
          new Promise((resolve) => setTimeout(resolve, 900).unref()),
        ]);
      }
      return {};
    default:
      throw new RpcError(-32601, "method_not_found");
  }
}

async function handleRequest(msg) {
  let reply;
  try {
    reply = { jsonrpc: "2.0", id: msg.id, result: await dispatch(msg.method, msg.params ?? {}) };
  } catch (error) {
    if (!(error instanceof RpcError)) {
      log("error", ["处理请求时出错：", error]);
      error = new RpcError(-32000, "internal_error");
    }
    reply = { jsonrpc: "2.0", id: msg.id, error: { code: error.code, message: error.message } };
  }
  if (!send(reply)) {
    log("error", ["结果无法编码成 JSON，或超过 2 MiB"]);
    send({ jsonrpc: "2.0", id: msg.id, error: { code: -32000, message: "result_too_large" } });
  }
  if (msg.method === "shutdown") {
    setTimeout(() => process.exit(0), 50).unref();
  }
}

function handleNotification(msg) {
  const params = msg.params ?? {};
  switch (msg.method) {
    case "notifications/initialized":
      if (plugin?.onReady) {
        Promise.resolve()
          .then(() => plugin.onReady(ctx))
          .catch((error) => log("error", ["onReady 出错：", error]));
      }
      break;
    case "notifications/event": {
      const handler = subscriptions.get(params.subscription_id);
      if (handler) {
        deliver(handler, params.event);
      } else {
        const list = earlyEvents.get(params.subscription_id) ?? [];
        if (list.length < 100) {
          list.push(params.event);
          earlyEvents.set(params.subscription_id, list);
        }
      }
      break;
    }
    // notifications/cancelled、notifications/capabilities/list_changed：无需处理
  }
}

function handleResponse(msg) {
  const waiter = pending.get(msg.id);
  if (!waiter) {
    return;
  }
  pending.delete(msg.id);
  if (msg.error) {
    waiter.reject(new GoqbotError(msg.error.code, msg.error.message, msg.error.data));
  } else {
    waiter.resolve(msg.result ?? {});
  }
}

/**
 * 超过 JS 安全整数范围（2^53）的整数保留成字符串，避免丢精度。
 * goqbot 的 group_id、user_id、message_id 等编号有 19 位，按数字解析最后几位会变；
 * 字符串原样传回 goqbot 时也能正确识别。
 */
function keepBigInt(_key, value, context) {
  if (typeof value === "number" && !Number.isSafeInteger(value) && /^-?\d+$/.test(context?.source ?? "")) {
    return context.source;
  }
  return value;
}

if (!entry) {
  log("error", ["goqbot-runner 需要插件入口文件作为参数"], () => process.exit(1));
} else {
  const lines = createInterface({ input: process.stdin, crlfDelay: Infinity });
  lines.on("line", (line) => {
    if (line.trim() === "") {
      return;
    }
    let msg;
    try {
      msg = JSON.parse(line, keepBigInt);
    } catch {
      return;
    }
    if (msg.method !== undefined && msg.id !== undefined) {
      handleRequest(msg);
    } else if (msg.method !== undefined) {
      handleNotification(msg);
    } else if (msg.id !== undefined) {
      handleResponse(msg);
    }
  });
  // goqbot 退出时标准输入关闭，插件随之退出
  lines.on("close", () => process.exit(0));
}
