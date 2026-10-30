"""板块三：富媒体 —— 图片 / 视频 / 语音 / 文件，各来源（base64 / URL / file:// / 绝对路径）与分片上传。"""

from nonebot.adapters.onebot.v11 import MessageSegment

from ..common import Ctx, Section, b64, big_file, big_png, cfg, make_png, write_tmp

sec = Section(
    "媒体测试",
    "富媒体",
    "图片/视频/语音/文件收发。本地路径类用例要求 goqbot 与 NoneBot 在同一台机器；"
    "视频/语音需传参或在 .env 配置 GOQBOT_TEST_VIDEO / GOQBOT_TEST_RECORD",
)


def _src(ctx: Ctx, default: str) -> str:
    return ctx.arg(0) or default


@sec.case("图片b64", "base64:// 小图（≤5MB 走 file_data 直传）")
async def _(ctx: Ctx):
    await ctx.send(MessageSegment.image(b64(make_png())))


@sec.case("图片url", "http(s) URL 图片（交给平台拉取），可传参覆盖 URL")
async def _(ctx: Ctx):
    await ctx.send(MessageSegment.image(_src(ctx, cfg.goqbot_test_image_url)))


@sec.case("图片file", "file:// 本地图片")
async def _(ctx: Ctx):
    p = write_tmp("gradient.png", make_png())
    await ctx.send(MessageSegment.image(p.as_uri()))


@sec.case("图片路径", "本地绝对路径图片（不带 file:// 前缀）")
async def _(ctx: Ctx):
    p = write_tmp("gradient.png", make_png())
    await ctx.send(MessageSegment("image", {"file": str(p)}))


@sec.case("大图", "约 6.4MB 噪点 PNG（>5MB 自动分片上传），生成需几秒")
async def _(ctx: Ctx):
    p = big_png()
    await ctx.send(f"开始发送 {p.stat().st_size / 1024 / 1024:.1f}MB 图片（分片）…")
    await ctx.send(MessageSegment.image(p.as_uri()))


@sec.case("图文合一", "文本 + 恰好 1 张图（无 markdown）→ 合并成一个气泡，文字在上")
async def _(ctx: Ctx):
    await ctx.send("图文合一测试：这段文字应和下图在同一个气泡里" + MessageSegment.image(b64(make_png(200, 120))))


@sec.case("多图", "文本 + 2 张图：不满足单图快路径，应拆成多条按序发出")
async def _(ctx: Ctx):
    await ctx.send(
        "多图测试（应拆分）"
        + MessageSegment.image(b64(make_png(80, 80)))
        + MessageSegment.image(b64(make_png(120, 60)))
    )


@sec.case("视频", "video 段；参数为 URL / file:// / 绝对路径，缺省读 GOQBOT_TEST_VIDEO")
async def _(ctx: Ctx):
    src = _src(ctx, cfg.goqbot_test_video)
    if not src:
        await ctx.send("未提供视频来源：/媒体测试 视频 <url|file://|/abs/path.mp4>")
        return
    await ctx.send(MessageSegment("video", {"file": src, "name": "test.mp4"}))


@sec.case("语音", "record 段；参数为 URL / file:// / 绝对路径（建议 silk/wav/mp3），缺省读 GOQBOT_TEST_RECORD")
async def _(ctx: Ctx):
    src = _src(ctx, cfg.goqbot_test_record)
    if not src:
        await ctx.send("未提供语音来源：/媒体测试 语音 <url|file://|/abs/path.silk>")
        return
    name = src.rsplit("/", 1)[-1] if "." in src.rsplit("/", 1)[-1] else "test.silk"
    await ctx.send(MessageSegment("record", {"file": src, "name": name}))


@sec.case("语音别名", "voice / audio 段类型别名（与 record 等价），需同语音来源")
async def _(ctx: Ctx):
    src = _src(ctx, cfg.goqbot_test_record)
    if not src:
        await ctx.send("未提供语音来源：/媒体测试 语音别名 <url|file://|/abs/path>")
        return
    await ctx.send(MessageSegment("voice", {"file": src}))
    await ctx.send(MessageSegment("audio", {"file": src}))


@sec.case("文件b64", "file 段 base64 文本文件（带 name）")
async def _(ctx: Ctx):
    content = "goqbot 文件发送测试\n这是一份 base64 直传的文本文件。\n".encode()
    await ctx.send(MessageSegment("file", {"file": b64(content), "name": "goqbot_test.txt"}))


@sec.case("文件路径", "file 段本地绝对路径，未带 name 时用 basename")
async def _(ctx: Ctx):
    p = write_tmp("goqbot_path_test.txt", "本地路径文件发送测试\n".encode())
    await ctx.send(MessageSegment("file", {"file": str(p)}))


@sec.case("文件url", "file 段 URL（参数指定，需含扩展名）")
async def _(ctx: Ctx):
    src = ctx.arg(0)
    if not src:
        await ctx.send("用法：/媒体测试 文件url <https://…/xxx.pdf>")
        return
    await ctx.send(MessageSegment("file", {"file": src}))


@sec.case("大文件", "约 6MB 随机文件（file://，>5MB 分片上传）；参数可指定 MB 数")
async def _(ctx: Ctx):
    try:
        size = float(ctx.arg(0, "6"))
    except ValueError:
        size = 6.0
    p = big_file(f"big_{size:g}MB.bin", size)
    await ctx.send(f"开始发送 {size:g}MB 文件（分片上传）…")
    await ctx.send(MessageSegment("file", {"file": p.as_uri(), "name": p.name}))


@sec.case("大文件b64", "约 6MB 随机文件走 base64:// 入参（验证大 WS 帧 + 分片）")
async def _(ctx: Ctx):
    p = big_file("big_6MB.bin", 6)
    await ctx.send(MessageSegment("file", {"file": b64(p.read_bytes()), "name": "big_b64.bin"}))


@sec.case("坏路径", "不存在的本地路径：段被丢弃，期望整条为空消息失败（retcode=1400）")
async def _(ctx: Ctx):
    params = {"group_id": ctx.group_id} if ctx.is_group else {"user_id": ctx.user_id}
    action = "send_group_msg" if ctx.is_group else "send_private_msg"
    r = await ctx.call(action, message=[{"type": "image", "data": {"file": "/nonexistent/goqbot.png"}}], **params)
    ok = not r.ok and r.retcode == 1400
    await ctx.send(("✅ 符合预期\n" if ok else "❌ 不符合预期\n") + r.brief())


@sec.case("收图", "引导：向机器人发送图片/视频/语音/文件，由「入站富媒体」观察器回显解析出的段")
async def _(ctx: Ctx):
    await ctx.send(
        "请直接发送图片 / 视频 / 语音 / 文件（群内需 @机器人）。\n"
        "插件会回显入站段 type 与 data（image 应为 file=<md5>.image + url + width/height；"
        "file 应带 name / file_size）。"
    )
