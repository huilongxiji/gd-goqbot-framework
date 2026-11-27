"""板块二：消息段 —— text / at（markdown 艾特、私聊 at）/ reply / CQ 字符串（含宽松写法、文字段里的 CQ 码）/ 多段顺序 / 图文卡片 / 空消息。

QQ 官方规定：群里艾特某个人只在 markdown 消息里生效，普通文字消息里的艾特会原样显示成标签；私聊不支持艾特。
"""

from nonebot.adapters.onebot.v11 import MessageSegment

from ..common import Ctx, Section, b64, cfg, make_png, md_seg

sec = Section("消息测试", "消息段", "文本、@（群里只有 markdown 能真正艾特）、引用回复、CQ 码字符串入参、多段拆分顺序与异常入参")


@sec.case("文本", "纯文本被动回复（自动套 msg_id）")
async def _(ctx: Ctx):
    r = await ctx.send("纯文本测试：Hello goqbot 👋\n第二行 & 特殊字符 [] ,")
    await ctx.send(f"返回：{r}")


@sec.case("at", "markdown 里艾特发送者（写 OneBot 数字 ID，goqbot 自动换成 openid）：应显示成真正的艾特", scene="group")
async def _(ctx: Ctx):
    await ctx.send(md_seg(f'<qqbot-at-user id="{ctx.user_id}" /> 这条 markdown 里的艾特应该生效'))


@sec.case("at文字", "普通文字消息里的 at 段：群文字消息不支持艾特（官方限制），标签会原样显示；at 机器人自己 / at 全体被去掉并在 goqbot 日志里说明", scene="group")
async def _(ctx: Ctx):
    msg = (
        MessageSegment.at(ctx.user_id)
        + " ← 群文字消息里的艾特会原样显示成标签（官方限制）；后面的 at 机器人自己与 at 全体应被去掉："
        + MessageSegment.at(int(ctx.bot.self_id))
        + MessageSegment.at("all")
    )
    await ctx.send(msg)


@sec.case("at他人", "markdown 里艾特参数里 @ 到的成员（对方需在本群发过消息，才能反查 openid）", scene="group")
async def _(ctx: Ctx):
    targets = ctx.at_targets()
    if not targets:
        await ctx.send("用法：/消息测试 at他人 @某人 [@某人…]")
        return
    await ctx.send(md_seg("点名：" + " ".join(f'<qqbot-at-user id="{uid}" />' for uid in targets)))


@sec.case("md艾特写法", "markdown 里三种数字 ID 写法 [CQ:at] / <qqbot-at-user id=\"数字\" /> / <@数字> 都应变成艾特；不存在的 ID 被去掉", scene="group")
async def _(ctx: Ctx):
    uid = ctx.user_id
    await ctx.send(
        md_seg(
            f"① CQ 码：[CQ:at,qq={uid}]\n\n"
            f'② 标签：<qqbot-at-user id="{uid}" />\n\n'
            f"③ 旧写法：<@{uid}>\n\n"
            "④ 不存在的 ID：[CQ:at,qq=12345]（应被去掉，goqbot 日志里写明原因）"
        )
    )


@sec.case("私聊at", "私聊不支持艾特：带 name 的 at 段改成文字「@名字」，不带 name 的去掉，markdown 里的艾特也去掉，都不会出现标签", scene="private")
async def _(ctx: Ctx):
    name = getattr(ctx.event.sender, "nickname", "") or "测试用户"
    await ctx.send(MessageSegment("at", {"qq": str(ctx.user_id), "name": name}) + " ← 带 name：应显示成文字「@名字」")
    await ctx.send(MessageSegment.at(ctx.user_id) + "← 不带 name：前面的艾特应被去掉")
    await ctx.send(md_seg(f'<qqbot-at-user id="{ctx.user_id}" />← markdown 里的艾特也应被去掉'))


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


@sec.case("CQ宽松", "CQ 码不规范写法：类型大写、参数名前有空格、末尾多逗号；图片应正常发出，看不到 [CQ: 文字")
async def _(ctx: Ctx):
    img = b64(make_png(96, 96))
    params = {"message": f"CQ 宽松写法测试：下面应是一张图片[CQ:IMAGE, file={img},]"}
    if ctx.is_group:
        params.update(message_type="group", group_id=ctx.group_id)
    else:
        params.update(message_type="private", user_id=ctx.user_id)
    await ctx.run("send_msg", "CQ 宽松写法 send_msg", **params)


@sec.case("CQ文字段", "文字段里写的 CQ 码：config.yml 的 onebot.parse_cq_in_text 关闭（默认）时原样显示成文字，开启后转换成图片")
async def _(ctx: Ctx):
    img = b64(make_png(32, 32))
    await ctx.send(MessageSegment.text(f"文字段里的 CQ 码 → [CQ:image,file={img}] ← 关闭时显示这段文字，开启时显示成图片"))


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


CARD_URL = "https://github.com/huilongxiji/gd-goqbot-docs"


@sec.case("卡片", "图文卡片 msg_type=8：OneBot v11 share 段（url/title/content/image）")
async def _(ctx: Ctx):
    await ctx.send(
        MessageSegment.share(
            url=CARD_URL,
            title="goqbot 图文卡片",
            content="由 share 段转换为官方 tuwen 卡片，点击跳转文档",
            image=cfg.goqbot_test_image_url,
        )
    )


@sec.case("卡片别名", "card 段（官方字段名 title/description/url/pic_url），引用当前消息同发")
async def _(ctx: Ctx):
    card = MessageSegment(
        "card",
        {
            "title": "card 段测试",
            "description": "使用官方同名字段构造的图文卡片",
            "url": CARD_URL,
            "pic_url": cfg.goqbot_test_image_url,
        },
    )
    await ctx.send(MessageSegment.reply(ctx.event.message_id) + MessageSegment.text("卡片前的文本") + card)


@sec.case("卡片失效", "http 链接的卡片，期望被丢弃并返回 1400 空消息")
async def _(ctx: Ctx):
    params = {"group_id": ctx.group_id} if ctx.is_group else {"user_id": ctx.user_id}
    action = "send_group_msg" if ctx.is_group else "send_private_msg"
    seg = {"type": "share", "data": {"url": "http://example.com", "title": "t", "image": cfg.goqbot_test_image_url}}
    r = await ctx.call(action, message=[seg], **params)
    ok = not r.ok and r.retcode == 1400
    await ctx.send(("✅ 符合预期\n" if ok else "❌ 不符合预期\n") + r.brief())


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
