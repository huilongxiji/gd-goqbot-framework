"""板块五：互动回调 —— interaction 通知、put_interaction 回执、event_id 被动回复、派生 message 事件。"""

from nonebot import on_notice, on_startswith
from nonebot.adapters.onebot.v11 import Bot, MessageEvent, NoticeEvent

from ..common import Ctx, Section, button, call, dump, md_seg, row

sec = Section("互动测试", "互动回调", "点击回调按钮 → notice/interaction + 派生 message；插件自动回执并被动回复")

_state = {"ack_code": 0, "history": []}

ACK_CODES = {0: "成功", 1: "操作失败", 2: "操作频繁", 3: "重复操作", 4: "没有权限", 5: "仅管理员操作"}


def _is_interaction(event: NoticeEvent) -> bool:
    return event.notice_type == "interaction"


interaction = on_notice(rule=_is_interaction, priority=5, block=False)


@interaction.handle()
async def _(bot: Bot, event: NoticeEvent) -> None:
    fields = [
        "sub_type", "user_id", "user_openid", "group_id", "group_openid", "chat_type",
        "interaction_id", "data_type", "button_id", "button_data",
    ]
    info = {k: getattr(event, k, None) for k in fields if getattr(event, k, None) not in (None, "")}
    _state["history"] = (_state["history"] + [info])[-10:]

    code = _state["ack_code"]
    ack = await call(bot, "put_interaction", interaction_id=getattr(event, "interaction_id", ""), code=code)

    # 回执后用被动 event_id 回一条（框架已把外层 event_id 存入 lazy 池）
    params = {"message": f"[interaction 通知]\n回执 code={code}({ACK_CODES.get(code, '?')}) → {ack.brief()}\n{dump(info, 800)}"}
    gid = getattr(event, "group_id", None)
    if gid:
        params.update(message_type="group", group_id=gid)
    else:
        params.update(message_type="private", user_id=getattr(event, "user_id", 0))
    await call(bot, "send_msg", **params)


derived = on_startswith("cb:", priority=6, block=True)


@derived.handle()
async def _(bot: Bot, event: MessageEvent) -> None:
    iid = getattr(event, "interaction_id", None)
    src = "按钮派生" if iid else "用户手动输入"
    await bot.send(event, f"[派生 message 事件] 来源={src} text={event.get_plaintext()!r} interaction_id={iid}")


@sec.case("面板", "发送一组回调按钮用于点击测试")
async def _(ctx: Ctx):
    rows = [
        row(button("回调 A", "cb:A", bid="A"), button("回调 B", "cb:B", bid="B")),
        row(button("回调·引用本消息", "cb:ref", reply=True)),
    ]
    await ctx.send(md_seg(f"### 互动回调测试\n当前回执码：{_state['ack_code']}（{ACK_CODES[_state['ack_code']]}）", rows))


@sec.case("回执码", "设置后续回执 code：0成功 1失败 2频繁 3重复 4无权限 5仅管理员")
async def _(ctx: Ctx):
    try:
        code = int(ctx.arg(0))
    except ValueError:
        await ctx.send("用法：/互动测试 回执码 <0-5>\n" + dump(ACK_CODES))
        return
    _state["ack_code"] = code
    await ctx.send(f"后续回执 code={code}（{ACK_CODES.get(code, '未定义')}）")


@sec.case("记录", "最近 10 次 interaction 通知")
async def _(ctx: Ctx):
    await ctx.send(dump(_state["history"]) if _state["history"] else "尚未收到 interaction 通知")


@sec.case("回执异常", "put_interaction 缺少 interaction_id，期望 retcode=1400；别名 send_interaction_ack 同测")
async def _(ctx: Ctx):
    a = await ctx.call("put_interaction")
    b = await ctx.call("send_interaction_ack")
    ok = (not a.ok and a.retcode == 1400) and (not b.ok and b.retcode == 1400)
    await ctx.send(("✅ 符合预期\n" if ok else "❌ 不符合预期\n") + a.brief() + "\n" + b.brief())
