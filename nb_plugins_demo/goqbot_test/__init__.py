"""goqbot 全功能测试插件（NoneBot2 + OneBot V11 适配器）。

发送 /测试帮助 查看所有板块；/<板块命令> 查看该板块用例；/全部自测 一键跑只读检查。
"""

from nonebot import on_command
from nonebot.matcher import Matcher
from nonebot.plugin import PluginMetadata

from .common import SECTIONS
from .config import Config
from . import sections  # noqa: F401  注册各板块命令

__plugin_meta__ = PluginMetadata(
    name="goqbot 全功能测试",
    description="按板块覆盖 goqbot 全部 OneBot 动作、消息段、事件的测试插件",
    usage="/测试帮助",
    type="application",
    config=Config,
    supported_adapters={"~onebot.v11"},
)

MANUAL_ITEMS = [
    "以下能力无法由插件主动触发，需手动验证：",
    "· 正向 / 反向 WS 连接、access_token 鉴权、断线自动重连",
    "· 反向连接失败缓存重发；全部下游掉线时 QQ 兜底文本（停掉 NoneBot 后给机器人发消息）",
    "· array / string 两种上报格式（切换 config.yml 后用 /基础测试 事件 对比）",
    "· mention_mode: at（群全量模式下 @机器人 时 to_me 为真）",
    "· 文件日志 logs/goqbot-YYYY-MM-DD.log 与终端分级上色",
]

help_cmd = on_command("测试帮助", aliases={"goqbot测试"}, priority=5, block=True)


@help_cmd.handle()
async def _(matcher: Matcher) -> None:
    lines = ["goqbot 全功能测试 · 板块列表", ""]
    for s in SECTIONS:
        lines.append(f"/{s.command}  {s.title}（{len(s.cases)} 项）")
    lines += ["/全部自测  一键只读自测", "", "发送板块命令（不带参数）查看用例；群内需 @机器人。", ""]
    lines += MANUAL_ITEMS
    await matcher.finish("\n".join(lines))
