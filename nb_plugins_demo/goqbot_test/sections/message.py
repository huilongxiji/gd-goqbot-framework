"""板块二：消息段 —— text / at / reply / CQ 字符串 / 多段顺序 / 空消息。"""

from nonebot.adapters.onebot.v11 import MessageSegment

from ..common import Ctx, Section, b64, make_png

sec = Section("消息测试", "消息段", "文本、@、引用回复、CQ 码字符串入参、多段拆分顺序与异常入参")


@sec.case("文本", "纯文本被动回复（自动套 msg_id）")
async def _(ctx: Ctx):
    r = await ctx.send("纯文本测试：Hello goqbot 👋\n第二行 & 特殊字符 [] ,")
    await ctx.send(f"返回：{r}")


@sec.case("at", "@ 发送者（转 <qqbot-at-user/>），并验证 at 自己 / at all 被跳过", scene="group")
async def _(ctx: Ctx):
    msg = (
        MessageSegment.at(ctx.user_id)
        + " 这是 at 测试；后面的 at 机器人自身与 at 全体应被跳过："
        + MessageSegment.at(int(ctx.bot.self_id))
        + MessageSegment.at("all")
    )
    await ctx.send(msg)


@sec.case("at他人", "@ 参数里的成员（需对方在本群发过消息，才能反查 openid）", scene="group")
async def _(ctx: Ctx):
    targets = ctx.at_targets()
    if not targets:
        await ctx.send("用法：/消息测试 at他人 @某人 [@某人…]")
        return
    msg = MessageSegment.text("点名：")
    for uid in targets:
        msg += MessageSegment.at(uid) + " "
    await ctx.send(msg)


@sec.case("引用", "reply 段引用当前消息（被动 msg_id + REFIDX 引用气泡）")
async def _(ctx: Ctx):
    await ctx.send(MessageSegment.reply(ctx.event.message_id) + "这条应引用你的消息")


@sec.case("引用机器人", "先发一条，再引用机器人自己的那条（出站消息仅引用、不作被动凭证）")
async def _(ctx: Ctx):
    r = await ctx.send("被引用的原消息")
    mid = r.get("message_id") if isinstance(r, dict) else None
    if not mid:
        await ctx.send(f"首条发送未返回 message_id：{r}")
        return
    await ctx.send(MessageSegment.reply(mid) + "这条应引用上一条机器人消息")


@sec.case("引用失效", "reply 一个不存在的 message_id，期望退化为普通消息仍能发出")
async def _(ctx: Ctx):
    await ctx.send(MessageSegment.reply(1) + "reply id=1 未命中缓存，本条应照常发出（无引用）")


@sec.case("CQ码", "以 CQ 码字符串（非数组）作为 message 入参，含转义与图片")
async def _(ctx: Ctx):
    at = f"[CQ:at,qq={ctx.user_id}]" if ctx.is_group else ""
    img = f"[CQ:image,file={b64(make_png(96, 96))}]"
    text = "CQ 字符串测试 &#91;转义方括号&#93; &amp; 逗号&#44;OK"
    params = {"message": at + text + img}
    if ctx.is_group:
        params.update(message_type="group", group_id=ctx.group_id)
    else:
        params.update(message_type="private", user_id=ctx.user_id)
    await ctx.run("send_msg", "CQ 字符串 send_msg", **params)


@sec.case("多段", "文本 → 图片 → 文本 → 文件：非文本段各成一条，按序发出")
async def _(ctx: Ctx):
    msg = (
        MessageSegment.text("① 第一段文本")
        + MessageSegment.image(b64(make_png(64, 64)))
        + MessageSegment.text("③ 图片之后的文本")
        + MessageSegment("file", {"file": b64("多段测试文件内容\n".encode()), "name": "multi.txt"})
        + MessageSegment.text("⑤ 末尾文本")
    )
    await ctx.send(msg)


@sec.case("send_msg", "send_msg 不带 message_type，按 group_id 有无自动分派")
async def _(ctx: Ctx):
    if ctx.is_group:
        await ctx.run("send_msg", "send_msg 自动判定=群", group_id=ctx.group_id, message="send_msg 自动分派到群")
    else:
        await ctx.run("send_msg", "send_msg 自动判定=私聊", user_id=ctx.user_id, message="send_msg 自动分派到私聊")


@sec.case("空消息", "发送空消息，期望 retcode=1400 空消息")
async def _(ctx: Ctx):
    params = {"group_id": ctx.group_id} if ctx.is_group else {"user_id": ctx.user_id}
    action = "send_group_msg" if ctx.is_group else "send_private_msg"
    r = await ctx.call(action, message=[], **params)
    ok = not r.ok and r.retcode == 1400
    await ctx.send(("✅ 符合预期\n" if ok else "❌ 不符合预期\n") + r.brief())


@sec.case("未知目标", "向从未出现过的 group_id/user_id 发送，期望 retcode=1400")
async def _(ctx: Ctx):
    r = await ctx.call("send_private_msg", user_id=1, message="不应发出")
    ok = not r.ok and r.retcode == 1400
    await ctx.send(("✅ 符合预期\n" if ok else "❌ 不符合预期\n") + r.brief())
