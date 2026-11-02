"""_install_umid.py —— 从 @qoder-ai/qodercli 提取内嵌 UMID 原生组件（runtime-info）

背景（issue #10）
────────────────
Qoder 官方风控身份桥 runtime-info 在 Windows 上来自已安装的桌面客户端
（<install>/resources/umid/runtime-info.exe）。Linux / macOS / Docker / WSL
没有该文件，网关此前只能退化为派生假身份（derived）——而 issue #10 实测证明
"发送全套派生 cosy-machine* 头"会被服务端判定为非官方客户端，把 CLAIMABLE
的「每日领取 100 Credits」整条过滤。本脚本让 POSIX 环境也能拿到真身份；
没装桌面客户端的 Windows 也可用它提取 runtime-info.exe + sgsdk.dll。

组件来源与提取原理
──────────────────
npm 包 @qoder-ai/qodercli 是纯 JS bundle（tarball ~31MB）；其中
package/bundle/qoder-worker-runtime.mjs 内嵌多个平台的原生组件，以长 base64
字符串字面量（>= BLOB_MIN_LEN 个字符）存放。脚本流程：
  1. 读 npm registry 元数据（标准库 urllib，零第三方依赖）；
  2. 下载 tarball（按 npm integrity 做 sha512 校验）；
  3. 解出 worker-runtime 的 mjs 文本，扫描全部长 base64 字面量；
  4. 按魔数识别格式/架构（ELF e_machine / Mach-O cputype / WASM / PE），
     **不按出现顺序硬编码索引**（上游结构随时可能调整）；
  5. 选出与目标平台/架构匹配的组件，校验后原子写入安装目录
     （默认 <repo>/umid/runtime-info，Windows 为 runtime-info.exe + sgsdk.dll；
     可用 --dest 或 $QD_UMID_DIR 覆盖），chmod 755。

实测对照（qodercli 1.1.65 / 1.1.75 相同，仅作注释记录，不参与选择逻辑）：
  blob[3] Mach-O arm64 1,026,912 B；blob[4] Mach-O x86_64 992,624 B；
  blob[5] ELF x86-64 652,488 B；blob[6] ELF aarch64 660,624 B；
  blob[7] PE x86-64 480,752 B（runtime-info.exe）；
  blob[8] PE x86-64 7,354,928 B（sgsdk.dll，带 IMAGE_FILE_DLL 标志位）；其余为 WASM。

调用契约（官方桌面端 0.4.8 / qodercli 1.1.75 一致；环境码见下）
────────────────────────────────────────────────────────────
    runtime-info <env>                                              # linux
    printf '{"account":"<uid>"}\\n' | runtime-info <env> --account-stdin  # darwin / win32
env：国内版 0；国际版桌面端 3、CLI 4（网关的活动链路按桌面端传 3）。
输出 JSON：{machineToken, machineType, machineCode, vmInfo, ...}

【重要 · 已知限制】机器身份是机器级的，无法按账号拆分
────────────────────────────────────────────────
实测五组（account=alice / account=bob / 空 account / 不存在的 account /
不同 HOME+XDG_CONFIG_HOME）：machineToken 逐字节相同。组件无 CLI 界面
（--help 也输出同一份 JSON）、不落任何状态文件、没有环境变量开关。
→ 提取组件能修复"发假头反被过滤"，让 Linux 拿到**真身份**；
→ 但**不能**让同一台机器上的多个账号各自领到——国际版按**设备**去重
   （每台设备每日仅 1 个国际版账号可领），这是服务端策略，只能靠不同机器
   解决。本脚本与网关都不会尝试伪造不同机器身份。

用法
────
    python _install_umid.py                                # 当前平台（linux / darwin / win32）
    python _install_umid.py --platform linux --arch x64    # 为 WSL / Docker 提取
    python _install_umid.py --platform linux --dry-run     # 只识别不落盘（仍会下载）
"""

import argparse
import base64
import hashlib
import io
import json
import os
import platform as _platform
import re
import struct
import sys
import tarfile
import urllib.request

# ---------------------------------------------------------------------------
# 常量
# ---------------------------------------------------------------------------
BLOB_MIN_LEN = 4000                     # 长 base64 字面量阈值（字符数）
BUNDLE_MEMBER = "package/bundle/qoder-worker-runtime.mjs"
REGISTRY_URL = "https://registry.npmjs.org/@qoder-ai/qodercli"
USER_AGENT = "qoder2api-hub/umid-installer"
TARGET_FILENAME = "runtime-info"
DEFAULT_DEST_DIRNAME = "umid"

# 同一目标出现多个候选时，优先选体积落在该区间的组件（实测 480KB–1MB 的都是
# 真 runtime-info；7.3MB 的 PE 是 Windows 的 sgsdk.dll，另按 DLL 标志位区分）
HEURISTIC_SIZE_MIN = 400 * 1024
HEURISTIC_SIZE_MAX = 2 * 1024 * 1024

ELF_MAGIC = b"\x7fELF"
ELF_EM_X86_64 = 0x3E                    # e_machine: x86-64
ELF_EM_AARCH64 = 0xB7                   # e_machine: aarch64
MACHO_MAGIC_64_LE = b"\xcf\xfa\xed\xfe"   # 64 位小端 Mach-O
MACHO_CPU_X86_64 = 0x01000007
MACHO_CPU_ARM64 = 0x0100000C
WASM_MAGIC = b"\x00asm"
PE_IMAGE_FILE_DLL = 0x2000              # IMAGE_FILE_HEADER.Characteristics: DLL


# ---------------------------------------------------------------------------
# 纯函数（可离线单测；无网络 / 无文件副作用）
# ---------------------------------------------------------------------------
def identify_blob(data):
    """按魔数识别 blob 的格式与架构标签。

    返回：elf-x86_64 / elf-aarch64 / macho-x86_64 / macho-arm64 /
          pe-x86_64 / pe-aarch64 / wasm / unknown
    """
    if not data:
        return "unknown"
    if data[:4] == WASM_MAGIC:
        return "wasm"
    if data[:4] == ELF_MAGIC:
        # EI_CLASS=2（64 位）、EI_DATA=1（小端），e_machine 在 offset 18
        if len(data) < 20 or data[4] != 2 or data[5] != 1:
            return "unknown"
        machine = struct.unpack_from("<H", data, 18)[0]
        if machine == ELF_EM_X86_64:
            return "elf-x86_64"
        if machine == ELF_EM_AARCH64:
            return "elf-aarch64"
        return "unknown"
    if data[:4] == MACHO_MAGIC_64_LE:
        # cputype 在 offset 4（小端 4 字节）
        if len(data) < 8:
            return "unknown"
        cpu = struct.unpack_from("<I", data, 4)[0]
        if cpu == MACHO_CPU_X86_64:
            return "macho-x86_64"
        if cpu == MACHO_CPU_ARM64:
            return "macho-arm64"
        return "unknown"
    if data[:2] == b"MZ":
        # PE：e_lfanew(0x3C) -> "PE\0\0" -> machine
        try:
            off = struct.unpack_from("<I", data, 0x3C)[0]
            if data[off:off + 4] == b"PE\x00\x00":
                machine = struct.unpack_from("<H", data, off + 4)[0]
                if machine == 0x8664:
                    return "pe-x86_64"
                if machine == 0xAA64:
                    return "pe-aarch64"
        except Exception:
            pass
        return "unknown"
    return "unknown"


def scan_bundle_candidates(text, min_len=BLOB_MIN_LEN):
    """扫描 bundle 文本里的长 base64 字面量。

    返回 [(start, end, blob)]：start/end 为 base64 内容在 text 中的起止偏移
    （不含引号），blob 为解码后的 bytes；解码失败的候选跳过。列表顺序即文本
    出现顺序——选择逻辑禁止依赖该顺序（见 select_candidate）。
    """
    pattern = re.compile(r"(['\"])([A-Za-z0-9+/=]{%d,})\1" % int(min_len))
    out = []
    for m in pattern.finditer(text):
        try:
            blob = base64.b64decode(m.group(2), validate=False)
        except Exception:
            continue
        out.append((m.start(2), m.end(2), blob))
    return out


def component_for_platform(platform_name, machine):
    """目标平台/架构 -> 需要的组件标签；不支持时返回 None。

    platform_name: sys.platform 风格（"linux" / "darwin" / "win32"）
    machine:       platform.machine() 风格（x86_64 / AMD64 / aarch64 / arm64）
    win32 返回 None（Windows 需要 exe + dll 两个文件，由 select_win32_pair 处理）。
    """
    p = (platform_name or "").strip().lower()
    m = (machine or "").strip().lower()
    if m in ("amd64", "x86_64", "x64"):
        m = "x86_64"
    elif m in ("arm64", "aarch64"):
        m = "arm64"
    if p.startswith("linux"):
        return {"x86_64": "elf-x86_64", "arm64": "elf-aarch64"}.get(m)
    if p == "darwin":
        return {"x86_64": "macho-x86_64", "arm64": "macho-arm64"}.get(m)
    return None


def verify_component(data, expected):
    """组件 bytes 是否与期望标签一致（魔数 + 架构级校验）。"""
    return bool(data) and identify_blob(data) == expected


def is_pe_dll(data):
    """检查 PE 镜像 COFF 头 Characteristics 是否带 IMAGE_FILE_DLL (0x2000) 标志位。"""
    if identify_blob(data) not in ("pe-x86_64", "pe-aarch64"):
        return False
    try:
        off = struct.unpack_from("<I", data, 0x3C)[0]
        chars = struct.unpack_from("<H", data, off + 22)[0]
        return bool(chars & PE_IMAGE_FILE_DLL)
    except Exception:
        return False


def select_candidate(candidates, expected):
    """从候选里选出 expected 组件；返回 (index, blob) 或 None。

    多个匹配时先按体积启发式过滤（HEURISTIC_SIZE_MIN..MAX），仍多者取最小；
    index 为 candidates 列表下标（与 --pick 对应）。
    """
    hits = [(i, blob) for i, (_start, _end, blob) in enumerate(candidates)
            if identify_blob(blob) == expected]
    if not hits:
        return None
    if len(hits) > 1:
        sized = [h for h in hits
                 if HEURISTIC_SIZE_MIN <= len(h[1]) <= HEURISTIC_SIZE_MAX]
        if sized:
            hits = sized
        hits.sort(key=lambda h: len(h[1]))
    return hits[0]


def select_win32_pair(candidates):
    """从候选里选出 Windows 的 (runtime-info.exe, sgsdk.dll) 二元组。"""
    pe_hits = [(i, blob) for i, (_s, _e, blob) in enumerate(candidates)
               if identify_blob(blob) == "pe-x86_64"]
    exe_cands = [h for h in pe_hits
                 if not is_pe_dll(h[1])
                 and HEURISTIC_SIZE_MIN <= len(h[1]) <= HEURISTIC_SIZE_MAX]
    dll_cands = [h for h in pe_hits
                 if is_pe_dll(h[1]) and len(h[1]) > HEURISTIC_SIZE_MAX]
    if not exe_cands or not dll_cands:
        return None
    exe_cands.sort(key=lambda h: len(h[1]))
    dll_cands.sort(key=lambda h: len(h[1]), reverse=True)
    return exe_cands[0], dll_cands[0]


def is_installed(path, expected):
    """目标文件已存在且校验通过（幂等判定）。"""
    try:
        with open(path, "rb") as fh:
            data = fh.read()
    except OSError:
        return False
    return verify_component(data, expected)


def win32_pair_installed(exe_path, dll_path):
    """Windows 双文件已存在且校验通过（exe 无 DLL 标志位、dll 有）。"""
    try:
        with open(exe_path, "rb") as fh:
            exe = fh.read()
        with open(dll_path, "rb") as fh:
            dll = fh.read()
    except OSError:
        return False
    return (verify_component(exe, "pe-x86_64") and not is_pe_dll(exe)
            and verify_component(dll, "pe-x86_64") and is_pe_dll(dll))


def verify_integrity(data, integrity):
    """校验 npm integrity（sha512-<base64>）。

    返回 True/False；integrity 缺失或算法不支持时返回 None（跳过校验）。
    """
    if not integrity:
        return None
    algo, _, b64 = str(integrity).partition("-")
    if algo != "sha512" or not b64:
        return None
    try:
        expect = base64.b64decode(b64)
    except Exception:
        return None
    return hashlib.sha512(data).digest() == expect


def default_dest_dir():
    """默认安装目录：$QD_UMID_DIR，否则 <repo>/umid。"""
    env = (os.environ.get("QD_UMID_DIR") or "").strip()
    if env:
        return env
    return os.path.join(os.path.dirname(os.path.abspath(__file__)),
                        DEFAULT_DEST_DIRNAME)


# ---------------------------------------------------------------------------
# 网络 / 解包 / 落盘（main 流程用；单测时 stub 这些即可完全离线）
# ---------------------------------------------------------------------------
def resolve_tarball(registry_url=REGISTRY_URL, timeout=60.0):
    """读 npm 元数据 -> (version, tarball_url, integrity)。"""
    req = urllib.request.Request(registry_url, headers={"User-Agent": USER_AGENT})
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        meta = json.load(resp)
    version = str((meta.get("dist-tags") or {}).get("latest") or "")
    entry = (meta.get("versions") or {}).get(version) or {}
    dist = entry.get("dist") or {}
    url = str(dist.get("tarball") or "")
    if not url:
        raise RuntimeError("npm 元数据缺少 dist.tarball（version=%r）" % version)
    return version, url, str(dist.get("integrity") or "")


def download_tarball(url, timeout=300.0, log=print):
    """下载 tarball 到内存（分块读 + 百分比进度）。"""
    req = urllib.request.Request(url, headers={"User-Agent": USER_AGENT})
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        try:
            total = int(resp.headers.get("Content-Length") or 0)
        except Exception:
            total = 0
        chunks = []
        got = 0
        next_tick = 0
        while True:
            chunk = resp.read(256 * 1024)
            if not chunk:
                break
            chunks.append(chunk)
            got += len(chunk)
            if got >= next_tick:
                if total:
                    log("  下载中 %3d%%（%.1f / %.1f MB）"
                        % (got * 100 // total, got / 1e6, total / 1e6))
                else:
                    log("  下载中 %.1f MB" % (got / 1e6))
                next_tick = got + 4 * 1024 * 1024
    return b"".join(chunks)


def extract_runtime_bundle(tgz_bytes):
    """从 tarball 里取出 worker-runtime bundle 的文本。"""
    with tarfile.open(fileobj=io.BytesIO(tgz_bytes), mode="r:gz") as tf:
        for member in tf.getmembers():
            if member.name.lstrip("./") == BUNDLE_MEMBER:
                fh = tf.extractfile(member)
                if fh is None:
                    break
                return fh.read().decode("utf-8", "replace")
    raise RuntimeError("tarball 里找不到 %s（上游包结构可能已变化）" % BUNDLE_MEMBER)


def fetch_candidates():
    """下载 npm 包并校验 sha512，返回 bundle 里的候选组件；校验失败返回 None。"""
    version, url, integrity = resolve_tarball()
    print("npm 包  ：@qoder-ai/qodercli@%s" % version)
    print("下载源  ：%s" % url)
    tgz = download_tarball(url)
    print("下载完成：%.1f MB" % (len(tgz) / 1e6))

    integrity_ok = verify_integrity(tgz, integrity)
    if integrity_ok is False:
        print("错误：tarball 的 sha512 校验失败，中止（可能下载损坏或被篡改）")
        return None
    if integrity_ok is None:
        print("提示：npm 元数据缺少可用的 integrity，跳过包校验。")

    text = extract_runtime_bundle(tgz)
    print("bundle  ：%s（%.1f MB），扫描 >= %d 字符的 base64 字面量…"
          % (BUNDLE_MEMBER, len(text) / 1e6, BLOB_MIN_LEN))
    candidates = scan_bundle_candidates(text)
    print("候选 %d 个：" % len(candidates))
    for i, (_start, _end, blob) in enumerate(candidates):
        print("  [%2d] %10d B  %s" % (i, len(blob), identify_blob(blob)))
    return candidates


def install(data, dest_dir, log=print, filename=TARGET_FILENAME):
    """原子写入 <dest_dir>/<filename> 并 chmod 755；返回目标路径。"""
    os.makedirs(dest_dir, exist_ok=True)
    target = os.path.join(dest_dir, filename)
    tmp = target + ".tmp"
    with open(tmp, "wb") as fh:
        fh.write(data)
    os.chmod(tmp, 0o755)
    os.replace(tmp, target)
    return target


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------
def main(argv=None):
    parser = argparse.ArgumentParser(
        description="提取 @qoder-ai/qodercli 内嵌的 UMID 组件（runtime-info）",
        epilog="机器身份是机器级的：同一台机器上的多个账号不能借此各自领到"
               "（服务端按设备去重），详见模块 docstring。")
    parser.add_argument("--platform", default=sys.platform,
                        help="目标平台：linux / darwin / win32（默认当前平台）")
    parser.add_argument("--arch", default=None,
                        help="目标架构：x64 / arm64（默认当前机器架构）")
    parser.add_argument("--dest", default=None,
                        help="安装目录（默认 $QD_UMID_DIR 或 <repo>/umid）")
    parser.add_argument("--dry-run", action="store_true",
                        help="只下载/识别/报告，不落盘（仍会实际下载 npm 包）")
    parser.add_argument("--force", action="store_true",
                        help="覆盖已存在且校验通过的目标文件")
    parser.add_argument("--pick", type=int, default=None,
                        help="多候选时手动指定（候选列表下标，见输出清单）")
    args = parser.parse_args(argv)

    plat = (args.platform or "").strip().lower()
    machine = args.arch or _platform.machine()
    dest_dir = args.dest or default_dest_dir()
    if plat.startswith("win"):
        exe_target = os.path.join(dest_dir, "runtime-info.exe")
        dll_target = os.path.join(dest_dir, "sgsdk.dll")
        print("目标组件：pe-x86_64 双文件（runtime-info.exe + sgsdk.dll）")
        print("安装路径：%s, %s" % (exe_target, dll_target))
        if (not args.force and not args.dry_run
                and win32_pair_installed(exe_target, dll_target)):
            print("已就绪：现有文件校验通过，跳过下载（--force 可强制重装）。")
            return 0
        candidates = fetch_candidates()
        if candidates is None:
            return 1
        pair = select_win32_pair(candidates)
        if pair is None:
            print("错误：bundle 里未找齐 Windows runtime-info.exe + sgsdk.dll")
            return 1
        (exe_idx, exe_blob), (dll_idx, dll_blob) = pair
        print("选中    ：[%d] runtime-info.exe (%d B), [%d] sgsdk.dll (%d B)"
              % (exe_idx, len(exe_blob), dll_idx, len(dll_blob)))
        if args.dry_run:
            print("dry-run ：不落盘；实际执行时会写入 %s, %s" % (exe_target, dll_target))
            return 0
        install(exe_blob, dest_dir, filename="runtime-info.exe")
        install(dll_blob, dest_dir, filename="sgsdk.dll")
        print("完成    ：%s, %s" % (exe_target, dll_target))
        return 0

    expected = component_for_platform(plat, machine)
    if not expected:
        print("错误：不支持的平台/架构组合：%s / %s" % (plat, machine))
        print("      支持：linux+x64、linux+arm64、darwin+x64、darwin+arm64、win32+x64")
        return 1
    target = os.path.join(dest_dir, TARGET_FILENAME)
    print("目标组件：%s（%s / %s）" % (expected, plat, machine))
    print("安装路径：%s" % target)
    if not args.force and not args.dry_run and is_installed(target, expected):
        print("已就绪：现有文件校验通过，跳过下载（--force 可强制重装）。")
        return 0

    candidates = fetch_candidates()
    if candidates is None:
        return 1

    if args.pick is not None:
        if not 0 <= args.pick < len(candidates):
            print("错误：--pick 越界（有效范围 0..%d）" % (len(candidates) - 1))
            return 1
        blob = candidates[args.pick][2]
        label = identify_blob(blob)
        if label != expected:
            print("错误：--pick %d 是 %s，不是 %s" % (args.pick, label, expected))
            return 1
        picked = (args.pick, blob)
    else:
        picked = select_candidate(candidates, expected)
    if picked is None:
        print("错误：bundle 里没有 %s 组件（上游结构可能已变化）" % expected)
        return 1

    index, blob = picked
    if not verify_component(blob, expected):
        print("错误：选中组件 [%d] 校验失败" % index)
        return 1
    print("选中    ：[%d] %s，%d 字节" % (index, expected, len(blob)))

    if args.dry_run:
        print("dry-run ：不落盘；实际执行时会写入 %s" % target)
        return 0

    path = install(blob, dest_dir)
    print("完成    ：%s（%d 字节，chmod 755）" % (path, len(blob)))
    print("下一步  ：qoder_accounts.runtime_info_exe() 会自动发现该组件（POSIX）；")
    print("          机器身份为机器级——同机多账号不能借此各自领取（服务端按设备去重）。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
