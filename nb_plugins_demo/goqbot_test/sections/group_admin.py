"""板块八：群管理 —— 群信息 / 机器人状态 / 成员 / 入群申请 / 禁言。

白名单接口（info / bot_state / 成员列表 / 查他人成员信息）非白名单返回 11253；
入群申请与禁言需机器人为群管理员。失败时透传 QQ 原始错误，这里只负责调用与回显。
"""

from nonebot import on_request
from nonebot.adapters.onebot.v11 import Bot, GroupRequestEvent

from ..common import Ctx, Section, call, dump

sec = Section("群管测试", "群管理", "白名单接口与群管理员接口；改动类用例仅 SUPERUSERS 可执行")

# 最近一次成员列表的 next_cursor，便于「成员列表 下一页」
_cursor: dict[int, str] = {}


@sec.case("群信息", "get_group_info（白名单）", scene="group")
async def _(ctx: Ctx):
    await ctx.run("get_group_info", group_id=ctx.group_id)


@sec.case("机器人状态", "get_group_bot_state：主动推送开关 / 收消息设置 / 角色（白名单）", scene="group")
async def _(ctx: Ctx):
    await ctx.run("get_group_bot_state", group_id=ctx.group_id)


@sec.case("成员列表", "get_group_member_list，每页 ≤30；参数「下一页」使用上次 next_cursor", scene="group")
async def _(ctx: Ctx):
    cursor = _cursor.get(ctx.group_id, "") if ctx.arg(0) == "下一页" else ""
    r = await ctx.call("get_group_member_list", group_id=ctx.group_id, cursor=cursor)
    if not r.ok:
        await ctx.send(r.brief())
        return
    data = r.data or {}
    members = data.get("members", [])
    _cursor[ctx.group_id] = data.get("next_cursor", "")
    brief = [
        {k: m.get(k) for k in ("user_id", "nickname", "role", "bot", "joined_at")}
        for m in members
    ]
    await ctx.send(f"✅ 本页 {len(members)} 人 next_cursor={data.get('next_cursor')!r}\n{dump(brief)}")


@sec.case("我的信息", "get_group_member_info 查询自己（官方成员接口）", scene="group")
async def _(ctx: Ctx):
    await ctx.run("get_group_member_info", group_id=ctx.group_id, user_id=ctx.user_id)


@sec.case("机器人信息", "get_group_member_info 查 self_id（走 bot_state，带推送设置）", scene="group")
async def _(ctx: Ctx):
    await ctx.run("get_group_member_info", group_id=ctx.group_id, user_id=int(ctx.bot.self_id))


@sec.case("成员信息", "get_group_member_info 查询 @ 到的成员", scene="group")
async def _(ctx: Ctx):
    targets = ctx.at_targets()
    if not targets:
        await ctx.send("用法：/群管测试 成员信息 @某人")
        return
    for uid in targets:
        await ctx.run("get_group_member_info", group_id=ctx.group_id, user_id=uid)


@sec.case("申请列表", "get_group_join_requests 及别名 get_group_system_msg（需群管理员）", scene="group")
async def _(ctx: Ctx):
    r = await ctx.run("get_group_join_requests", group_id=ctx.group_id, limit=10)
    alias = await ctx.call("get_group_system_msg", group_id=ctx.group_id, limit=10)
    await ctx.send(f"别名 get_group_system_msg：{alias.brief()}")
    if r.ok:
        flags = [i.get("flag") for i in (r.data or {}).get("list", [])]
        if flags:
            await ctx.send("可用 /群管测试 同意 <flag> 或 /群管测试 拒绝 <flag> [理由]\n" + "\n".join(flags))


@sec.case("同意", "set_group_add_request approve=true：/群管测试 同意 <flag>", scene="group", admin=True)
async def _(ctx: Ctx):
    if not ctx.arg(0):
        await ctx.send("用法：/群管测试 同意 <flag>")
        return
    await ctx.run("set_group_add_request", "同意入群", flag=ctx.arg(0), sub_type="add", approve=True)


@sec.case("拒绝", "set_group_add_request approve=false：/群管测试 拒绝 <flag> [理由] [拉黑]", scene="group", admin=True)
async def _(ctx: Ctx):
    if not ctx.arg(0):
        await ctx.send("用法：/群管测试 拒绝 <flag> [理由] [拉黑]")
        return
    await ctx.run(
        "set_group_add_request",
        "拒绝入群",
        flag=ctx.arg(0),
        sub_type="add",
        approve=False,
        reason=ctx.arg(1, "测试拒绝"),
        blacklist=ctx.arg(2) == "拉黑",
    )


@sec.case("审批未知", "set_group_add_request 未知 flag，期望 retcode=1404", scene="group")
async def _(ctx: Ctx):
    r = await ctx.call("set_group_add_request", flag="not-exist", approve=True)
    ok = not r.ok and r.retcode == 1404
    await ctx.send(("✅ 符合预期\n" if ok else "❌ 不符合预期\n") + r.brief())


@sec.case("禁言", "set_group_ban：/群管测试 禁言 @某人 [秒数=60]；秒数 0 为解除", scene="group", admin=True)
async def _(ctx: Ctx):
    targets = ctx.at_targets()
    nums = [a for a in ctx.args if a.isdigit()]
    if not targets:
        await ctx.send("用法：/群管测试 禁言 @某人 [秒数，默认 60；0=解除]")
        return
    duration = int(nums[-1]) if nums else 60
    for uid in targets:
        await ctx.run("set_group_ban", f"禁言 {uid} {duration}s", group_id=ctx.group_id, user_id=uid, duration=duration)


@sec.case("解禁", "set_group_ban duration=0：/群管测试 解禁 @某人", scene="group", admin=True)
async def _(ctx: Ctx):
    targets = ctx.at_targets()
    if not targets:
        await ctx.send("用法：/群管测试 解禁 @某人")
        return
    for uid in targets:
        await ctx.run("set_group_ban", f"解禁 {uid}", group_id=ctx.group_id, user_id=uid, duration=0)


@sec.case("禁言状态", "get_group_ban 及别名 get_group_mute（需群管理员）", scene="group")
async def _(ctx: Ctx):
    await ctx.run("get_group_ban", group_id=ctx.group_id)
    alias = await ctx.call("get_group_mute", group_id=ctx.group_id)
    await ctx.send(f"别名 get_group_mute：{alias.brief()}")


@sec.case("全员禁言", "set_group_whole_ban：官方不支持，期望 retcode=1404", scene="group")
async def _(ctx: Ctx):
    r = await ctx.call("set_group_whole_ban", group_id=ctx.group_id, enable=True)
    ok = not r.ok and r.retcode == 1404
    await ctx.send(("✅ 符合预期\n" if ok else "❌ 不符合预期\n") + r.brief())


@sec.case("未知群", "群类动作传入未知 group_id，期望 retcode=1404")
async def _(ctx: Ctx):
    r = await ctx.call("get_group_info", group_id=1)
    ok = not r.ok and r.retcode == 1404
    await ctx.send(("✅ 符合预期\n" if ok else "❌ 不符合预期\n") + r.brief())


# ---- 入群申请事件：request/group，flag = join_request_id ----

join_req = on_request(priority=5, block=False)


@join_req.handle()
async def _(bot: Bot, event: GroupRequestEvent) -> None:
    info = {
        "sub_type": event.sub_type,
        "group_id": event.group_id,
        "user_id": event.user_id,
        "comment": event.comment,
        "flag": event.flag,
        "user_openid": getattr(event, "user_openid", None),
        "union_openid": getattr(event, "union_openid", None),
    }
    await call(
        bot,
        "send_group_msg",
        group_id=event.group_id,
        message=f"[入群申请 request/group]\n{dump(info, 600)}\n处理：/群管测试 同意 {event.flag}",
    )
