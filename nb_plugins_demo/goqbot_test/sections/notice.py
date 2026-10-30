"""板块十：通知事件与观察器。

- 记录全部 notice（入退群、好友增删、群/C2C 主动推送开关等），可查询最近记录；
- get_c2c_msg_state 查询 C2C 主动推送开关（本地事件缓存）；
- 入站观察器：自动回显富媒体段、合并转发（forward）与 recent_messages。
"""

from nonebot import on_message, on_notice
from nonebot.adapters.onebot.v11 import Bot, MessageEvent, NoticeEvent
from nonebot.log import logger

from ..common import Ctx, Section, call, cfg, dump
from .recall import last_forward

sec = Section("通知测试", "通知与观察器", "notice 事件记录、C2C 推送开关查询、入站富媒体 / 转发观察")

NOTICE_NAMES = {
    "group_increase": "入群（invite=机器人被拉入 / approve=成员加入）",
    "group_decrease": "退群（kick_me=机器人被移出 / leave=成员退出）",
    "group_msg_reject": "群关闭主动推送",
    "group_msg_receive": "群开启主动推送",
    "friend_add": "用户添加机器人好友",
    "friend_decrease": "用户删除机器人好友",
    "c2c_msg_reject": "用户关闭单聊主动消息",
    "c2c_msg_receive": "用户开启单聊主动消息",
    "interaction": "按钮互动回调",
}

_notices: list[dict] = []
_observe = {"on": True}

notice = on_notice(priority=1, block=False)


@notice.handle()
async def _(bot: Bot, event: NoticeEvent) -> None:
    fields = ["notice_type", "sub_type", "time", "group_id", "user_id", "operator_id", "group_openid", "user_openid"]
    info = {k: getattr(event, k, None) for k in fields if getattr(event, k, None) not in (None, "")}
    info["说明"] = NOTICE_NAMES.get(event.notice_type, "未登记的通知类型")
    _notices.append(info)
    del _notices[:-30]
    logger.info(f"[goqbot_test] notice {dump(info, 400)}")

    if not cfg.goqbot_test_notice_reply:
        return
    nt, st = event.notice_type, getattr(event, "sub_type", "")
    if nt == "friend_add":
        await call(bot, "send_private_msg", user_id=event.user_id, message="[friend_add] 感谢添加，发送 /测试帮助 查看测试板块")
    elif nt == "group_increase" and st == "invite":
        await call(bot, "send_group_msg", group_id=event.group_id, message="[group_increase/invite] 机器人已入群，@我 发送 /测试帮助")
    elif nt == "group_increase" and st == "approve":
        await call(bot, "send_group_msg", group_id=event.group_id, message=f"[group_increase/approve] 欢迎新成员 user_id={event.user_id}")
    elif nt == "c2c_msg_receive":
        await call(bot, "send_private_msg", user_id=event.user_id, message="[c2c_msg_receive] 已开启主动消息")


@sec.case("最近", "最近 30 条 notice 记录（可加参数按 notice_type 过滤）")
async def _(ctx: Ctx):
    items = [n for n in _notices if not ctx.arg(0) or n.get("notice_type") == ctx.arg(0)]
    await ctx.send(dump(items[-10:]) if items else "尚未收到相关 notice")


@sec.case("类型", "列出 goqbot 上报的全部 notice 类型及触发方式")
async def _(ctx: Ctx):
    await ctx.send(dump(NOTICE_NAMES))


@sec.case("推送状态", "get_c2c_msg_state：查询自己（或参数 user_id）的 C2C 主动推送开关")
async def _(ctx: Ctx):
    uid = int(ctx.arg(0)) if ctx.arg(0).isdigit() else ctx.user_id
    r = await ctx.run("get_c2c_msg_state", user_id=uid)
    alias = await ctx.call("get_friend_msg_state", user_id=uid)
    hint = ""
    if r.ok and not (r.data or {}).get("known"):
        hint = "\nknown=false：尚未收到该用户的开关事件，可在机器人资料页切换一次「接收主动消息」后再查"
    await ctx.send(f"别名 get_friend_msg_state：{alias.brief()}{hint}")


@sec.case("推送状态openid", "get_c2c_msg_state 以 user_openid 入参")
async def _(ctx: Ctx):
    o = await ctx.call("get_openid", user_id=ctx.user_id)
    oid = (o.data or {}).get("user_openid", "")
    if not oid:
        await ctx.send(o.brief())
        return
    await ctx.run("get_c2c_msg_state", user_openid=oid)


@sec.case("观察", "入站观察器开关：/通知测试 观察 开|关（回显富媒体段 / forward / recent_messages）")
async def _(ctx: Ctx):
    if ctx.arg(0) in ("开", "关"):
        _observe["on"] = ctx.arg(0) == "开"
    await ctx.send(f"入站观察器：{'开' if _observe['on'] else '关'}")


# ---- 入站观察器：不拦截，仅在含富媒体 / forward / recent_messages 时回显 ----

MEDIA_TYPES = {"image", "video", "record", "file", "forward"}


def _interesting(event: MessageEvent) -> bool:
    # 群全量消息模式下只观察 @机器人 的消息，避免刷屏
    if not _observe["on"] or not event.to_me:
        return False
    if any(s.type in MEDIA_TYPES for s in event.original_message):
        return True
    return bool(getattr(event, "recent_messages", None))


observer = on_message(rule=_interesting, priority=99, block=False)


@observer.handle()
async def _(bot: Bot, event: MessageEvent) -> None:
    out = []
    for seg in event.original_message:
        if seg.type == "forward":
            fid = str(seg.data.get("id", ""))
            last_forward["id"] = fid
            content = seg.data.get("content") or []
            out.append({"type": "forward", "id": fid, "节点数": len(content), "标题": getattr(event, "forward_title", None)})
            r = await call(bot, "get_forward_msg", id=fid)
            out.append({"get_forward_msg": r.brief(), "前2节点": (r.data or {}).get("messages", [])[:2] if r.ok else None})
        elif seg.type in MEDIA_TYPES:
            out.append({"type": seg.type, "data": seg.data})
    rm = getattr(event, "recent_messages", None)
    if rm:
        out.append({"recent_messages": f"{len(rm)} 条", "前3条": rm[:3]})
    out.append({"qq_message_type": getattr(event, "qq_message_type", None)})
    await bot.send(event, "[入站观察]\n" + dump(out, 1200))
