#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
mkpkg.py —— 从本仓库 src/ 取材，产出三件可直接交付给设备的东西：

    1) imsengine_<ver>-<rel>_<arch>.ipk        IMS 注册引擎（含旁路上网守护、面板补丁片段）
    2) luci-app-fzs_<ver>-<rel>_all.ipk        FZS 面板（含 IMS 短信界面）
    3) fzs-ims-suite-<ver>.tar.gz              系统恢复包
                                               （LuCI → 系统 → 升级/备份 → 恢复备份上传）

为什么还要「恢复包」：厂商固件的「刷写固件」框做了 platform_check_image 放通，
但那会直写 flash 固件分区，伪造的 .bin 必砖；而「恢复备份」框本质是
    tar -C / -xzf <file>
只向根目录覆盖文件，不碰 flash，所以本机队列/ISP 配置也能留住。这是唯一安全的
「不动分区的系统升级」通道。

ipk 容器格式说明（踩过坑）：
    opkg 原生是 ar 归档，但这台 CPE 的 opkg 只认「外层 tar.gz」包装，
    ar 包装会被拒收（Malformed package file）。故本脚本外层用 gzip tar，
    内含 ./debian-binary ./control.tar.gz ./data.tar.gz 三个成员。

用法：
    python3 tools/mkpkg.py            # 输出到 build/packages/
"""
import gzip
import hashlib
import io
import os
import sys
import tarfile

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
BOARD = os.path.join(ROOT, "src", "board-files")
OUT = os.environ.get("FZS_PKG_OUT", os.path.join(ROOT, "build", "packages"))

# IMS 引擎随本仓维护；面板取自独立项目 fzs1-panel 的 Release（不在此仓存源码副本）
ENGINE_VER, ENGINE_REL, ENGINE_ARCH = "0.1.7", "1", "mipsel_24kc"
PANEL_VER, PANEL_REL, PANEL_ARCH = "0.3.2", "1", "all"
PANEL_REPO = os.environ.get("FZS_PANEL_REPO", "jianglihai/fzs1-panel")
SUITE_VER = ENGINE_VER

# ---------------------------------------------------------------------------
# IMS 引擎包：文件路径（相对 src/board-files/）与安装权限
# ---------------------------------------------------------------------------
ENGINE_FILES = [
    ("usr/bin/imsengine",                          0o755),
    ("etc/init.d/ims",                             0o755),
    ("etc/config/ims",                             0o644),
    ("usr/libexec/ims-ppp",                        0o755),
    ("usr/libexec/ims-data",                       0o755),
    ("usr/lib/imsengine/patch-fzs",                0o755),
    ("usr/lib/imsengine/fzs-at.sms.snippet",       0o644),
    ("usr/lib/imsengine/fzs-at.ims.snippet",       0o644),
    ("usr/lib/imsengine/sms-ims-card.snippet",     0o644),
    ("usr/lib/imsengine/full/fzs-at",              0o644),
    ("usr/lib/imsengine/full/sms.js",              0o644),
    ("etc/operators/46000.json",                   0o644),
    ("etc/operators/46001.json",                   0o644),
    ("etc/operators/46011.json",                   0o644),
    ("lib/upgrade/keep.d/imsengine",               0o644),
]

ENGINE_CONTROL = """Package: imsengine
Version: {ver}-{rel}
Depends: libc
Section: net
Architecture: {arch}
Maintainer: SimAdmin
Source: imsengine
Description: Userland IMS registration engine (SMS over IMS) for EC200T
 Runs on OpenWrt CPE without the module native IMS stack:
 SIP REGISTER -> 401 -> USIM AKA (AT+CSIM) -> userland ESP -> 200 OK.
 Auto-detects modules with native IMS (EC200N) and switches to bypass mode,
 where ims-data binds the data CID for internet while native IMS keeps SMS.
"""

ENGINE_POSTINST = """#!/bin/sh
[ -n "${IPKG_INSTROOT}" ] && exit 0
/etc/init.d/ims enable
/usr/lib/imsengine/patch-fzs
# 升级场景 service 已在跑（restart 换掉旧进程）；
# 全新安装时 running 为假，直接 restart 会向 ubus 打一条 "Not found"，故分支处理
if /etc/init.d/ims running 2>/dev/null; then
	/etc/init.d/ims restart
else
	/etc/init.d/ims start
fi
exit 0
"""

# 卸载前先还原厂商 fzs-at / sms.js，避免面板残留调用已被删除的 IMS 接口
ENGINE_PRERM = """#!/bin/sh
[ -n "${IPKG_INSTROOT}" ] && exit 0
[ -x /usr/lib/imsengine/patch-fzs ] && /usr/lib/imsengine/patch-fzs restore
/etc/init.d/ims stop 2>/dev/null
/etc/init.d/ims disable 2>/dev/null
exit 0
"""

PANEL_CONTROL = """Package: luci-app-fzs
Version: {ver}-{rel}
Depends: libc, luci-base, rpcd
Section: luci
Architecture: {arch}
Maintainer: zzt718
Source: luci-app-fzs
Description: LuCI panel for FZS-series 4G CPE (SMS / APN / band / cell / network)
 Ships the IMS SMS tab: history janitor (fzs-sms.uc), per-SIM SMS number
 memory and the IMS card row on the overview page.
"""

PANEL_POSTINST = """#!/bin/sh
[ -n "${IPKG_INSTROOT}" ] && exit 0
/etc/init.d/fzs-sms enable
rm -f /tmp/luci-indexcache /tmp/luci-modulecache* 2>/dev/null
if /etc/init.d/fzs-sms running 2>/dev/null; then
	/etc/init.d/fzs-sms restart
else
	/etc/init.d/fzs-sms start
fi
exit 0
"""

PANEL_PRERM = """#!/bin/sh
[ -n "${IPKG_INSTROOT}" ] && exit 0
/etc/init.d/fzs-sms stop 2>/dev/null
/etc/init.d/fzs-sms disable 2>/dev/null
rm -f /tmp/luci-indexcache /tmp/luci-modulecache* 2>/dev/null
exit 0
"""

# ---------------------------------------------------------------------------
# 恢复包附加内容
# ---------------------------------------------------------------------------
# service enable 生成的开机链接（恢复包不走 postinst，故内置）
RCLINKS = {
    "./etc/rc.d/S95ims":     "../init.d/ims",
    "./etc/rc.d/K90ims":     "../init.d/ims",
    "./etc/rc.d/S95fzs-sms": "../init.d/fzs-sms",
}

# 不打包：用户既有配置，覆盖了会把人家短信/转发设置冲掉
SUITE_EXCLUDE = {"./etc/config/fzs-sms"}

# imsengine 的路径前缀（其余归面板），用于补登记脚本生成 opkg 的 .list
ENGINE_PREFIXES = (
    "usr/bin/imsengine", "etc/init.d/ims", "etc/config/ims", "etc/operators/",
    "usr/libexec/ims-ppp", "usr/libexec/ims-data", "usr/lib/imsengine/",
    "lib/upgrade/keep.d/imsengine",
)

CHECK_SH = r"""#!/bin/sh
# fzs-ims-check —— IMS 升级包装完后的自检（只读，不改动任何东西）
echo "===== 1. 版本 ====="
opkg list-installed 2>/dev/null | grep -Ei 'imsengine|luci-app-fzs' || echo "(opkg 里查不到 = 用恢复包装的，正常)"
echo
echo "===== 2. 关键文件 ====="
for f in /usr/bin/imsengine /usr/libexec/ims-data /usr/libexec/ims-ppp \
         /usr/lib/imsengine/patch-fzs /etc/init.d/ims /usr/libexec/fzs-at /usr/bin/fzs-atio; do
    [ -e "$f" ] && echo "  OK   $f" || echo "  MISS $f"
done
echo
echo "===== 3. 开机自启 ====="
for l in /etc/rc.d/S95ims /etc/rc.d/S95fzs-sms; do
    [ -L "$l" ] && echo "  OK   $l -> $(readlink "$l")" || echo "  MISS $l"
done
echo
echo "===== 4. 面板补丁（是否注入 IMS 分支）====="
/usr/lib/imsengine/patch-fzs status 2>/dev/null || grep -c 'ims-bridge' /usr/libexec/fzs-at 2>/dev/null
echo
echo "===== 5. IMS 状态 ====="
cat /tmp/ims_status.txt 2>/dev/null || echo "(无状态文件：服务没起 / 刚装还没重启)"
echo
echo "===== 6. 进程 ====="
ps | grep -E 'imsengine|ims-data|ims-ppp|fzs-smsd' | grep -v grep || echo "(都没有)"
echo
echo "===== 7. 默认路由 / 上网 ====="
ip route | grep default
ping -c 2 -W 3 223.5.5.5 2>&1 | tail -2
"""

REGISTER_SH = r"""#!/bin/sh
# fzs-ims-register-opkg —— 用「恢复备份包」装完 IMS 后，向 opkg 补登记
#
# 为什么需要：sysupgrade -r（恢复备份）只做 tar -C / -xzf，**不走 postinst**，
#   于是 opkg 数据库里既没有 imsengine，面板版本也还停留在厂商旧号。
#   后果：opkg list-installed 查不到、opkg remove 说没装、别人看面板版本被误导。
#   本脚本补齐元数据（先备份 /usr/lib/opkg/status，可回滚）。
#
# 用法： fzs-ims-register-opkg
# 回滚： cp /usr/lib/opkg/status.bak-imsbundle /usr/lib/opkg/status
set -e
ENGINE_VER='%ENGINE_VER%'
PANEL_VER='%PANEL_VER%'
NOW=$(date +%s)
ST=/usr/lib/opkg/status

echo "[1/5] 备份 status -> $ST.bak-imsbundle"
cp -a "$ST" "$ST.bak-imsbundle"

echo "[2/5] 登记 imsengine"
if grep -q '^Package: imsengine$' "$ST"; then
    awk 'BEGIN{RS="";ORS="\n\n"} !/(^|\n)Package: imsengine(\n|$)/' "$ST.bak-imsbundle" > /tmp/.st
    mv /tmp/.st "$ST"
fi
{
    echo "Package: imsengine"
    echo "Version: $ENGINE_VER"
    echo "Depends: libc"
    echo "Status: install user installed"
    echo "Architecture: mipsel_24kc"
    echo "Installed-Time: $NOW"
    echo ""
} >> "$ST"

echo "[3/5] 面板版本对齐实际文件 -> $PANEL_VER"
sed -i '/^Package: luci-app-fzs$/,/^Version:/ s/^Version:.*/Version: '"$PANEL_VER"'/' "$ST"

echo "[4/5] 写 /usr/lib/opkg/info/*"
mkdir -p /usr/lib/opkg/info
cat > /usr/lib/opkg/info/imsengine.control << 'EOF'
Package: imsengine
Version: %ENGINE_VER%
Depends: libc
Section: net
Architecture: mipsel_24kc
Maintainer: SimAdmin
Source: imsengine
Description: Userland IMS registration engine (SMS over IMS) for EC200T
 (installed via fzs-ims-suite restore bundle)
EOF
sed -i "s/%%ENGINE_VER%%/$ENGINE_VER/" /usr/lib/opkg/info/imsengine.control
cat > /usr/lib/opkg/info/imsengine.conffiles << 'EOF'
/etc/config/ims
EOF
cat > /usr/lib/opkg/info/imsengine.list << 'EOF'
%ENGINE_LIST%
EOF

echo "[5/5] 完成。核对："
opkg list-installed 2>/dev/null | grep -Ei 'imsengine|luci-app-fzs'
echo "回滚: cp /usr/lib/opkg/status.bak-imsbundle /usr/lib/opkg/status"
"""


# ---------------------------------------------------------------------------
# 基础工具
# ---------------------------------------------------------------------------
def read(rel, base):
    p = os.path.join(base, rel)
    if not os.path.exists(p):
        die("缺少源文件: " + p)
    with open(p, "rb") as f:
        return f.read()


def die(msg):
    print("ERROR: " + msg, file=sys.stderr)
    sys.exit(1)


def tar_gz(members):
    """members: [(name, mode, bytes_or_None_for_dir)]"""
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz", compresslevel=9,
                      format=tarfile.GNU_FORMAT) as tf:
        for name, mode, content in members:
            ti = tarfile.TarInfo(name)
            if content is None:
                ti.type = tarfile.DIRTYPE
                ti.size = 0
                content = b""
            else:
                ti.size = len(content)
            ti.mode = mode
            ti.mtime = 0
            ti.uid = ti.gid = 0
            ti.uname = ti.gname = "root"
            tf.addfile(ti, io.BytesIO(content))
    return buf.getvalue()


def build_data(name_map):
    """name_map: {'./usr/bin/x': (mode, bytes)} → data.tar.gz"""
    members = []
    # opkg 解包不自动建父目录，缺目录项会报 No such file or directory
    dirs = set()
    for arc in name_map:
        d = os.path.dirname(arc).rstrip("/")
        while d and d not in (".", ""):
            dirs.add(d + "/")
            d = os.path.dirname(d).rstrip("/")
    for d in sorted(dirs):
        members.append((d, 0o755, None))
    for arc in sorted(name_map):
        mode, content = name_map[arc]
        members.append((arc, mode, content))
    return tar_gz(members)


def make_ipk(name_map, control, conffiles, postinst, prerm, out_path):
    ctrl = tar_gz([
        ("./control",   0o644, control.encode()),
        ("./conffiles", 0o644, conffiles.encode()),
        ("./postinst",  0o755, postinst.encode()),
        ("./prerm",     0o755, prerm.encode()),
    ])
    data = build_data(name_map)
    blob = tar_gz([
        ("./debian-binary",  0o644, b"2.0\n"),
        ("./control.tar.gz", 0o644, ctrl),
        ("./data.tar.gz",    0o644, data),
    ])
    os.makedirs(os.path.dirname(out_path), exist_ok=True)
    with open(out_path, "wb") as f:
        f.write(blob)
    print("  %-42s %7d B  (%d 文件, data %d B)"
          % (os.path.basename(out_path), len(blob), len(name_map), len(data)))
    return blob


def collect_engine():
    m = {}
    for rel, mode in ENGINE_FILES:
        m["./" + rel] = (mode, read(rel, BOARD))
    return m


def fetch_panel_ipk():
    """取面板 ipk：本地文件优先（FZS_PANEL_IPK），否则从面板项目 Release 下载。

    ★ 面板源码不在这个仓库 —— 真源是独立的 fzs1-panel 项目，本仓只消费它发布的
      ipk（README「从源码构建」如此约定）。曾经在 src/luci-app-fzs-files/ 放过一份
      源码副本，导致面板源码两处分叉、版本对不上难排查，已移除；此处也因此必须
      保留 os.walk 之外的一条取材路径。
    """
    local = os.environ.get("FZS_PANEL_IPK", "")
    if local and os.path.isfile(local):
        print("[mkpkg] 面板来源：本地 %s" % local)
        with open(local, "rb") as f:
            return f.read()

    ver_full = "%s-%s" % (PANEL_VER, PANEL_REL)
    url = os.environ.get("FZS_PANEL_URL") or (
        "https://github.com/%s/releases/download/v%s/luci-app-fzs_%s_%s.ipk"
        % (PANEL_REPO, ver_full, ver_full, PANEL_ARCH))
    dst = os.path.join(OUT, "luci-app-fzs_%s_%s.ipk" % (ver_full, PANEL_ARCH))
    if os.path.isfile(dst) and os.path.getsize(dst) > 0:
        print("[mkpkg] 面板来源：缓存 %s" % os.path.basename(dst))
        with open(dst, "rb") as f:
            return f.read()

    print("[mkpkg] 面板来源：下载 %s" % url)
    os.makedirs(OUT, exist_ok=True)
    try:
        import urllib.request
        with urllib.request.urlopen(url, timeout=120) as r:
            blob = r.read()
        with open(dst, "wb") as f:
            f.write(blob)
        return blob
    except Exception as e:  # noqa: BLE001 - 下载失败必须早失败，不能产出空面板包
        sys.exit("[mkpkg] 面板 ipk 获取失败：%s\n"
                 "        可设 FZS_PANEL_IPK=/path/to/luci-app-fzs_x.y.z_all.ipk 指定本地文件" % e)


def collect_panel():
    """从面板 ipk 的 data.tar.gz 取材，沿用包内的权限位（上游已按 755/644 归类）。"""
    m = {}
    with tarfile.open(fileobj=io.BytesIO(gzip.decompress(fetch_panel_ipk()))) as outer:
        data = outer.extractfile("./data.tar.gz").read()
    with tarfile.open(fileobj=io.BytesIO(gzip.decompress(data))) as dt:
        for ti in dt.getmembers():
            if not ti.isfile():
                continue
            arc = ti.name if ti.name.startswith("./") else "./" + ti.name
            arc = arc.replace("//", "/")
            # 沿用包内权限位。兜底补 755：防止上游 ipk 里某个可执行角色失真
            # （历史教训：usr/bin/fzs-atio 曾是 0644，面板 AT 桥接直接起不来）。
            mode = ti.mode & 0o7777
            rel = ti.name.lstrip("./")
            if mode & 0o111 == 0 and (
                    rel.startswith("etc/init.d/") or
                    rel.startswith("usr/libexec/") or
                    rel.startswith("usr/bin/")):
                mode = 0o755
            m[arc] = (mode, dt.extractfile(ti).read())
    return m


def make_suite(engine_map, panel_map, out_path):
    members = {}          # arcname -> (mode, bytes or None(dir) or ('sym', target))
    for src in (engine_map, panel_map):
        for arc, (mode, content) in src.items():
            if arc in SUITE_EXCLUDE:
                continue
            members[arc] = (mode, content)
    n = len(members)

    for arc, target in RCLINKS.items():
        members[arc] = ("sym", target)
    print("  + %d 个 rc.d 开机链接" % len(RCLINKS))

    members["./usr/bin/fzs-ims-check"] = (0o755, CHECK_SH.encode())
    eng_list = "\n".join(
        "/" + a[2:] for a in sorted(members)
        if a.startswith("./") and a[2:].startswith(ENGINE_PREFIXES)
        and not a.startswith("./etc/rc.d/")
    )
    reg = (REGISTER_SH
           .replace("%ENGINE_VER%", "%s-%s" % (ENGINE_VER, ENGINE_REL))
           .replace("%PANEL_VER%", "%s-%s" % (PANEL_VER, PANEL_REL))
           .replace("%ENGINE_LIST%", eng_list))
    members["./usr/bin/fzs-ims-register-opkg"] = (0o755, reg.encode())
    print("  + fzs-ims-check / fzs-ims-register-opkg (%d 条 opkg 清单)" % len(eng_list.split("\n")))

    # 与 opkg 的 data.tar 保持一致：显式带上各级目录项。
    # 虽然 GNU tar 解包时会自动建父目录，但 busybox tar 在某些层叠目录上依赖目录项，
    # 缺了会报 "Cannot create directory"。
    dirs = set()
    for arc in list(members):
        d = os.path.dirname(arc).rstrip("/")
        while d and d not in (".", ""):
            dirs.add(d + "/")
            d = os.path.dirname(d).rstrip("/")
    for d in sorted(dirs):
        members.setdefault(d, (0o755, None))

    raw = io.BytesIO()
    with tarfile.open(fileobj=raw, mode="w", format=tarfile.GNU_FORMAT) as tf:
        for arc in sorted(members):
            mode, payload = members[arc]
            ti = tarfile.TarInfo(arc)
            ti.mtime = 0
            ti.uid = ti.gid = 0
            ti.uname = ti.gname = "root"
            if isinstance(mode, str):          # symlink
                ti.type = tarfile.SYMTYPE
                ti.linkname = payload
                ti.mode = 0o777
                tf.addfile(ti)
                continue
            ti.mode = mode
            if payload is None:                # directory
                ti.type = tarfile.DIRTYPE
                ti.size = 0
                tf.addfile(ti)
            else:
                ti.size = len(payload)
                tf.addfile(ti, io.BytesIO(payload))
    # 目录项：ipk 的 data 里有，恢复包里按需补建即可（tar -xzf 会自动建）
    blob = gzip.compress(raw.getvalue(), compresslevel=9)
    os.makedirs(os.path.dirname(out_path), exist_ok=True)
    with open(out_path, "wb") as f:
        f.write(blob)
    print("  %-42s %7d B  (%d 个成员)"
          % (os.path.basename(out_path), len(blob), len(members)))
    return blob, n


def main():
    print("[mkpkg] 产物输出目录: %s" % OUT)
    os.makedirs(OUT, exist_ok=True)

    engine_map = collect_engine()
    panel_map = collect_panel()
    print("[mkpkg] 取材：engine %d 文件 (src/board-files)，panel %d 文件 (fzs1-panel %s-%s ipk)"
          % (len(engine_map), len(panel_map), PANEL_VER, PANEL_REL))
    # ★ 闸门：0.3.2 起修的就是 OpenWrt 23.05 上 rpcd file.exec 全废的缺陷
    #   （file.c:812 的 `sid && env`）。模块缺席 ⇒ 面板所有页面取不到数据，
    #   而设备侧其实一切正常 —— 这种"看着像没起来"的故障最难查，故在此硬性拦下。
    if not any(arc.endswith("/fzsexec.js") for arc in panel_map):
        sys.exit("[mkpkg] 面板 ipk 缺少 www/luci-static/resources/fzsexec.js："
                 "版本过低（<%s-%s？），OpenWrt 23.05 上面板将全部失效" % (PANEL_VER, PANEL_REL))

    engine_ipk = os.path.join(OUT, "imsengine_%s-%s_%s.ipk"
                              % (ENGINE_VER, ENGINE_REL, ENGINE_ARCH))
    panel_ipk = os.path.join(OUT, "luci-app-fzs_%s-%s_%s.ipk"
                             % (PANEL_VER, PANEL_REL, PANEL_ARCH))
    suite = os.path.join(OUT, "fzs-ims-suite-%s.tar.gz" % SUITE_VER)

    print("[mkpkg] 打包 ipk：")
    make_ipk(engine_map,
             ENGINE_CONTROL.format(ver=ENGINE_VER, rel=ENGINE_REL, arch=ENGINE_ARCH),
             "/etc/config/ims\n", ENGINE_POSTINST, ENGINE_PRERM, engine_ipk)
    make_ipk(panel_map,
             PANEL_CONTROL.format(ver=PANEL_VER, rel=PANEL_REL, arch=PANEL_ARCH),
             "/etc/config/fzs-sms\n", PANEL_POSTINST, PANEL_PRERM, panel_ipk)

    print("[mkpkg] 合成恢复包：")
    make_suite(engine_map, panel_map, suite)

    lines = []
    for p in (engine_ipk, panel_ipk, suite):
        h = hashlib.sha256(open(p, "rb").read()).hexdigest()
        lines.append("%s  %s" % (h, os.path.basename(p)))
    with open(os.path.join(OUT, "sha256sums"), "w", newline="\n") as f:
        f.write("\n".join(lines) + "\n")
        f.write("# 固件本体的 sha256 见主 build/ 目录\n")
    print("[mkpkg] sha256sums:")
    for l in lines:
        print("  " + l)
    print("[mkpkg] 完成。")


if __name__ == "__main__":
    main()
