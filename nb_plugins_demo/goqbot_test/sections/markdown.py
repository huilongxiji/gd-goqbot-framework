"""板块四：Markdown 与按钮 —— 原生 markdown、keyboard 各按钮类型、唤起相册、引用同发、模板（已废弃）。"""

import base64
import json

from nonebot.adapters.onebot.v11 import MessageSegment

from ..common import Ctx, Section, button, cfg, md_seg, row

sec = Section("按钮测试", "Markdown 与按钮", "原生 markdown + keyboard；回调按钮的点击结果见「互动测试」板块")

SAMPLE_MD = (
    "# goqbot Markdown 测试\n"
    "**粗体** *斜体* ~~删除线~~ `行内代码`\n\n"
    "> 引用块\n\n"
    "- 列表项 1\n- 列表项 2\n\n"
    "[链接](https://github.com/huilongxiji)\n"
)


@sec.case("md", "原生 markdown（content）")
async def _(ctx: Ctx):
    await ctx.send(md_seg(SAMPLE_MD))


@sec.case("md图片", "markdown 内嵌网络图片（![#宽px #高px](url)；转存失败则整条不发出）")
async def _(ctx: Ctx):
    url = ctx.arg(0) or cfg.goqbot_test_image_url
    await ctx.send(md_seg(f"### 图片 markdown\n![img #200px #200px]({url})\n图片下方文字"))


@sec.case("按钮", "markdown + 全类型按钮：回调 / 指令(enter) / 指令(填入) / 跳转 / 二次确认 / 管理员可点")
async def _(ctx: Ctx):
    rows = [
        row(
            button("回调按钮", "cb:hello", type_=1, visited="已回调"),
            button("回调·引用", "cb:reply", type_=1, reply=True),
        ),
        row(
            button("指令·直接发送", "/基础测试 版本", type_=2, enter=True),
            button("指令·填入输入框", "/测试帮助", type_=2),
        ),
        row(
            button("跳转链接", "https://github.com/huilongxiji/gd-goqbot-docs", type_=0, style=0),
            button(
                "二次确认",
                "cb:modal",
                type_=1,
                style=3,
                modal={"content": "确定要触发回调吗？", "confirm_text": "确定", "cancel_text": "算了"},
            ),
        ),
        row(button("仅管理员", "cb:admin", type_=1, permission=1, style=4)),
    ]
    await ctx.send(md_seg("### 按钮全类型测试\n点击下方按钮，回调类按钮会触发 interaction 通知", rows))


@sec.case("按钮组", "同一 group_id 的回调按钮：点其中一个后其余变灰")
async def _(ctx: Ctx):
    rows = [
        row(
            button("选项A", "cb:vote:A", group_id="vote", bid="a"),
            button("选项B", "cb:vote:B", group_id="vote", bid="b"),
            button("选项C", "cb:vote:C", group_id="vote", bid="c"),
        )
    ]
    await ctx.send(md_seg("### 单选测试\n只能点一个", rows))


@sec.case("相册", "指令按钮 anchor=1：点击唤起手机端相册选图器", scene="")
async def _(ctx: Ctx):
    rows = [row(button("📷 选择图片", " ", type_=2, anchor=1))]
    await ctx.send(md_seg("### 唤起相册\n点击按钮将打开相册（仅手机端），选图后发送给机器人", rows))


@sec.case("md引用", "reply + markdown 同发：引用当前消息的 markdown")
async def _(ctx: Ctx):
    await ctx.send(MessageSegment.reply(ctx.event.message_id) + md_seg("**这条 markdown 引用了你的消息**"))


@sec.case("md混发", "文本 + markdown + 文本：markdown 单独成条，按序发出")
async def _(ctx: Ctx):
    await ctx.send("markdown 之前的文本" + md_seg("**中间的 markdown**") + "markdown 之后的文本")


@sec.case("md_b64", "markdown 段 data 以 base64:// 字符串传入")
async def _(ctx: Ctx):
    inner = {
        "markdown": {"content": "**base64 形式的 markdown 段**"},
        "keyboard": {"content": {"rows": [row(button("回调", "cb:b64"))]}},
    }
    raw = base64.b64encode(json.dumps(inner, ensure_ascii=False).encode()).decode()
    await ctx.send(MessageSegment("markdown", {"data": "base64://" + raw}))


@sec.case("md顶层rows", "keyboard 以顶层 rows 字段传入（兼容写法）")
async def _(ctx: Ctx):
    inner = {"markdown": {"content": "**顶层 rows 写法**"}, "rows": [row(button("回调", "cb:rows"))]}
    await ctx.send(MessageSegment("markdown", {"data": inner}))


@sec.case("模板", "模板 markdown（官方已废弃，仍可发）：/按钮测试 模板 <custom_template_id> [key=value…]")
async def _(ctx: Ctx):
    tid = ctx.arg(0)
    if not tid:
        await ctx.send("用法：/按钮测试 模板 <custom_template_id> [key=value …]\n（goqbot 会打一次废弃告警）")
        return
    params = []
    for kv in ctx.args[1:]:
        if "=" in kv:
            k, v = kv.split("=", 1)
            params.append({"key": k, "values": [v]})
    inner = {"markdown": {"custom_template_id": tid, "params": params}}
    await ctx.send(MessageSegment("markdown", {"data": inner}))


@sec.case("click_limit", "按钮带已废弃的 click_limit 字段：应被忽略并告警一次，消息正常发出")
async def _(ctx: Ctx):
    btn = button("限点按钮", "cb:limit")
    btn["action"]["click_limit"] = 1
    await ctx.send(md_seg("**click_limit 废弃字段测试**", [row(btn)]))
