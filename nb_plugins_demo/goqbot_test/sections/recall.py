"""板块六：撤回与取回 —— delete_msg / get_msg / get_forward_msg。"""

import asyncio

from ..common import Ctx, Section, cfg, dump

sec = Section("撤回测试", "撤回与取回", "消息撤回、消息快照取回（内存缓存）与合并转发取回")

# 观察器记录的最近一次 forward id，供「转发」用例在未带参数时使用
last_forward: dict = {"id": ""}


@sec.case("撤回", "发送一条消息，N 秒后 delete_msg 撤回（N 取参数或 GOQBOT_TEST_RECALL_DELAY）")
async def _(ctx: Ctx):
    try:
        delay = int(ctx.arg(0, str(cfg.goqbot_test_recall_delay)))
    except ValueError:
        delay = cfg.goqbot_test_recall_delay
    r = await ctx.send(f"这条消息将在 {delay} 秒后被撤回")
    mid = r.get("message_id") if isinstance(r, dict) else None
    if not mid:
        await ctx.send(f"发送未返回 message_id：{r}")
        return
    await asyncio.sleep(delay)
    await ctx.run("delete_msg", f"撤回 message_id={mid}", message_id=mid)


@sec.case("撤回未知", "撤回不存在的 message_id，期望 retcode=1400")
async def _(ctx: Ctx):
    r = await ctx.call("delete_msg", message_id=1)
    ok = not r.ok and r.retcode == 1400
    await ctx.send(("✅ 符合预期\n" if ok else "❌ 不符合预期\n") + r.brief())


@sec.case("取回入站", "get_msg 取回当前这条（入站）消息快照")
async def _(ctx: Ctx):
    await ctx.run("get_msg", f"get_msg 入站 {ctx.event.message_id}", message_id=ctx.event.message_id)


@sec.case("取回出站", "先发一条，再 get_msg 取回机器人发出的快照")
async def _(ctx: Ctx):
    r = await ctx.send("用于 get_msg 的出站消息")
    mid = r.get("message_id") if isinstance(r, dict) else None
    if mid:
        await ctx.run("get_msg", f"get_msg 出站 {mid}", message_id=mid)


@sec.case("取回指定", "/撤回测试 取回指定 <message_id>")
async def _(ctx: Ctx):
    if not ctx.arg(0).isdigit():
        await ctx.send("用法：/撤回测试 取回指定 <message_id>")
        return
    await ctx.run("get_msg", message_id=int(ctx.arg(0)))


@sec.case("取回未知", "get_msg 不存在的 id，期望 retcode=1404；缺参期望 1400")
async def _(ctx: Ctx):
    a = await ctx.call("get_msg", message_id=1)
    b = await ctx.call("get_msg")
    ok = (not a.ok and a.retcode == 1404) and (not b.ok and b.retcode == 1400)
    await ctx.send(("✅ 符合预期\n" if ok else "❌ 不符合预期\n") + a.brief() + "\n" + b.brief())


@sec.case("转发", "get_forward_msg：参数为 forward id，缺省用观察器最近一次捕获的 id")
async def _(ctx: Ctx):
    fid = ctx.arg(0) or last_forward["id"]
    if not fid:
        await ctx.send(
            "尚未捕获 forward。获取方式：\n"
            "① 群设为「最近十条」后 @机器人 发任意消息（103）；\n"
            "② 向机器人转发一份聊天记录（102）。\n"
            "观察器会自动回显并记录 id。"
        )
        return
    r = await ctx.call("get_forward_msg", id=fid)
    if not r.ok:
        await ctx.send(r.brief())
        return
    nodes = (r.data or {}).get("messages", [])
    await ctx.send(f"✅ get_forward_msg 节点数={len(nodes)}\n" + dump(nodes[:5]))
    alias = await ctx.call("get_forward_msg", message_id=fid)
    await ctx.send("message_id 参数别名：" + alias.brief())


@sec.case("转发未知", "get_forward_msg 未知 id，期望 1404；缺参期望 1400")
async def _(ctx: Ctx):
    a = await ctx.call("get_forward_msg", id="not-exist")
    b = await ctx.call("get_forward_msg")
    ok = (not a.ok and a.retcode == 1404) and (not b.ok and b.retcode == 1400)
    await ctx.send(("✅ 符合预期\n" if ok else "❌ 不符合预期\n") + a.brief() + "\n" + b.brief())


@sec.case("快照字段", "检查 get_msg 出站快照字段是否齐全（time/message_type/message_id/real_id/sender/message/raw_message）")
async def _(ctx: Ctx):
    r = await ctx.send("快照字段检查")
    mid = r.get("message_id") if isinstance(r, dict) else None
    g = await ctx.call("get_msg", message_id=mid)
    need = ["time", "message_type", "message_id", "real_id", "sender", "message", "raw_message", "qq_msg_id"]
    need += ["group_id", "group_openid"] if ctx.is_group else ["target_id", "user_openid"]
    missing = [k for k in need if not g.ok or k not in (g.data or {})]
    await ctx.send(("✅ 字段齐全" if not missing else f"❌ 缺少：{missing}") + "\n" + dump(g.data, 600))
