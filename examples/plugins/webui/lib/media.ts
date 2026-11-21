// 图片缓存：QQ 发来的图片地址带有时效（rkey），过一段时间就打不开了。
// 收到消息时先把图片下载到 plugin-data/webui/media/，页面优先显示本地这份，原地址只作备用。
//
// 文件名用图片内容的 md5（QQ 给的 file / filename 字段就是它），拿不到时用去掉 rkey 的地址算 sha1；
// 页面里用 /media/<文件名>?s=<签名> 访问（<img> 带不了登录口令）。签名用的密钥随机生成，存在 media/.key，
// 和登录口令无关，所以改口令后已保存的图片地址照样能用。
// 缓存超过上限（默认 1 GB，可在设置里改）时，按最后修改时间删掉最早的，降到上限的 80%；
// 设置了聊天记录保留天数时，超过天数的图片也会删掉。
import { createHash, createHmac, randomBytes, timingSafeEqual } from "node:crypto";
import { mkdir, readdir, readFile, rename, stat, unlink, writeFile } from "node:fs/promises";
import { join } from "node:path";

const MAX_FILE = 20 * 1024 * 1024; // 单张图片上限
const NAME_RE = /^[0-9a-f]{32,40}$/;

/** 按文件头判断图片类型，返回 content-type；不是图片时为空 */
export function imageType(b: Buffer): string {
  const hex = b.subarray(0, 12).toString("hex");
  if (hex.startsWith("89504e47")) return "image/png";
  if (hex.startsWith("ffd8ff")) return "image/jpeg";
  if (hex.startsWith("47494638")) return "image/gif";
  if (hex.startsWith("424d")) return "image/bmp";
  if (hex.startsWith("52494646") && b.subarray(8, 12).toString("ascii") === "WEBP") return "image/webp";
  return "";
}

type Obj = Record<string, unknown>;

export class MediaCache {
  readonly dir: string;
  private secret: Buffer = randomBytes(32);
  private pending = new Map<string, Promise<void>>();
  private sinceprune = 0;
  private maxBytes = 1024 * 1024 * 1024; // 缓存总大小上限，0=不限制
  private keepDays = 0; // 保留天数，0=不按天数删除
  private total = 0; // 上次清理时统计的总大小，加上之后下载的

  /** 设置缓存上限和保留天数，立即按新设置清理一次 */
  setLimits(maxBytes: number, keepDays: number): Promise<void> {
    this.maxBytes = Math.max(0, Math.floor(maxBytes) || 0);
    this.keepDays = Math.max(0, Math.floor(keepDays) || 0);
    return this.prune();
  }

  /** 图片缓存现在占的空间（字节） */
  usage(): number {
    return this.total;
  }

  constructor(dataDir: string) {
    this.dir = join(dataDir, "media");
  }

  async init(): Promise<void> {
    await mkdir(this.dir, { recursive: true });
    const keyFile = join(this.dir, ".key");
    try {
      const key = await readFile(keyFile);
      if (key.length >= 32) this.secret = key;
      else throw new Error("密钥太短");
    } catch {
      await writeFile(keyFile, this.secret, { mode: 0o600 });
    }
    void this.prune().catch(() => {});
    // 每 6 小时检查一次，按保留天数删掉旧图片（下载很少时也能按时清理）
    setInterval(() => void this.prune().catch(() => {}), 6 * 3600_000).unref?.();
  }

  /** 图片的缓存文件名：优先用 QQ 给的 md5，否则用去掉 rkey 的地址算 sha1 */
  nameFor(url: string, hint: unknown): string {
    const m = /^([0-9a-fA-F]{32})(?:\.|$)/.exec(String(hint ?? ""));
    if (m) return m[1].toLowerCase();
    let key = url;
    try {
      const u = new URL(url);
      u.searchParams.delete("rkey");
      key = u.toString();
    } catch {
      // 不是合法地址时按原文算
    }
    return createHash("sha1").update(key).digest("hex");
  }

  private sign(name: string): string {
    return createHmac("sha256", this.secret).update(name).digest("hex").slice(0, 20);
  }

  /** 页面访问这张图的地址 */
  pathFor(name: string): string {
    return `/media/${name}?s=${this.sign(name)}`;
  }

  verify(name: string, sig: string): boolean {
    if (!NAME_RE.test(name)) return false;
    const want = Buffer.from(this.sign(name));
    const got = Buffer.from(String(sig ?? ""));
    return want.length === got.length && timingSafeEqual(want, got);
  }

  /** 读缓存文件；没有时返回 null */
  async read(name: string): Promise<{ data: Buffer; type: string } | null> {
    if (!NAME_RE.test(name)) return null;
    try {
      const data = await readFile(join(this.dir, name));
      return { data, type: imageType(data) || "application/octet-stream" };
    } catch {
      return null;
    }
  }

  /**
   * 给一条消息里的图片加上本地地址（data.local），并在后台下载。
   * 包括消息段里的图片、聊天记录（forward）节点里的图片，以及引用消息附件（event.msg_elements[].attachments）
   */
  attach(segs: Obj[], ev?: Obj): void {
    const visitSegs = (list: unknown, depth: number) => {
      if (!Array.isArray(list) || depth > 4) return;
      for (const s of list as Obj[]) {
        if (!s || typeof s !== "object") continue;
        const d = (s.data && typeof s.data === "object" ? s.data : {}) as Obj;
        if (s.type === "image") {
          const url = [d.url, d.file].map(String).find((u) => /^https?:\/\//i.test(u));
          if (url) d.local = this.track(url, d.file);
        } else if (s.type === "forward" || s.type === "node") {
          const nodes = Array.isArray(d.content) ? d.content : [];
          for (const n of nodes as Obj[]) {
            const nd = (n?.data && typeof n.data === "object" ? n.data : n) as Obj;
            visitSegs(nd?.content, depth + 1);
          }
        }
      }
    };
    visitSegs(segs, 0);
    const visitElements = (els: unknown, depth: number) => {
      if (!Array.isArray(els) || depth > 4) return;
      for (const el of els as Obj[]) {
        if (!el || typeof el !== "object") continue;
        for (const a of (Array.isArray(el.attachments) ? el.attachments : []) as Obj[]) {
          const type = String(a?.content_type ?? "");
          const url = String(a?.url ?? "");
          if ((type === "" || type.startsWith("image")) && /^https?:\/\//i.test(url)) a.local = this.track(url, a.filename);
        }
        visitElements(el.msg_elements, depth + 1);
      }
    };
    visitElements(ev?.msg_elements, 0);
  }

  /** 开始下载（同一张图只下一次），返回页面用的地址 */
  private track(url: string, hint: unknown): string {
    const name = this.nameFor(url, hint);
    if (!this.pending.has(name)) {
      const job = this.download(url, name).catch(() => {}).finally(() => this.pending.delete(name));
      this.pending.set(name, job);
    }
    return this.pathFor(name);
  }

  private async download(url: string, name: string): Promise<void> {
    const file = join(this.dir, name);
    try {
      await stat(file);
      return; // 已经缓存过
    } catch {
      // 没有缓存，继续下载
    }
    const res = await fetch(url, { signal: AbortSignal.timeout(20_000), headers: { "user-agent": "Mozilla/5.0 goqbot-webui" } });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    if (Number(res.headers.get("content-length") ?? 0) > MAX_FILE) throw new Error("图片太大");
    const data = Buffer.from(await res.arrayBuffer());
    if (data.length > MAX_FILE || !imageType(data)) throw new Error("不是图片");
    await writeFile(file + ".part", data);
    await rename(file + ".part", file);
    this.total += data.length;
    if (++this.sinceprune >= 100) {
      this.sinceprune = 0;
      void this.prune().catch(() => {});
    }
  }

  /** 删掉超过保留天数的；缓存超过上限时，按修改时间从旧到新删除 */
  private async prune(): Promise<void> {
    const cutoff = this.keepDays > 0 ? Date.now() - this.keepDays * 86400_000 : 0;
    const files: { path: string; size: number; time: number }[] = [];
    let total = 0;
    for (const name of await readdir(this.dir)) {
      if (!NAME_RE.test(name)) continue; // 只清理图片，不动 .key 和下载中的 .part
      const path = join(this.dir, name);
      try {
        const st = await stat(path);
        if (!st.isFile()) continue;
        if (cutoff && st.mtimeMs < cutoff) {
          await unlink(path);
          continue;
        }
        files.push({ path, size: st.size, time: st.mtimeMs });
        total += st.size;
      } catch {
        // 文件刚被删掉
      }
    }
    this.total = total;
    if (!this.maxBytes || total <= this.maxBytes) return;
    const target = this.maxBytes * 0.8;
    files.sort((a, b) => a.time - b.time);
    for (const f of files) {
      if (total <= target) break;
      try {
        await unlink(f.path);
        total -= f.size;
      } catch {
        // 删不掉就跳过
      }
    }
    this.total = total;
  }
}
