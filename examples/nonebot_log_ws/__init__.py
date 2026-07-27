"""NoneBot2 日志桥：把 logging 输出推到 WebSocket，供 goqbot sshmon 订阅。

安装（示例）:
  将本目录放到 NoneBot 插件路径，或复制 __init__.py 到你的插件包。

配置 .env / 环境变量:
  LOG_WS_HOST=127.0.0.1
  LOG_WS_PORT=8081

goqbot config.yml:
  sshmon:
    nonebot_log_ws: "ws://127.0.0.1:8081/log"

处理群消息时会尽量带上 group_id，使日志落到对应群聊页。
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
from contextvars import ContextVar
from typing import Any, Set

from nonebot import get_driver, on_message
from nonebot.adapters.onebot.v11 import GroupMessageEvent, MessageEvent
from nonebot.plugin import PluginMetadata

try:
    from fastapi import FastAPI, WebSocket, WebSocketDisconnect
except Exception:  # pragma: no cover
    FastAPI = None  # type: ignore
    WebSocket = None  # type: ignore
    WebSocketDisconnect = Exception  # type: ignore

__plugin_meta__ = PluginMetadata(
    name="nonebot_log_ws",
    description="Stream NoneBot logs over WebSocket for goqbot mobile monitor",
    usage="sshmon.nonebot_log_ws = ws://host:port/log",
)

current_group_id: ContextVar[int | None] = ContextVar("current_group_id", default=None)

_clients: Set[Any] = set()
_loop: asyncio.AbstractEventLoop | None = None


class WSLogHandler(logging.Handler):
    def emit(self, record: logging.LogRecord) -> None:
        try:
            text = self.format(record)
            level = record.levelname.lower()
            if level == "warn":
                level = "warning"
            gid = current_group_id.get()
            payload = {
                "level": level,
                "text": text,
                "group_id": gid or 0,
                "source": "nonebot",
            }
            data = json.dumps(payload, ensure_ascii=False)
            loop = _loop
            if loop and loop.is_running():
                asyncio.run_coroutine_threadsafe(_broadcast(data), loop)
        except Exception:
            pass


async def _broadcast(data: str) -> None:
    dead = []
    for ws in list(_clients):
        try:
            await ws.send_text(data)
        except Exception:
            dead.append(ws)
    for ws in dead:
        _clients.discard(ws)


driver = get_driver()


@driver.on_startup
async def _on_startup() -> None:
    global _loop
    _loop = asyncio.get_running_loop()
    root = logging.getLogger()
    handler = WSLogHandler()
    handler.setFormatter(logging.Formatter("[%(levelname)s] %(name)s: %(message)s"))
    root.addHandler(handler)

    if FastAPI is None:
        logging.getLogger(__name__).warning("fastapi missing; log ws disabled")
        return

    host = os.getenv("LOG_WS_HOST", "127.0.0.1")
    port = int(os.getenv("LOG_WS_PORT", "8081"))

    app = FastAPI()

    @app.websocket("/log")
    async def log_ws(ws: WebSocket):  # type: ignore
        await ws.accept()
        _clients.add(ws)
        try:
            while True:
                await ws.receive_text()
        except WebSocketDisconnect:
            pass
        finally:
            _clients.discard(ws)

    # 挂到 nonebot 已有驱动较复杂；这里独立起一个极简 uvicorn 任务
    try:
        import uvicorn

        config = uvicorn.Config(app, host=host, port=port, log_level="warning")
        server = uvicorn.Server(config)
        asyncio.create_task(server.serve())
        logging.getLogger(__name__).info("nonebot_log_ws on ws://%s:%s/log", host, port)
    except Exception as e:
        logging.getLogger(__name__).error("failed to start log ws: %s", e)


_msg = on_message(priority=1, block=False)


@_msg.handle()
async def _bind_group(event: MessageEvent) -> None:
    if isinstance(event, GroupMessageEvent):
        current_group_id.set(int(event.group_id))
    else:
        current_group_id.set(None)
