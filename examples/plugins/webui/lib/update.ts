// goqbot 更新：查 GitHub 上的最新版本，下载对应平台的精简包，取出程序后交给 host.update 替换并重启。
// 也可以由页面直接上传新程序（单个可执行文件，或发布页下载的 zip 包）。
import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { inflateRawSync } from "node:zlib";

const REPO = "huilongxiji/gd-goqbot-framework";

export interface ReleaseInfo {
  current: string;
  latest: string;
  has_update: boolean;
  name: string;
  notes: string;
  published_at: string;
  url: string; // 发布页
  asset?: { name: string; size: number; download_url: string };
}

/** 比较 1.2.3 这样的版本号 */
export function compareVersion(a: string, b: string): number {
  const pa = a.replace(/^v/, "").split(".").map((x) => Number.parseInt(x, 10) || 0);
  const pb = b.replace(/^v/, "").split(".").map((x) => Number.parseInt(x, 10) || 0);
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
    const d = (pa[i] ?? 0) - (pb[i] ?? 0);
    if (d) {
      return d;
    }
  }
  return 0;
}

/** 给下载地址加上代理前缀（国内访问 GitHub 慢时用），例如 https://ghfast.top/ */
function withProxy(url: string, proxy: string): string {
  return proxy ? proxy.replace(/\/?$/, "/") + url : url;
}

/** fetch 的网络错误（英文的 fetch failed）转成中文说明 */
function netError(e: unknown, what: string): Error {
  const err = e as Error & { cause?: { code?: string; message?: string } };
  what = /[A-Za-z]$/.test(what) ? what + " " : what; // 中英文之间留空格
  if (err.name === "TimeoutError" || err.name === "AbortError") {
    return new Error(`${what}超时，请检查网络，或在下面设置下载代理`);
  }
  const code = err.cause?.code ?? "";
  const reason: Record<string, string> = {
    ENOTFOUND: "找不到服务器（域名解析失败）",
    EAI_AGAIN: "域名解析失败",
    ECONNREFUSED: "连接被拒绝",
    ECONNRESET: "连接被断开",
    ETIMEDOUT: "连接超时",
    UND_ERR_CONNECT_TIMEOUT: "连接超时",
    CERT_HAS_EXPIRED: "证书已过期",
  };
  return new Error(`${what}失败：${reason[code] ?? (code || err.cause?.message || err.message)}，请检查网络，或在下面设置下载代理`);
}

/** 查最新版本。platform 是 host.info 里的 linux-amd64 这种格式 */
export async function checkRelease(current: string, platform: string, proxy: string, apiBase = "https://api.github.com"): Promise<ReleaseInfo> {
  let res: Response;
  try {
    res = await fetch(`${apiBase}/repos/${REPO}/releases/latest`, {
      headers: { accept: "application/vnd.github+json", "user-agent": "goqbot-webui" },
      signal: AbortSignal.timeout(15_000),
    });
  } catch (e) {
    throw netError(e, "连接 GitHub");
  }
  if (!res.ok) {
    throw new Error(res.status === 403 ? "GitHub 拒绝了请求（访问太频繁），请过一会儿再试" : `查询 GitHub 失败（HTTP ${res.status}）`);
  }
  const r = (await res.json()) as {
    tag_name: string; name: string; body: string; published_at: string; html_url: string;
    assets: { name: string; size: number; browser_download_url: string }[];
  };
  const latest = r.tag_name.replace(/^v/, "");
  const wanted = `goqbot-v${latest}-${platform}-lite.zip`;
  const asset = r.assets.find((a) => a.name === wanted);
  return {
    current,
    latest,
    has_update: compareVersion(latest, current) > 0,
    name: r.name || r.tag_name,
    notes: r.body ?? "",
    published_at: r.published_at,
    url: r.html_url,
    asset: asset && { name: asset.name, size: asset.size, download_url: withProxy(asset.browser_download_url, proxy) },
  };
}

/** 下载文件（最大 200 MB） */
export async function download(url: string): Promise<Buffer> {
  try {
    const res = await fetch(url, { headers: { "user-agent": "goqbot-webui" }, signal: AbortSignal.timeout(10 * 60_000) });
    if (!res.ok || !res.body) {
      throw new Error(`下载失败（HTTP ${res.status}）`);
    }
    const chunks: Buffer[] = [];
    let size = 0;
    for await (const chunk of res.body) {
      size += chunk.length;
      if (size > 200 * 1024 * 1024) {
        throw new Error("下载的文件超过 200 MB，已停止");
      }
      chunks.push(Buffer.from(chunk));
    }
    return Buffer.concat(chunks);
  } catch (e) {
    if ((e as Error).message.startsWith("下载")) {
      throw e;
    }
    throw netError(e, "下载");
  }
}

/** 从 zip 里取出 goqbot 程序（发布包里是 <目录>/goqbot 或 <目录>/goqbot.exe） */
export function extractProgram(zip: Buffer): Buffer {
  // 找中央目录结束记录
  let eocd = -1;
  for (let i = zip.length - 22; i >= Math.max(0, zip.length - 65_557); i--) {
    if (zip.readUInt32LE(i) === 0x06054b50) {
      eocd = i;
      break;
    }
  }
  if (eocd < 0) {
    throw new Error("不是有效的 zip 文件");
  }
  const count = zip.readUInt16LE(eocd + 10);
  let p = zip.readUInt32LE(eocd + 16);
  for (let n = 0; n < count; n++) {
    if (zip.readUInt32LE(p) !== 0x02014b50) {
      throw new Error("zip 文件已损坏");
    }
    const method = zip.readUInt16LE(p + 10);
    const compSize = zip.readUInt32LE(p + 20);
    const nameLen = zip.readUInt16LE(p + 28);
    const extraLen = zip.readUInt16LE(p + 30);
    const commentLen = zip.readUInt16LE(p + 32);
    const local = zip.readUInt32LE(p + 42);
    const name = zip.subarray(p + 46, p + 46 + nameLen).toString("utf8");
    p += 46 + nameLen + extraLen + commentLen;
    const base = name.split("/").pop();
    if (base !== "goqbot" && base !== "goqbot.exe") {
      continue;
    }
    const start = local + 30 + zip.readUInt16LE(local + 26) + zip.readUInt16LE(local + 28);
    const data = zip.subarray(start, start + compSize);
    if (method === 0) {
      return Buffer.from(data);
    }
    if (method === 8) {
      return inflateRawSync(data);
    }
    throw new Error("zip 用了不支持的压缩方式");
  }
  throw new Error("zip 里没有找到 goqbot 程序");
}

/** 把新程序写到数据目录，返回路径和 sha256，交给 host.update */
export async function stage(dataDir: string, data: Buffer): Promise<{ file: string; sha256: string }> {
  const program = data.length > 4 && data.readUInt32LE(0) === 0x04034b50 ? extractProgram(data) : data;
  if (program.length < 1024 * 1024) {
    throw new Error("文件太小，不像是 goqbot 程序");
  }
  const dir = join(dataDir, "update");
  await mkdir(dir, { recursive: true });
  const file = join(dir, "goqbot.new");
  await writeFile(file, program, { mode: 0o755 });
  const sha256 = createHash("sha256").update(await readFile(file)).digest("hex");
  return { file, sha256 };
}
