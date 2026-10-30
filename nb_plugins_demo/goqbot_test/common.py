"""公共基础：板块注册、动作调用封装、结果格式化、测试素材生成。"""

import base64
import json
import os
import re
import struct
import tempfile
import zlib
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Awaitable, Callable

from nonebot import get_plugin_config, on_command
from nonebot.adapters.onebot.v11 import (
    Bot,
    GroupMessageEvent,
    Message,
    MessageEvent,
    MessageSegment,
    PrivateMessageEvent,
)
from nonebot.adapters.onebot.v11.exception import ActionFailed
from nonebot.exception import MatcherException
from nonebot.log import logger
from nonebot.matcher import Matcher
from nonebot.params import CommandArg
from nonebot.permission import SUPERUSER

from .config import Config

cfg = get_plugin_config(Config)

# 单条回显的最大字符数，避免超长 JSON 刷屏 / 超出 QQ 单条长度
MAX_DUMP = 1500


# ---------------------------------------------------------------- 动作调用


@dataclass
class Result:
    """一次 OneBot 动作调用的结果（成功与失败统一表示）。"""

    action: str
    ok: bool
    retcode: int = 0
    data: Any = None
    message: str = ""

    def brief(self) -> str:
        if self.ok:
            return f"✅ {self.action} ok"
        return f"❌ {self.action} retcode={self.retcode} {self.message}"


async def call(bot: Bot, action: str, **params: Any) -> Result:
    """调用动作并吞掉 ActionFailed，统一返回 Result。"""
    try:
        data = await bot.call_api(action, **params)
        return Result(action, True, 0, data)
    except ActionFailed as e:
        info = getattr(e, "info", {}) or {}
        return Result(
            action,
            False,
            int(info.get("retcode", -1) or -1),
            info.get("data"),
            str(info.get("message") or info.get("wording") or ""),
        )
    except Exception as e:  # 超时 / 网络异常
        logger.opt(exception=e).warning(f"[goqbot_test] {action} 调用异常")
        return Result(action, False, -1, None, f"{type(e).__name__}: {e}")


_URL_RE = re.compile(r"(https?)://([^/\s\"']+)")


def defang(s: str) -> str:
    """QQ 官方机器人纯文本里出现未备案 URL 会被平台拒发，回显时把链接改写为 http[:]//a[.]b 形式。"""
    return _URL_RE.sub(lambda m: f"{m[1]}[:]//{m[2].replace('.', '[.]')}", s)


def dump(obj: Any, limit: int = MAX_DUMP) -> str:
    """把任意对象格式化为缩进 JSON（链接去活化），超长截断。"""
    try:
        s = defang(json.dumps(obj, ensure_ascii=False, indent=1, default=str))
    except Exception:
        s = defang(repr(obj))
    if len(s) > limit:
        s = s[:limit] + f"\n…(已截断，共 {len(s)} 字符)"
    return s


def report(r: Result, title: str = "") -> str:
    head = f"【{title}】" if title else ""
    if r.ok:
        return f"{head}{r.brief()}\n{dump(r.data)}"
    return defang(f"{head}{r.brief()}")


# ---------------------------------------------------------------- 板块注册


@dataclass
class Ctx:
    """单次用例执行上下文。"""

    bot: Bot
    event: MessageEvent
    matcher: Matcher
    args: list[str]
    msg: Message

    @property
    def is_group(self) -> bool:
        return isinstance(self.event, GroupMessageEvent)

    @property
    def is_private(self) -> bool:
        return isinstance(self.event, PrivateMessageEvent)

    @property
    def group_id(self) -> int:
        return int(getattr(self.event, "group_id", 0) or 0)

    @property
    def user_id(self) -> int:
        return int(self.event.user_id)

    def arg(self, i: int, default: str = "") -> str:
        return self.args[i] if len(self.args) > i else default

    def at_targets(self) -> list[int]:
        """取参数里 @ 到的用户（排除机器人自身）。"""
        ids = []
        for seg in self.msg:
            if seg.type == "at" and str(seg.data.get("qq", "")).isdigit():
                qq = int(seg.data["qq"])
                if qq != int(self.bot.self_id):
                    ids.append(qq)
        return ids

    async def send(self, message: str | Message | MessageSegment) -> Any:
        return await self.bot.send(self.event, message)

    async def call(self, action: str, **params: Any) -> Result:
        return await call(self.bot, action, **params)

    async def run(self, action: str, title: str = "", **params: Any) -> Result:
        """调用动作并把结果回显到当前会话。"""
        r = await self.call(action, **params)
        await self.send(report(r, title or action))
        return r


Handler = Callable[[Ctx], Awaitable[Any]]


@dataclass
class Case:
    name: str
    desc: str
    func: Handler
    scene: str = ""  # "" 不限 / "group" 仅群 / "private" 仅单聊
    admin: bool = False  # 仅 SUPERUSER 可执行（会改动线上配置 / 群状态）


@dataclass
class Section:
    """一个测试板块 = 一条命令 + 若干子用例。`/<命令> <用例> [参数]`。"""

    command: str
    title: str
    desc: str
    aliases: set[str] = field(default_factory=set)
    cases: dict[str, Case] = field(default_factory=dict)

    def __post_init__(self) -> None:
        SECTIONS.append(self)
        matcher = on_command(self.command, aliases=self.aliases, priority=5, block=True)

        async def _handle(bot: Bot, event: MessageEvent, matcher: Matcher, msg: Message = CommandArg()) -> None:
            await self._dispatch(bot, event, matcher, msg)

        matcher.handle()(_handle)

    def case(self, name: str, desc: str, *, scene: str = "", admin: bool = False):
        def deco(func: Handler) -> Handler:
            self.cases[name] = Case(name, desc, func, scene, admin)
            return func

        return deco

    def usage(self) -> str:
        lines = [f"【{self.title}】/{self.command} <用例> [参数]", self.desc, ""]
        for c in self.cases.values():
            tags = []
            if c.scene == "group":
                tags.append("群")
            elif c.scene == "private":
                tags.append("私")
            if c.admin:
                tags.append("超管")
            tag = f"[{'/'.join(tags)}]" if tags else ""
            lines.append(f"· {c.name}{tag}  {c.desc}")
        return "\n".join(lines)

    async def _dispatch(self, bot: Bot, event: MessageEvent, matcher: Matcher, msg: Message) -> None:
        args = msg.extract_plain_text().split()
        if not args or args[0] not in self.cases:
            await matcher.finish(self.usage())
        case = self.cases[args[0]]
        if case.scene == "group" and not isinstance(event, GroupMessageEvent):
            await matcher.finish(f"用例「{case.name}」仅能在群内执行")
        if case.scene == "private" and not isinstance(event, PrivateMessageEvent):
            await matcher.finish(f"用例「{case.name}」仅能在单聊执行")
        if case.admin and not await SUPERUSER(bot, event):
            await matcher.finish(f"用例「{case.name}」会改动线上状态，仅 SUPERUSERS 可执行")
        ctx = Ctx(bot, event, matcher, args[1:], msg)
        try:
            await case.func(ctx)
        except MatcherException:
            raise  # finish / send 等流程控制异常需继续上抛
        except Exception as e:
            logger.opt(exception=e).error(f"[goqbot_test] 用例 {self.command}/{case.name} 异常")
            await matcher.send(f"用例「{case.name}」异常：{type(e).__name__}: {e}")


SECTIONS: list[Section] = []


# ---------------------------------------------------------------- 测试素材


def tmp_dir() -> Path:
    p = Path(cfg.goqbot_test_tmp_dir) if cfg.goqbot_test_tmp_dir else Path(tempfile.gettempdir()) / "goqbot_test"
    p.mkdir(parents=True, exist_ok=True)
    return p.resolve()


def _png_chunk(tag: bytes, data: bytes) -> bytes:
    return struct.pack(">I", len(data)) + tag + data + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)


def make_png(w: int = 256, h: int = 256, *, noise: bool = False) -> bytes:
    """纯标准库生成 RGB PNG。noise=True 为随机噪点（几乎不可压缩，用于造大图测分片）。"""
    raw = bytearray()
    for y in range(h):
        raw.append(0)  # filter: none
        if noise:
            raw += os.urandom(w * 3)
        else:
            for x in range(w):
                raw += bytes((x * 255 // max(w - 1, 1), y * 255 // max(h - 1, 1), 160))
    ihdr = struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0)
    level = 0 if noise else 6
    return (
        b"\x89PNG\r\n\x1a\n"
        + _png_chunk(b"IHDR", ihdr)
        + _png_chunk(b"IDAT", zlib.compress(bytes(raw), level))
        + _png_chunk(b"IEND", b"")
    )


def b64(data: bytes) -> str:
    return "base64://" + base64.b64encode(data).decode()


def write_tmp(name: str, data: bytes) -> Path:
    p = tmp_dir() / name
    p.write_bytes(data)
    return p


def big_file(name: str, size_mb: float) -> Path:
    """生成指定大小的随机内容文件（已存在且大小一致则复用）。"""
    p = tmp_dir() / name
    size = int(size_mb * 1024 * 1024)
    if not p.exists() or p.stat().st_size != size:
        with p.open("wb") as f:
            left = size
            while left > 0:
                n = min(left, 1024 * 1024)
                f.write(os.urandom(n))
                left -= n
    return p


def big_png(name: str = "big_noise.png", side: int = 1500) -> Path:
    """1500x1500 噪点 PNG ≈ 6.4MB，超过 5MB 直传阈值，走分片上传。"""
    p = tmp_dir() / name
    if not p.exists():
        p.write_bytes(make_png(side, side, noise=True))
    return p


def md_seg(content: str, rows: list[dict] | None = None) -> MessageSegment:
    """构造 goqbot 的 markdown 段：data.data = {markdown, keyboard?}。"""
    inner: dict[str, Any] = {"markdown": {"content": content}}
    if rows:
        inner["keyboard"] = {"content": {"rows": rows}}
    return MessageSegment("markdown", {"data": inner})


def button(
    label: str,
    data: str,
    *,
    type_: int = 1,
    style: int = 1,
    enter: bool = False,
    anchor: int = 0,
    reply: bool = False,
    visited: str = "",
    bid: str = "",
    group_id: str = "",
    permission: int = 2,
    modal: dict | None = None,
) -> dict:
    """keyboard 按钮。type_: 0 跳转 / 1 回调 / 2 指令；permission: 2 所有人 / 1 管理员。"""
    action: dict[str, Any] = {
        "type": type_,
        "permission": {"type": permission},
        "data": data,
        "unsupport_tips": "当前客户端不支持该按钮",
    }
    if enter:
        action["enter"] = True
    if anchor:
        action["anchor"] = anchor
    if reply:
        action["reply"] = True
    if modal:
        action["modal"] = modal
    btn: dict[str, Any] = {
        "id": bid or label,
        "render_data": {"label": label, "visited_label": visited or label, "style": style},
        "action": action,
    }
    if group_id:
        btn["group_id"] = group_id
    return btn


def row(*buttons: dict) -> dict:
    return {"buttons": list(buttons)}
