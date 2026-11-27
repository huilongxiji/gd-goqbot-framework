"""板块一：基础信息 —— 登录信息 / 状态 / 版本 / 网关频控 / openid 反查 / 消息配额 / 用户信息 / 插件列表与调用 / 事件扩展字段 / 元事件。"""

import time

from nonebot import on_metaevent
from nonebot.adapters.onebot.v11 import MetaEvent

from ..common import Ctx, Section, dump

sec = Section("基础测试", "基础信息", "握手类动作、网关频控、id 反查、消息配额、用户信息、goqbot 插件与入站事件扩展字段")

# 元事件统计（lifecycle / heartbeat）
_meta: dict = {"lifecycle": [], "heartbeat_count": 0, "last_heartbeat": None}

meta = on_metaevent(priority=1, block=False)


@meta.handle()
async def _(event: MetaEvent) -> None:
    if event.meta_event_type == "lifecycle":
        _meta["lifecycle"].append({"time": event.time, "sub_type": getattr(event, "sub_type", "")})
        _meta["lifecycle"] = _meta["lifecycle"][-10:]
    elif event.meta_event_type == "heartbeat":
        _meta["heartbeat_count"] += 1
        _meta["last_heartbeat"] = {
            "time": event.time,
            "interval": getattr(event, "interval", None),
            "status": getattr(event, "status", None),
        }


@sec.case("登录信息", "get_login_info：self_id 与机器人昵称")
async def _(ctx: Ctx):
    await ctx.run("get_login_info")


@sec.case("状态", "get_status")
async def _(ctx: Ctx):
    await ctx.run("get_status")


@sec.case("版本", "get_version_info：程序名 / 版本 / 协议版本")
async def _(ctx: Ctx):
    await ctx.run("get_version_info")


@sec.case("网关", "get_gateway 及别名 get_gateway_bot / get_session_limit")
async def _(ctx: Ctx):
    r = await ctx.run("get_gateway")
    lines = []
    for alias in ("get_gateway_bot", "get_session_limit"):
        a = await ctx.call(alias)
        lines.append(a.brief())
    await ctx.send("别名检查：\n" + "\n".join(lines) + ("" if r.ok else "\n（主动作失败，别名结果仅供参考）"))


@sec.case("openid", "get_openid：反查自己（及当前群）的原始 openid")
async def _(ctx: Ctx):
    params = {"user_id": ctx.user_id}
    if ctx.is_group:
        params["group_id"] = ctx.group_id
    await ctx.run("get_openid", **params)


@sec.case("配额", "get_message_quota：当前会话的被动回复剩余次数、今天已发的主动消息条数")
async def _(ctx: Ctx):
    params = {"group_id": ctx.group_id} if ctx.is_group else {"user_id": ctx.user_id}
    await ctx.run("get_message_quota", **params)


@sec.case("用户信息", "get_user_info 查询自己，及别名 get_stranger_info")
async def _(ctx: Ctx):
    await ctx.run("get_user_info", user_id=ctx.user_id)
    alias = await ctx.call("get_stranger_info", user_id=ctx.user_id)
    await ctx.send(f"别名 get_stranger_info：{alias.brief()}")


@sec.case("插件列表", "get_plugin_list：goqbot 插件目录里的全部插件（含未开启的）、状态与能力")
async def _(ctx: Ctx):
    r = await ctx.call("get_plugin_list")
    if not r.ok:
        await ctx.send(r.brief())
        return
    plugins = (r.data or {}).get("plugins") or []
    brief = [
        {
            "name": p.get("name"),
            "state": p.get("state"),
            "version": p.get("version"),
            "能力": [c.get("name") for c in p.get("capabilities") or []],
        }
        for p in plugins
    ]
    await ctx.send(f"✅ 共 {len(plugins)} 个插件\n{dump(brief)}")


@sec.case("插件调用", "call_plugin：/基础测试 插件调用 [插件名] [能力名] [文字]，缺省调用示例插件 hello_ts 的 hello_ts_echo（需先在 goqbot 里开启 hello_ts）")
async def _(ctx: Ctx):
    plugin = ctx.arg(0, "hello_ts")
    capability = ctx.arg(1, "hello_ts_echo")
    text = " ".join(ctx.args[2:]) or "你好"
    await ctx.run("call_plugin", f"call_plugin {plugin} / {capability}", plugin=plugin, capability=capability, arguments={"text": text})
    if (plugin, capability) == ("hello_ts", "hello_ts_echo"):
        # 插件的能力同时注册成同名 OneBot 动作，也可以直接调用
        direct = await ctx.call("hello_ts_echo", text=text)
        await ctx.send("直接调用插件注册的动作 hello_ts_echo：" + direct.brief())


@sec.case("插件异常", "call_plugin 缺参期望 retcode=1400；不存在的插件期望 1404")
async def _(ctx: Ctx):
    a = await ctx.call("call_plugin")
    b = await ctx.call("call_plugin", plugin="goqbot_no_such_plugin", capability="x")
    ok = (not a.ok and a.retcode == 1400) and (not b.ok and b.retcode == 1404)
    await ctx.send(("✅ 符合预期\n" if ok else "❌ 不符合预期\n") + a.brief() + "\n" + b.brief())


@sec.case("事件", "回显当前消息事件的 goqbot 扩展字段（openid / 角色 / union_openid / 原始类型等）")
async def _(ctx: Ctx):
    ev = ctx.event
    keys = [
        "message_type", "sub_type", "user_id", "group_id", "message_id",
        "user_openid", "group_openid", "union_openid", "qq_msg_id",
        "qq_message_type", "msg_scene_ext", "forward_title",
    ]
    info = {k: getattr(ev, k, None) for k in keys if getattr(ev, k, None) not in (None, "")}
    sender = ev.sender.model_dump() if hasattr(ev.sender, "model_dump") else ev.sender.dict()
    info["sender"] = {k: v for k, v in sender.items() if v not in (None, "", 0)}
    extra_sender = getattr(ev.sender, "union_openid", None)
    if extra_sender:
        info["sender"]["union_openid"] = extra_sender
    info["message"] = [{"type": s.type, "data": s.data} for s in ev.original_message]
    info["raw_message"] = ev.raw_message
    info["to_me"] = ev.to_me
    if getattr(ev, "msg_elements", None):
        info["msg_elements(条数)"] = len(ev.msg_elements)
    if getattr(ev, "recent_messages", None):
        info["recent_messages(前3条)"] = ev.recent_messages[:3]
    await ctx.send(dump(info))


@sec.case("元事件", "lifecycle 记录与 heartbeat 计数（插件加载后统计）")
async def _(ctx: Ctx):
    data = dict(_meta)
    if data["last_heartbeat"]:
        data["距上次心跳(秒)"] = int(time.time()) - int(data["last_heartbeat"]["time"])
    await ctx.send(dump(data))


@sec.case("未知动作", "调用不存在的动作，期望 retcode=1404 unsupported action")
async def _(ctx: Ctx):
    r = await ctx.call("goqbot_no_such_action")
    verdict = "✅ 符合预期" if (not r.ok and r.retcode == 1404) else "❌ 不符合预期"
    await ctx.send(f"{verdict}\n{r.brief()}")


@sec.case("异步后缀", "调用 get_status_async，验证 _async 后缀被自动去除")
async def _(ctx: Ctx):
    r = await ctx.call("get_status_async")
    await ctx.send(("✅ " if r.ok else "❌ ") + r.brief())
