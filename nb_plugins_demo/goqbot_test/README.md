# goqbot_test —— goqbot 全功能测试插件

NoneBot2 插件，按板块覆盖 goqbot 当前版本的全部 OneBot 动作、消息段与入站事件。

## 安装

1. 把 `goqbot_test` 目录复制到 NoneBot 项目的插件目录（如 `src/plugins/`），或在 `pyproject.toml` 的 `plugin_dirs` 中加入 `nb_plugins_demo`。
2. 依赖：`nonebot2>=2.2`、`nonebot-adapter-onebot>=2.4`，无其他第三方库（测试图片用标准库生成）。
3. `.env` 中配置 `SUPERUSERS`（改动类用例只允许超管执行），以及可选项：

| 配置项 | 默认值 | 说明 |
|---|---|---|
| `GOQBOT_TEST_IMAGE_URL` | QQ 头像 URL | 图片 URL 用例的网络图片 |
| `GOQBOT_TEST_VIDEO` | 空 | 视频用例的默认来源（URL / `file://` / 绝对路径） |
| `GOQBOT_TEST_RECORD` | 空 | 语音用例的默认来源（建议 silk / wav / mp3） |
| `GOQBOT_TEST_RECALL_DELAY` | `5` | 撤回用例：发送后多少秒撤回 |
| `GOQBOT_TEST_TMP_DIR` | 系统临时目录 `/goqbot_test` | 测试文件生成目录 |
| `GOQBOT_TEST_NOTICE_REPLY` | `true` | 收到好友添加 / 入群等通知时是否自动回消息 |

> 本地路径（`file://`、绝对路径）类用例要求 goqbot 与 NoneBot 在**同一台机器**，goqbot 进程能读到该文件。

## 用法

群内需 `@机器人`。发送 `/测试帮助` 查看板块；发送板块命令（不带参数）查看该板块全部用例；`/<板块> <用例> [参数]` 执行。

| 命令 | 板块 | 覆盖内容 |
|---|---|---|
| `/基础测试` | 基础信息 | `get_login_info` / `get_status` / `get_version_info` / `get_gateway`（含别名）/ `get_openid` / 事件扩展字段 / 元事件 / 未知动作 1404 / `_async` 后缀 |
| `/消息测试` | 消息段 | text / at / reply（含引用机器人消息、失效引用）/ CQ 码字符串入参 / 多段顺序 / `send_msg` 自动分派 / 空消息与未知目标 |
| `/媒体测试` | 富媒体 | 图片 base64 / URL / `file://` / 绝对路径 / >5MB 分片；图文合一；视频；语音（record / voice / audio）；文件（base64 / 路径 / URL / 大文件分片）；坏路径；入站富媒体 |
| `/按钮测试` | Markdown 与按钮 | 原生 markdown、markdown 图片、全类型按钮（回调 / 指令 / 跳转 / 二次确认 / 权限）、按钮组、唤起相册 `anchor=1`、reply+markdown、base64 与顶层 rows 写法、模板（已废弃）、`click_limit`（已废弃） |
| `/互动测试` | 互动回调 | `notice/interaction` → `put_interaction` 回执（可切换回执码）→ event_id 被动回复；派生 message 事件；`send_interaction_ack` 别名 |
| `/撤回测试` | 撤回与取回 | `delete_msg` / `get_msg`（入站 / 出站 / 字段齐全性）/ `get_forward_msg`（含异常） |
| `/发送测试` | 发送模式 | 被动 / `send_group_active_msg` / `send_private_active_msg` / `is_wakeup` 互动召回 / 流式（append / replace / 旧字段 / markdown / 缺 id / 群聊降级 / 夹带图片） |
| `/群管测试` | 群管理 | 群信息 / 机器人状态 / 成员列表分页 / 成员信息 / 入群申请列表与审批 / 禁言与解禁 / 禁言状态 / 全员禁言（不支持）/ 入群申请事件 |
| `/菜单测试` | 菜单与指令面板 | `get_menu` / `set_menu`（自动备份 + 还原）/ 指令面板增删改查与关联对象 / 缺参校验 |
| `/素材测试` | 素材管理 | `upload_media_image`（随附图片 / base64 / `file://` / URL）→ markdown 引用 `qqbot-media://` / 同内容复用与强制重传 / `get_media_image` 按文件名·sha1·image_id 查询 / `get_media_images` 分页 / `delete_media_image` / 缺参与非法格式校验 |
| `/通知测试` | 通知与观察器 | 全部 notice 记录 / `get_c2c_msg_state`（含 openid 入参与别名）/ 入站观察器（富媒体段、forward、recent_messages） |
| `/全部自测` | 一键自测 | 只读与预期失败类动作批量执行，输出通过表 |

## 说明

- 「✅ 符合预期 / ❌ 不符合预期」类用例校验的是框架返回的 retcode；白名单或管理员接口在权限不足时返回平台原始错误（retcode=1500），属于正常现象。
- `菜单设置` / `面板*` / `禁言` / `同意` / `拒绝` / 素材 `上传*` / `删除` 会改动线上状态，仅 SUPERUSERS 可执行；`菜单设置` 需额外带参数 `确认`。
- 回显内容里的链接会被改写为 `http[:]//a[.]b`，避免未备案 URL 导致 QQ 平台拒发。
- 跳转按钮、markdown 链接 / 图片的域名需在 QQ 开放平台配置，否则平台会拒发。
- 连接与重连、失败重发、下游全部掉线兜底、上报格式切换、文件日志等无法由插件触发，`/测试帮助` 末尾列出了手动验证项。
