"""板块十：素材管理 —— upload_media_image / get_media_images / delete_media_image。

上传得到 qqbot-media://image/v1/<token> 引用链接，仅能在 markdown 中以 ![描述 #宽px #高px](url) 引用。
每个机器人最多 100 张、有效期 90 天，上传 / 删除会占用或释放线上额度，仅 SUPERUSERS 可执行。
"""

from ..common import Ctx, Result, Section, b64, cfg, make_png, md_seg, write_tmp

sec = Section("素材测试", "素材管理", "上传图片拿 qqbot-media:// 链接并在 markdown 中引用；上传/删除仅 SUPERUSERS 可执行")

_state: dict = {"last_id": "", "last_url": ""}


def _remember(r: Result) -> None:
    if r.ok:
        _state["last_id"] = (r.data or {}).get("image_id", "")
        _state["last_url"] = (r.data or {}).get("url", "")


def _attached_image(ctx: Ctx) -> dict | None:
    """取命令消息里随附的第一张图片段（goqbot 入站 data.url 为 QQ 真实下载链接）。"""
    for seg in ctx.msg:
        if seg.type == "image" and str(seg.data.get("url", "")).startswith("http"):
            return seg.data
    return None


def _md_size(w: int, h: int, max_w: int = 400) -> str:
    """按原图比例缩放到 max_w 宽以内，生成 markdown 图片的 #宽px #高px。"""
    if w <= 0 or h <= 0:
        return "#256px #256px"
    if w > max_w:
        w, h = max_w, max(1, h * max_w // w)
    return f"#{w}px #{h}px"


async def _upload_and_show(ctx: Ctx, title: str, size: str = "#256px #256px", **params) -> None:
    r = await ctx.run("upload_media_image", title, **params)
    _remember(r)
    if r.ok and _state["last_url"]:
        await ctx.send(md_seg(f"### {title}\n![素材 {size}]({_state['last_url']})\n上方为素材图片引用"))


@sec.case("上传", "命令消息里随附图片则上传该图，否则 base64 上传渐变 PNG；同内容已上传过会直接复用。参数「强制」跳过查重", admin=True)
async def _(ctx: Ctx):
    force = ctx.arg(0) == "强制"
    img = _attached_image(ctx)
    if img:
        size = _md_size(int(img.get("width") or 0), int(img.get("height") or 0))
        # 用 QQ 下发的原始文件名作为 name，之后可按该名查询
        params = {"file": img["url"], "force": force}
        if img.get("file_name"):
            params["name"] = img["file_name"]
        await _upload_and_show(ctx, "素材上传 随附图片", size, **params)
        return
    await _upload_and_show(ctx, "素材上传 base64", file=b64(make_png()), name="gradient.png", force=force)


@sec.case("查询", "get_media_image：/素材测试 查询 <文件名|sha1|image_id>，或随附图片按其 QQ 文件名查询")
async def _(ctx: Ctx):
    img = _attached_image(ctx)
    key = ctx.arg(0) or (img or {}).get("file_name", "")
    if not key:
        await ctx.send("用法：/素材测试 查询 <文件名|sha1|image_id>（或随附一张图片）")
        return
    if len(key) == 40 and all(c in "0123456789abcdefABCDEF" for c in key):
        r = await ctx.run("get_media_image", f"按 sha1 查询 {key}", sha1=key)
    else:
        r = await ctx.run("get_media_image", f"按文件名查询 {key}", file_name=key)
        if r.ok and not (r.data or {}).get("found"):
            await ctx.run("get_media_image", f"按 image_id 查询 {key}", image_id=key)


@sec.case("上传文件", "file:// 本地图片上传", admin=True)
async def _(ctx: Ctx):
    p = write_tmp("media_gradient.png", make_png())
    await _upload_and_show(ctx, "素材上传 file://", file=f"file://{p}")


@sec.case("上传URL", "http(s) 图片：goqbot 先下载再上传；参数为 URL，缺省读 GOQBOT_TEST_IMAGE_URL", admin=True)
async def _(ctx: Ctx):
    await _upload_and_show(ctx, "素材上传 URL", file=ctx.arg(0) or cfg.goqbot_test_image_url)


@sec.case("发送", "markdown 引用素材：/素材测试 发送 [qqbot-media://...]，缺省用最近上传的")
async def _(ctx: Ctx):
    url = ctx.arg(0) or _state["last_url"]
    if not url:
        await ctx.send("用法：/素材测试 发送 <qqbot-media://...>（或先执行「上传」）")
        return
    await ctx.send(md_seg(f"### 素材引用\n![素材 #256px #256px]({url})\n图片下方文字"))


@sec.case("列表", "get_media_images：/素材测试 列表 [cursor]，按创建时间倒序")
async def _(ctx: Ctx):
    params = {"cursor": ctx.arg(0)} if ctx.arg(0) else {}
    await ctx.run("get_media_images", **params)


@sec.case("删除", "delete_media_image：/素材测试 删除 [image_id]，缺省删最近上传的", admin=True)
async def _(ctx: Ctx):
    image_id = ctx.arg(0) or _state["last_id"]
    if not image_id:
        await ctx.send("用法：/素材测试 删除 <image_id>")
        return
    r = await ctx.run("delete_media_image", f"删除素材 {image_id}", image_id=image_id)
    if r.ok and image_id == _state["last_id"]:
        _state["last_id"] = _state["last_url"] = ""


@sec.case("异常", "缺 file / 缺 image_id / 缺查询条件期望 1400；BMP 格式期望框架本地拒绝 1500")
async def _(ctx: Ctx):
    bmp = b"BM" + bytes(64)  # 仅 BMP 魔数，足以被识别为 image/bmp
    checks = [
        (await ctx.call("upload_media_image"), 1400),
        (await ctx.call("delete_media_image"), 1400),
        (await ctx.call("get_media_image"), 1400),
        (await ctx.call("upload_media_image", file=b64(bmp), name="x.bmp"), 1500),
    ]
    lines = [("✅ " if (not r.ok and r.retcode == want) else "❌ ") + r.brief() for r, want in checks]
    await ctx.send("\n".join(lines))
