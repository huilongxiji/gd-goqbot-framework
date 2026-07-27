# NoneBot 日志 WebSocket 桥

把 NoneBot2 的 logging 输出推到 `ws://HOST:PORT/log`，供 goqbot `sshmon.nonebot_log_ws` 订阅。

## 安装

1. 将本目录作为插件加载，或把 `__init__.py` 拷入你的插件包并注册。  
2. 依赖：`fastapi`、`uvicorn`（一般 NoneBot 已具备）。  
3. 环境变量（可选）：
   - `LOG_WS_HOST=127.0.0.1`
   - `LOG_WS_PORT=8081`

## goqbot 配置

```yaml
sshmon:
  nonebot_log_ws: "ws://127.0.0.1:8081/log"
```

## 协议

每条日志为 JSON 文本帧：

```json
{"level":"info","text":"...","group_id":123456,"source":"nonebot"}
```

`group_id` 在处理群消息时由 ContextVar 注入，便于 App 落到对应群聊灰字时间线。
