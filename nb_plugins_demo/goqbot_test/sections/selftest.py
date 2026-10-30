"""一键自测：顺序执行只读 / 预期失败类动作，汇总通过情况。不发送富媒体、不改动任何线上配置。"""

from dataclasses import dataclass
from typing import Any, Callable

from nonebot import on_command
from nonebot.adapters.onebot.v11 import Bot, GroupMessageEvent, MessageEvent, PrivateMessageEvent
from nonebot.matcher import Matcher

from ..common import Result, call


@dataclass
class Check:
    name: str
    action: str
    params: Callable[[MessageEvent], dict[str, Any]]
    expect: int | None = 0  # 期望 retcode；None 表示「接口可达即可」（权限/白名单不足也算通过）
    scene: str = ""


def _gid(e: MessageEvent) -> dict:
    return {"group_id": getattr(e, "group_id", 0)}


CHECKS = [
    Check("登录信息", "get_login_info", lambda e: {}),
    Check("状态", "get_status", lambda e: {}),
    Check("版本", "get_version_info", lambda e: {}),
    Check("网关频控", "get_gateway", lambda e: {}),
    Check("openid 反查", "get_openid", lambda e: {"user_id": e.user_id}),
    Check("get_msg 入站", "get_msg", lambda e: {"message_id": e.message_id}),
    Check("get_msg 未知", "get_msg", lambda e: {"message_id": 1}, 1404),
    Check("get_forward_msg 未知", "get_forward_msg", lambda e: {"id": "x"}, 1404),
    Check("delete_msg 未知", "delete_msg", lambda e: {"message_id": 1}, 1400),
    Check("未知动作", "goqbot_no_such_action", lambda e: {}, 1404),
    Check("put_interaction 缺参", "put_interaction", lambda e: {}, 1400),
    Check("C2C 推送状态", "get_c2c_msg_state", lambda e: {"user_id": e.user_id}),
    Check("菜单查询", "get_menu", lambda e: {}, None),
    Check("面板列表", "get_command_panels", lambda e: {"scope": "group" if isinstance(e, GroupMessageEvent) else "c2c"}, None),
    Check("素材列表", "get_media_images", lambda e: {}, None),
    Check("素材上传缺参", "upload_media_image", lambda e: {}, 1400),
    Check("素材删除缺参", "delete_media_image", lambda e: {}, 1400),
    Check("素材查询缺参", "get_media_image", lambda e: {}, 1400),
    Check("全员禁言(不支持)", "set_group_whole_ban", lambda e: {}, 1404),
    Check("群信息(白名单)", "get_group_info", _gid, None, "group"),
    Check("机器人群状态(白名单)", "get_group_bot_state", _gid, None, "group"),
    Check("成员列表(白名单)", "get_group_member_list", _gid, None, "group"),
    Check("机器人成员信息", "get_group_member_info", lambda e: {**_gid(e), "user_id": int(e.self_id)}, None, "group"),
    Check("入群申请(管理员)", "get_group_join_requests", _gid, None, "group"),
    Check("禁言状态(管理员)", "get_group_ban", _gid, None, "group"),
    Check("空消息", "send_group_msg", lambda e: {**_gid(e), "message": []}, 1400, "group"),
    Check("空消息", "send_private_msg", lambda e: {"user_id": e.user_id, "message": []}, 1400, "private"),
    Check(
        "流式缺 id",
        "send_private_msg",
        lambda e: {"user_id": e.user_id, "message": [{"type": "stream", "data": {"state": 1, "index": 1}}, {"type": "text", "data": {"text": "x"}}]},
        1400,
        "private",
    ),
]


def _judge(c: Check, r: Result) -> bool:
    if c.expect is None:
        # 只要不是框架层的 1404/1400（动作不存在 / 参数错误）即视为接口打通
        return r.ok or r.retcode == 1500
    if c.expect == 0:
        return r.ok
    return not r.ok and r.retcode == c.expect


selftest = on_command("全部自测", aliases={"goqbot自测"}, priority=5, block=True)


@selftest.handle()
async def _(bot: Bot, event: MessageEvent, matcher: Matcher) -> None:
    scene = "group" if isinstance(event, GroupMessageEvent) else "private" if isinstance(event, PrivateMessageEvent) else ""
    lines, passed, total = [], 0, 0
    for c in CHECKS:
        if c.scene and c.scene != scene:
            continue
        total += 1
        r = await call(bot, c.action, **c.params(event))
        good = _judge(c, r)
        passed += good
        want = "ok" if c.expect == 0 else ("可达" if c.expect is None else f"retcode={c.expect}")
        got = "ok" if r.ok else f"retcode={r.retcode}"
        note = f" {r.message[:40]}" if (not r.ok and c.expect is None) else ""
        lines.append(f"{'✅' if good else '❌'} {c.name}  期望 {want} / 实际 {got}{note}")
    head = f"goqbot 一键自测（{'群' if scene == 'group' else '单聊'}）：{passed}/{total} 通过"
    tail = "\n\n说明：「可达」类为白名单/管理员接口，平台返回权限错误(1500 透传)也算接口打通。\n富媒体、按钮、流式、撤回等请按板块手动测试：/测试帮助"
    await matcher.finish(head + "\n" + "\n".join(lines) + tail)
