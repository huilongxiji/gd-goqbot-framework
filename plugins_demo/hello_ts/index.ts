// hello_ts：goqbot TypeScript 插件最小示例。复制到 plugins/ 下、重启 goqbot 即可运行，不需要编译。
//
// 演示内容：
//  1. 提供能力 hello_ts_echo：载入后成为 OneBot 动作，下游可直接 call_api("hello_ts_echo", text="你好")。
//  2. 订阅群消息：有人发 /hello_ts 时，调用 goqbot 的 send_group_msg 引用回复。
//  3. 回复后用 ctx.emit 发出 hello_ts_replied 事件，所有下游框架都会收到。
//  4. 把回复次数存在 ctx.dataDir 里（更新插件时不会丢）。
import { readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import type { Context, Plugin } from "../goqbot.js";

/** 读取已回复次数 */
async function loadCount(ctx: Context): Promise<number> {
  try {
    const saved = JSON.parse(await readFile(join(ctx.dataDir, "count.json"), "utf8"));
    return typeof saved.count === "number" ? saved.count : 0;
  } catch {
    return 0; // 第一次运行还没有文件
  }
}

const plugin: Plugin = {
  requires: ["send_group_msg", "events.subscribe", "events.emit"],

  events: [{ name: "hello_ts_replied", description: "插件回复了一次 /hello_ts" }],

  capabilities: {
    hello_ts_echo: {
      description: "原样返回传入的文字，用来测试插件能力调用",
      inputSchema: {
        type: "object",
        properties: { text: { type: "string" } },
        required: ["text"],
        additionalProperties: false,
      },
      async handler(args) {
        if (typeof args.text !== "string") {
          throw new Error("text 必须是字符串");
        }
        return { text: args.text, from: "hello_ts" };
      },
    },
  },

  async onReady(ctx) {
    let count = await loadCount(ctx);
    ctx.log.info(`hello_ts 已就绪，之前回复过 ${count} 次`);

    await ctx.on("message.group", async (ev) => {
      if (ev.raw_message?.trim() !== "/hello_ts") {
        return;
      }
      try {
        // 引用对方的消息回复：goqbot 会自动使用这条消息的被动回复凭证
        const sent = await ctx.call("send_group_msg", {
          group_id: ev.group_id,
          message: [
            { type: "reply", data: { id: String(ev.message_id) } },
            { type: "text", data: { text: `你好，我是 hello_ts 插件（第 ${count + 1} 次回复）` } },
          ],
        });
        count++;
        await writeFile(join(ctx.dataDir, "count.json"), JSON.stringify({ count }));
        // 通知所有下游框架
        await ctx.emit("hello_ts_replied", { group_id: ev.group_id, message_id: sent.message_id });
      } catch (e) {
        ctx.log.warn("回复 /hello_ts 失败：", (e as Error).message);
      }
    });
  },

  async onShutdown(ctx) {
    ctx.log.info("hello_ts 正在退出");
  },
};

export default plugin;
