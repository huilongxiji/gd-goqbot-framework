"""板块七：发送模式 —— 被动 / 主动推送 / 互动召回(is_wakeup) / 单聊流式消息。"""

import asyncio
from typing import Any

from nonebot.adapters.onebot.v11 import Message, MessageSegment

from ..common import Ctx, Section, b64, make_png, md_seg

sec = Section("发送测试", "发送模式", "被动 / 主动 / 互动召回 / 单聊流式（stream 段）")

STREAM_TEXT = ["流式", "消息", "测试：", "这是一段", "逐帧追加的", "文本。"]


def stream_seg(state: int, index: int, sid: str = "", **extra: Any) -> MessageSegment:
    data = {"state": state, "index": index, **extra}
    if sid:
        data["id"] = sid
    return MessageSegment("stream", data)


async def send_stream(
    ctx: Ctx,
    frames: list[str | MessageSegment],
    *,
    states: list[int] | None = None,
    interval: float = 0.6,
    **extra: Any,
) -> None:
    """逐帧发送：首帧不带 id，取回 stream_id 后续帧回填；末帧 state=10（可用 states 覆盖）。"""
    sid, last = "", {}
    for i, body in enumerate(frames):
        state = states[i] if states else (10 if i == len(frames) - 1 else 1)
        msg = stream_seg(state, i, sid, **extra) + Message(body)
        r = await ctx.call("send_private_msg", user_id=ctx.user_id, message=msg)
        if not r.ok:
            await ctx.send(f"第 {i} 帧失败：{r.brief()}")
            return
        last = r.data or {}
        sid = sid or last.get("stream_id", "")
        if not sid:
            await ctx.send(f"首帧未返回 stream_id：{last}")
            return
        await asyncio.sleep(interval)
    await ctx.send(f"✅ 流式结束 帧数={len(frames)} stream_id={sid[:16]}… remain_msg_len={last.get('remain_msg_len')}")


@sec.case("被动", "普通回复：自动套用 msg_id / event_id 被动凭证")
async def _(ctx: Ctx):
    await ctx.send("被动回复（goqbot 日志中模式应为「被动 msg_id=…」）")


@sec.case("主动群", "send_group_active_msg：强制主动推送（忽略 reply / lazy 凭证）", scene="group")
async def _(ctx: Ctx):
    await ctx.run(
        "send_group_active_msg",
        "主动群消息",
        group_id=ctx.group_id,
        message=MessageSegment.reply(ctx.event.message_id) + "主动群消息（reply 应被忽略，模式=主动）",
    )


@sec.case("主动私聊", "send_private_active_msg：强制主动单聊推送")
async def _(ctx: Ctx):
    await ctx.run("send_private_active_msg", "主动私聊", user_id=ctx.user_id, message="主动单聊推送测试（模式=主动）")


@sec.case("对照", "send_msg（被动）与 send_group_active_msg（主动）各发一条，对照日志模式", scene="group")
async def _(ctx: Ctx):
    await ctx.run("send_msg", "send_msg(被动)", message_type="group", group_id=ctx.group_id, message="send_msg 被动")
    await ctx.run("send_group_active_msg", "active(主动)", group_id=ctx.group_id, message="send_group_active_msg 主动")


@sec.case("召回", "is_wakeup=true 互动召回（仅单聊；每周期限 1 条，超限返回平台错误）", scene="private")
async def _(ctx: Ctx):
    await ctx.run("send_private_msg", "互动召回 is_wakeup", user_id=ctx.user_id, is_wakeup=True, message="互动召回测试消息")


@sec.case("流式", "stream 增量（append）纯文本，逐帧打字机输出", scene="private")
async def _(ctx: Ctx):
    await send_stream(ctx, STREAM_TEXT)


@sec.case("流式替换", "input_mode=replace：每帧为当前全量，须以已发送内容为前缀", scene="private")
async def _(ctx: Ctx):
    frames = ["".join(STREAM_TEXT[: i + 1]) for i in range(len(STREAM_TEXT))]
    await send_stream(ctx, frames, input_mode="replace")


@sec.case("流式旧字段", "旧字段 reset=true（等同 replace）+ 旧 state 值 11/20", scene="private")
async def _(ctx: Ctx):
    await send_stream(ctx, ["旧字段", "旧字段兼容", "旧字段兼容测试完成"], states=[11, 11, 20], reset=True)


@sec.case("流式md", "content_type=markdown，正文为 markdown 文本", scene="private")
async def _(ctx: Ctx):
    parts = ["## 流式 Markdown\n", "- **第一项**\n", "- *第二项*\n", "- `第三项`\n", "\n> 完成"]
    await send_stream(ctx, parts, content_type="markdown")


@sec.case("流式md段", "stream 段 + markdown 段（未指定 content_type 时自动按 markdown）", scene="private")
async def _(ctx: Ctx):
    await send_stream(ctx, [md_seg("**加粗开头**"), md_seg("，随后 *斜体*"), md_seg("，结束。")])


@sec.case("流式缺id", "续帧 index=1 不带 id，期望 retcode=1400 被框架拒绝", scene="private")
async def _(ctx: Ctx):
    r = await ctx.call("send_private_msg", user_id=ctx.user_id, message=stream_seg(1, 1) + "缺 id 的续帧")
    ok = not r.ok and r.retcode == 1400
    await ctx.send(("✅ 符合预期\n" if ok else "❌ 不符合预期\n") + r.brief())


@sec.case("流式群", "群内带 stream 段：应忽略 stream、按普通消息发送", scene="group")
async def _(ctx: Ctx):
    await ctx.send(stream_seg(1, 0) + "群聊不支持流式，本条应作为普通消息发出")


@sec.case("流式图片", "stream 帧夹带图片：图片被忽略并告警，仅文字下发", scene="private")
async def _(ctx: Ctx):
    r = await ctx.call(
        "send_private_msg",
        user_id=ctx.user_id,
        message=stream_seg(10, 0) + "流式只发文字，图片应被忽略" + MessageSegment.image(b64(make_png(32, 32))),
    )
    await ctx.send(r.brief())
