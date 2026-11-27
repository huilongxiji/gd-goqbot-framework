// goqbot 更新：查 GitHub 上的最新版本，下载对应平台的完整包，取出 goqbot 程序和 WebUI 自己的文件，
// 先换掉 WebUI 的文件，再把程序交给 host.update 替换并重启（程序替换失败时 WebUI 的文件会还原）。
// 也可以由页面直接上传：完整包会连 WebUI 一起更新；精简包或单个程序文件只更新 goqbot 程序。
import { createHash } from "node:crypto";
import { copyFile, mkdir, readFile, rm, stat, unlink, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";
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
  asset?: { name: string; size: number; download_url: string; webui: boolean }; // webui=安装包里带 WebUI 文件
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
  // 优先用完整包（带 WebUI 文件，能一起更新）；没有完整包时退回精简包，只更新程序
  const full = r.assets.find((a) => a.name === `goqbot-v${latest}-${platform}.zip`);
  const lite = r.assets.find((a) => a.name === `goqbot-v${latest}-${platform}-lite.zip`);
  const asset = full ?? lite;
  return {
    current,
    latest,
    has_update: compareVersion(latest, current) > 0,
    name: r.name || r.tag_name,
    notes: r.body ?? "",
    published_at: r.published_at,
    url: r.html_url,
    asset: asset && { name: asset.name, size: asset.size, download_url: withProxy(asset.browser_download_url, proxy), webui: asset === full },
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

interface ZipEntry {
  name: string;
  method: number;
  compSize: number;
  local: number;
}

/** 读出 zip 的文件列表（中央目录） */
function listZip(zip: Buffer): ZipEntry[] {
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
  const entries: ZipEntry[] = [];
  for (let n = 0; n < count; n++) {
    if (p + 46 > zip.length || zip.readUInt32LE(p) !== 0x02014b50) {
      throw new Error("zip 文件已损坏");
    }
    const nameLen = zip.readUInt16LE(p + 28);
    const extraLen = zip.readUInt16LE(p + 30);
    const commentLen = zip.readUInt16LE(p + 32);
    entries.push({
      name: zip.subarray(p + 46, p + 46 + nameLen).toString("utf8"),
      method: zip.readUInt16LE(p + 10),
      compSize: zip.readUInt32LE(p + 20),
      local: zip.readUInt32LE(p + 42),
    });
    p += 46 + nameLen + extraLen + commentLen;
  }
  return entries;
}

/** 解压 zip 里的一个文件 */
function readEntry(zip: Buffer, e: ZipEntry): Buffer {
  const start = e.local + 30 + zip.readUInt16LE(e.local + 26) + zip.readUInt16LE(e.local + 28);
  const data = zip.subarray(start, start + e.compSize);
  if (e.method === 0) {
    return Buffer.from(data);
  }
  if (e.method === 8) {
    return inflateRawSync(data);
  }
  throw new Error("zip 用了不支持的压缩方式");
}

/** WebUI 文件里不随更新覆盖的：本地生成的 plugin.yml、依赖目录、系统文件 */
function skipWebUIFile(rel: string): boolean {
  const parts = rel.split("/");
  return rel === "plugin.yml" || parts.includes("node_modules") || parts.includes(".DS_Store") || parts.includes("__MACOSX");
}

/**
 * 从发布包里取出 goqbot 程序和 WebUI 的文件。
 * 发布包里是 <目录>/goqbot[.exe] 和 <目录>/plugins/webui/...；没有 WebUI 文件（精简包）时 webui 为 null
 */
export function extractPackage(zip: Buffer): { program: Buffer; webui: Map<string, Buffer> | null } {
  let program: Buffer | null = null;
  const webui = new Map<string, Buffer>();
  for (const e of listZip(zip)) {
    if (e.name.endsWith("/")) {
      continue; // 目录
    }
    const parts = e.name.split("/");
    const base = parts[parts.length - 1];
    if ((base === "goqbot" || base === "goqbot.exe") && parts.length <= 2) {
      program = readEntry(zip, e);
      continue;
    }
    const at = parts.indexOf("plugins");
    if (at >= 0 && at <= 1 && parts[at + 1] === "webui" && parts.length > at + 2) {
      const relParts = parts.slice(at + 2);
      if (relParts.some((x) => x === "" || x === "." || x === ".." || x.includes("\\") || x.includes(":"))) {
        throw new Error(`安装包里有不安全的文件路径：${e.name}`);
      }
      const rel = relParts.join("/");
      if (!skipWebUIFile(rel)) {
        webui.set(rel, readEntry(zip, e));
      }
    }
  }
  if (!program) {
    throw new Error("zip 里没有找到 goqbot 程序");
  }
  // 没有入口文件的不算完整的 WebUI，只更新程序
  return { program, webui: webui.has("index.ts") ? webui : null };
}

export interface Staged {
  file: string; // 新程序，交给 host.update
  sha256: string;
  webui: Map<string, Buffer> | null; // 新的 WebUI 文件（相对 WebUI 目录的路径 → 内容），没有则为 null
}

/** 把新程序写到数据目录，算好 sha256；完整包里的 WebUI 文件一并取出 */
export async function stage(dataDir: string, data: Buffer): Promise<Staged> {
  let program = data;
  let webui: Map<string, Buffer> | null = null;
  if (data.length > 4 && data.readUInt32LE(0) === 0x04034b50) {
    ({ program, webui } = extractPackage(data));
  }
  if (program.length < 1024 * 1024) {
    throw new Error("文件太小，不像是 goqbot 程序");
  }
  const dir = join(dataDir, "update");
  await mkdir(dir, { recursive: true });
  const file = join(dir, "goqbot.new");
  await writeFile(file, program, { mode: 0o755 });
  const sha256 = createHash("sha256").update(await readFile(file)).digest("hex");
  return { file, sha256, webui };
}

/**
 * 用新文件覆盖 WebUI 目录。覆盖前把原来的文件备份到 dataDir/update/backup/，
 * 返回还原函数：程序替换失败时调用，把被覆盖的文件放回去、删掉新加的文件。
 * 旧版本里有、新版本里没有的文件保留不动。
 */
export async function applyWebUI(dataDir: string, webuiDir: string, files: Map<string, Buffer>): Promise<() => Promise<void>> {
  const backupDir = join(dataDir, "update", "backup");
  await rm(backupDir, { recursive: true, force: true });
  const replaced: string[] = []; // 覆盖了原有文件的
  const added: string[] = []; // 原来没有、新加的
  const restore = async () => {
    for (const rel of replaced) {
      await copyFile(join(backupDir, rel), join(webuiDir, rel)).catch(() => {});
    }
    for (const rel of added) {
      await unlink(join(webuiDir, rel)).catch(() => {});
    }
  };
  try {
    for (const [rel, content] of files) {
      const target = join(webuiDir, rel);
      const existed = await stat(target).then((st) => st.isFile(), () => false);
      if (existed) {
        await mkdir(dirname(join(backupDir, rel)), { recursive: true });
        await copyFile(target, join(backupDir, rel));
      }
      await mkdir(dirname(target), { recursive: true });
      await writeFile(target, content);
      (existed ? replaced : added).push(rel);
    }
  } catch (e) {
    await restore();
    throw new Error(`更新 WebUI 文件失败，已还原：${(e as Error).message}`);
  }
  return restore;
}
