// goqbot JS / TS 插件的类型说明，随 goqbot 发布包放在 plugins/goqbot.d.ts。
// 只用于编辑器提示和类型检查；插件里用 `import type { Plugin } from "../goqbot.js"` 引用（要写 .js，指的就是这个文件），运行时会被去掉。
// 示例见 examples/plugins/hello_ts。

/** JSON Schema（只要求是对象，具体写法同 JSON Schema 规范） */
export type JSONSchema = { [key: string]: unknown };

/** 一项能力的声明（goqbot 提供的、其他插件提供的都是这个格式） */
export interface CapabilityInfo {
  name: string;
  description: string;
  input_schema: JSONSchema;
  output_schema?: JSONSchema;
  sensitive: boolean;
}

/**
 * 编号（group_id、user_id、message_id 等）超过 JS 安全整数范围（2^53）时是字符串，否则是数字。
 * 调用 goqbot 能力时原样传回即可（字符串也能识别）；比较时先统一成字符串：String(ev.group_id)。
 */
export type ID = number | string;

/** OneBot V11 事件（字段随事件类型不同，常用字段列在这里） */
export interface OneBotEvent {
  time: number;
  self_id: ID;
  post_type: "message" | "notice" | "request" | string;
  message_type?: "group" | "private" | string;
  notice_type?: string;
  request_type?: string;
  sub_type?: string;
  group_id?: ID;
  user_id?: ID;
  message_id?: ID;
  raw_message?: string;
  message?: unknown;
  /** 插件发出的自定义事件（notice_type 为 plugin_event）：发出它的插件名 */
  plugin?: string;
  /** 插件发出的自定义事件：附带的数据 */
  data?: Record<string, unknown>;
  [key: string]: unknown;
}

/** ctx.call 失败时抛出的错误 */
export interface GoqbotError extends Error {
  /** 错误码，如 -32003（没有授权）、-32010（OneBot 动作失败） */
  code: number;
  /** 固定错误码，如 capability_forbidden、action_failed */
  message: string;
  /** 附加信息：OneBot 动作失败时带 retcode、message、wording；goqbot 能说明原因时带 detail */
  data?: { code?: string; wording?: string; detail?: string; retcode?: number; message?: string; [key: string]: unknown };
}

/** 插件拿到的上下文 */
export interface Context {
  /** 插件名（文件夹名） */
  readonly name: string;
  /** 插件文件夹的绝对路径（也是插件的当前目录） */
  readonly dir: string;
  /** 数据目录 plugin-data/<插件名>/ 的绝对路径，已自动创建；插件更新时不会被覆盖 */
  readonly dataDir: string;
  /** goqbot 的信息 */
  readonly host: { name: string; version: string; selfId: number };
  /** goqbot 开始退出时触发 */
  readonly signal: AbortSignal;

  /** 调用注册表里的一项能力（OneBot 动作、其他插件的能力、host.* 等）。失败时抛出 GoqbotError */
  call<T extends Record<string, unknown> = Record<string, any>>(name: string, args?: Record<string, unknown>): Promise<T>;

  /**
   * 订阅事件。types 写成 post_type.子类型，如 "message.group"、"notice.*"、"*"。
   * 机器人自己发出的消息是 "message_sent.group" / "message_sent.private"（只推给插件）。
   * 返回取消订阅的函数。
   */
  on(types: string | string[], handler: (event: OneBotEvent) => void | Promise<void>): Promise<() => Promise<void>>;

  /** 发出自定义事件，返回送达的下游连接数 */
  emit(event: string, data?: Record<string, unknown>): Promise<number>;

  /** 当前能调用的全部能力 */
  capabilities(): Promise<CapabilityInfo[]>;

  /** 写进 goqbot 日志，带 [plugin:<插件名>] 前缀 */
  readonly log: {
    info(...args: unknown[]): void;
    warn(...args: unknown[]): void;
    error(...args: unknown[]): void;
  };
}

/** 插件提供的一项能力 */
export interface Capability {
  /** 中文说明 */
  description: string;
  /** 参数的 JSON Schema，默认 { type: "object" }。goqbot 不替你校验参数 */
  inputSchema?: JSONSchema;
  /** 返回结果的 JSON Schema，只用于展示 */
  outputSchema?: JSONSchema;
  /** 是否敏感（返回凭证或能改动 goqbot），默认 false */
  sensitive?: boolean;
  /** 处理调用，返回一个对象；抛出错误时调用方收到 capability_call_failed（错误原文不会传出去） */
  handler(args: Record<string, any>, ctx: Context): Record<string, unknown> | Promise<Record<string, unknown>>;
}

/** 插件对象：入口文件的默认导出 */
export interface Plugin {
  /** 需要调用的 goqbot 能力，支持 * 通配（不匹配敏感能力，敏感能力要写全名） */
  requires?: string[];
  /** 提供的能力：键是能力名（小写字母、数字、下划线，以字母开头，最长 64 个字符），会注册成同名的 OneBot 动作 */
  capabilities?: Record<string, Capability>;
  /** 可能通过 ctx.emit 发出的事件，只用于展示。name 的规则同能力名 */
  events?: { name: string; description: string }[];
  /** 同时处理几个能力调用，1～64，默认 4 */
  maxConcurrency?: number;
  /** 握手完成后调用 */
  onReady?(ctx: Context): void | Promise<void>;
  /** goqbot 退出或重启前调用，有 1 秒时间清理 */
  onShutdown?(ctx: Context): void | Promise<void>;
}
