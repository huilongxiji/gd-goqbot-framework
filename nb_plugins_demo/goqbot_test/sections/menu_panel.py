"""板块九：自定义菜单与指令面板。

set_menu 为全局整体覆盖（约 5 QPM），执行前自动备份当前菜单，可用「菜单还原」恢复。
面板创建默认 target_type=specific，只关联当前用户 / 当前群，避免影响其他人。
"""

from ..common import Ctx, Section, dump

sec = Section("菜单测试", "菜单与指令面板", "get/set_menu 与指令面板增删改查；改动类用例仅 SUPERUSERS 可执行")

_state: dict = {"menu_backup": None, "last_panel": ""}

SAMPLE_MENU = {
    "items": [
        {"name": "测试帮助", "type": "send_message", "send_message": "/测试帮助"},
        {
            "name": "板块",
            "type": "menu",
            "sub_menu_items": [
                {"name": "基础", "type": "send_message", "send_message": "/基础测试"},
                {"name": "媒体", "type": "send_message", "send_message": "/媒体测试"},
                {"name": "流式", "type": "send_message", "send_message": "/发送测试 流式"},
                {"name": "文档", "type": "link", "link": "https://huilongxiji.github.io/gd-goqbot-docs/"},
            ],
        },
        {"name": "开关", "type": "switch", "switch": {"switch_id": "goqbot_test", "default": False}},
    ]
}

SAMPLE_PANEL_ITEMS = [
    {"name": "测试帮助", "desc": "列出全部测试板块", "type": "command"},
    {"name": "全部自测", "desc": "一键运行只读自测", "type": "command"},
    {"name": "文档", "desc": "goqbot 使用文档", "type": "link", "link": "https://huilongxiji.github.io/gd-goqbot-docs/"},
]


async def _openids(ctx: Ctx) -> tuple[str, str]:
    r = await ctx.call("get_openid", user_id=ctx.user_id, **({"group_id": ctx.group_id} if ctx.is_group else {}))
    d = r.data or {}
    return d.get("user_openid", ""), d.get("group_openid", "")


def _scope(ctx: Ctx, i: int = 0) -> str:
    s = ctx.arg(i)
    if s in ("c2c", "group", "channel", "dm"):
        return s
    return "group" if ctx.is_group else "c2c"


@sec.case("菜单查询", "get_menu：查询全局自定义菜单（仅单聊底部展示）")
async def _(ctx: Ctx):
    await ctx.run("get_menu")


@sec.case("菜单设置", "set_menu：覆盖为测试菜单（先自动备份）。须带参数「确认」", admin=True)
async def _(ctx: Ctx):
    if ctx.arg(0) != "确认":
        await ctx.send("该操作会整体覆盖线上全局菜单。确认执行：/菜单测试 菜单设置 确认\n将设置为：\n" + dump(SAMPLE_MENU))
        return
    old = await ctx.call("get_menu")
    if old.ok:
        _state["menu_backup"] = (old.data or {}).get("menu")
    await ctx.run("set_menu", "set_menu 测试菜单", menu=SAMPLE_MENU)


@sec.case("菜单还原", "set_menu：恢复「菜单设置」前备份的菜单", admin=True)
async def _(ctx: Ctx):
    backup = _state["menu_backup"]
    if not backup or not backup.get("items"):
        await ctx.send("没有可还原的备份（未执行过菜单设置，或原菜单为空——官方不允许设置空菜单）")
        return
    await ctx.run("set_menu", "还原菜单", menu={"items": backup["items"]})


@sec.case("菜单兼容", "set_menu 直接传 items（不包 menu）的兼容写法；缺 items 期望 1400", admin=True)
async def _(ctx: Ctx):
    r = await ctx.call("set_menu")
    ok = not r.ok and r.retcode == 1400
    await ctx.send(("✅ 缺参符合预期\n" if ok else "❌ 缺参不符合预期\n") + r.brief())
    if ctx.arg(0) == "确认":
        await ctx.run("set_menu", "set_menu(items 顶层)", items=SAMPLE_MENU["items"])
    else:
        await ctx.send("如需实际验证顶层 items 写法：/菜单测试 菜单兼容 确认")


@sec.case("面板列表", "get_command_panels：/菜单测试 面板列表 [c2c|group]，缺省按当前场景")
async def _(ctx: Ctx):
    await ctx.run("get_command_panels", scope=_scope(ctx), limit=20)


@sec.case("面板详情", "get_command_panel：/菜单测试 面板详情 [panel_id]，缺省用最近创建的")
async def _(ctx: Ctx):
    pid = ctx.arg(0) or _state["last_panel"]
    if not pid:
        await ctx.send("用法：/菜单测试 面板详情 <panel_id>")
        return
    await ctx.run("get_command_panel", panel_id=pid)


@sec.case("面板创建", "create_command_panel：specific 面板，仅关联当前用户/群", admin=True)
async def _(ctx: Ctx):
    user_oid, group_oid = await _openids(ctx)
    scope = "group" if ctx.is_group else "c2c"
    req = {
        "scope": scope,
        "target_type": "specific",
        "panel": {"items": SAMPLE_PANEL_ITEMS, "remark": "goqbot_test 自动创建"},
    }
    if scope == "group":
        req["group_openids"] = [group_oid]
    else:
        req["user_openids"] = [user_oid]
    r = await ctx.run("create_command_panel", "创建指令面板", **req)
    if r.ok:
        _state["last_panel"] = (r.data or {}).get("panel_id", "")


@sec.case("面板修改", "set_command_panel：整体覆盖面板项 /菜单测试 面板修改 [panel_id]", admin=True)
async def _(ctx: Ctx):
    pid = ctx.arg(0) or _state["last_panel"]
    if not pid:
        await ctx.send("用法：/菜单测试 面板修改 <panel_id>")
        return
    items = SAMPLE_PANEL_ITEMS + [{"name": "流式测试", "desc": "单聊流式消息", "type": "command"}]
    await ctx.run("set_command_panel", "修改指令面板", panel_id=pid, panel={"items": items, "remark": "goqbot_test 已修改"})


@sec.case("面板关联", "set_command_panel_target：/菜单测试 面板关联 <add|del> [panel_id]，对象为当前用户/群", admin=True)
async def _(ctx: Ctx):
    op = ctx.arg(0)
    if op not in ("add", "del"):
        await ctx.send("用法：/菜单测试 面板关联 <add|del> [panel_id]")
        return
    pid = ctx.arg(1) or _state["last_panel"]
    if not pid:
        await ctx.send("缺少 panel_id")
        return
    user_oid, group_oid = await _openids(ctx)
    target = {"group_openids": [group_oid]} if ctx.is_group else {"user_openids": [user_oid]}
    await ctx.run("set_command_panel_target", f"面板关联 {op}", panel_id=pid, op=op, **target)


@sec.case("面板删除", "delete_command_panel：/菜单测试 面板删除 [panel_id]", admin=True)
async def _(ctx: Ctx):
    pid = ctx.arg(0) or _state["last_panel"]
    if not pid:
        await ctx.send("用法：/菜单测试 面板删除 <panel_id>")
        return
    r = await ctx.run("delete_command_panel", f"删除面板 {pid}", panel_id=pid)
    if r.ok and pid == _state["last_panel"]:
        _state["last_panel"] = ""


@sec.case("面板异常", "缺 scope / panel_id / op 等入参，期望均为 retcode=1400")
async def _(ctx: Ctx):
    checks = [
        await ctx.call("get_command_panels"),
        await ctx.call("get_command_panel"),
        await ctx.call("create_command_panel", scope="c2c"),
        await ctx.call("set_command_panel", panel_id="x"),
        await ctx.call("delete_command_panel"),
        await ctx.call("set_command_panel_target", panel_id="x"),
    ]
    lines = [("✅ " if (not r.ok and r.retcode == 1400) else "❌ ") + r.brief() for r in checks]
    await ctx.send("\n".join(lines))
