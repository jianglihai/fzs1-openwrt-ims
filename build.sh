#!/bin/bash
# ============================================================================
#  fzs1-openwrt —— 一键编译脚本（对外版）
#
#  用途：在 Linux / WSL2（Ubuntu 22.04+）里，从零拉取 OpenWrt 23.05 源码，
#        注入本项目的板级适配与内置面板，编译出可刷固件。
#
#  用法：
#     bash build.sh              # 全流程（clone + 注入 + defconfig + 编译）
#     bash build.sh clone        # 只拉源码
#     bash build.sh inject       # 只注入板级 + 面板
#     bash build.sh config       # 只生成 .config
#     bash build.sh build        # 只编译
#     bash build.sh clean        # 清理产物
#
#  产物：build/openwrt-ramips-mt76x8-mibox_668m2-squashfs-sysupgrade.bin
# ============================================================================
set -e

OPENWRT_VER="v23.05.5"
OPENWRT_REPO="https://github.com/openwrt/openwrt.git"
HERE="$(cd "$(dirname "$0")" && pwd)"
WORK="${FZS_WORK:-$HOME/fzs1-openwrt-build}"
SRC="$WORK/openwrt"
OUT="$HERE/build"
JOBS="$(nproc)"

# ---- 内置面板（luci-app-fzs）版本与来源 -------------------------------------
# 面板**不在这个仓库里改** —— 它的真源是独立的 fzs1-panel 项目，本仓只负责
# 把那个项目 Release 上的 ipk 拉来内置（README「从源码构建」也是这么说的）。
#
# ★ 为什么必须这样做：曾经在 src/luci-app-fzs-files/ 下放过一份面板源码副本，
#   inject 时副本优先 ⇒ 每次都想改面板的人都跑到这里改，结果
#   「真源改了、副本没同步」/「副本改了、真源不知道」，两棵树长期分叉，
#   排查问题时极易误判命中哪个版本。已删除该副本，**面板一律走 Release**。
#
# 可用环境变量覆盖：
#   FZS_PANEL_IPK=/path/to/luci-app-fzs_x.y.z_all.ipk   （本地文件，开发调试用）
#   FZS_PANEL_VER=0.3.2-1                               （指定版本）
#
# 0.3.2-1 起包含：修复 OpenWrt 23.05 上面板所有 AT 交互失效
#                （rpcd file.c:812 的 `sid && env` 无条件拒绝导致 file.exec 全废）
#                 + fzs-at 的 IMS 原生栈旁路能力。
PANEL_VER="${FZS_PANEL_VER:-0.3.2-1}"
PANEL_REPO="jianglihai/fzs1-panel"

# ★ 诊断信息必须走 stderr，不能走 stdout。
#   本脚本里有形如 `ipk="$(fetch_panel)"` 的命令替换：那会把**整个函数的 stdout**
#   一起捕获进去。若 log 打到 stdout，函数返回的"ipk 路径"就会被日志文本污染
#   （实测踩过：tar 报 `Cannot open: No such file or directory`，路径里混进了
#   带 ANSI 转义的日志），这种错还极难看懂 —— 所以这里统一重定向到 stderr。
log(){ echo -e "\n\033[1;36m>>> $*\033[0m" >&2; }
die(){ echo -e "\n\033[1;31m!! $*\033[0m" >&2; exit 1; }

# 获取面板 ipk（本地优先，否则从 Release 下载）
fetch_panel(){
    if [ -n "${FZS_PANEL_IPK:-}" ] && [ -f "$FZS_PANEL_IPK" ]; then
        echo "$FZS_PANEL_IPK"; return 0
    fi
    local url="https://github.com/${PANEL_REPO}/releases/download/v${PANEL_VER}/luci-app-fzs_${PANEL_VER}_all.ipk"
    local dst="$WORK/luci-app-fzs_${PANEL_VER}_all.ipk"
    mkdir -p "$WORK"
    if [ ! -f "$dst" ]; then
        log "下载面板 ipk：$url"
        wget -q -O "$dst" "$url" || curl -fL -o "$dst" "$url" || die "面板 ipk 下载失败（可设 FZS_PANEL_IPK 指定本地文件）"
    fi
    echo "$dst"
}

# ---------------------------------------------------------------------------
clone(){
    log "拉取 OpenWrt $OPENWRT_VER 源码 → $SRC"
    mkdir -p "$WORK"; cd "$WORK"
    if [ ! -d openwrt/.git ]; then
        git clone --depth 1 -b "$OPENWRT_VER" "$OPENWRT_REPO"
    else
        echo "源码已存在，跳过 clone"
    fi
    cd "$SRC"
    log "更新 feeds"
    ./scripts/feeds update -a
    ./scripts/feeds install -a

    # 第三方包：Argon 主题不在官方 luci feed 中（官方只有 bootstrap）。
    # 若不在此处 clone，CONFIG_PACKAGE_luci-theme-argon=y 会在 defconfig 时被静默丢弃，
    # 结果 uci 里指向 /luci-static/argon 而资源不存在，LuCI 只能回退 bootstrap-dark
    # —— 界面就是一副"毛坯"样子（2026-10-22 实测）。
    clone_third() {
        local name="$1" repo="$2" dest="$SRC/package/$1"
        if [ -d "$dest/.git" ]; then
            ( cd "$dest" && git pull --ff-only -q ) 2>/dev/null || true
        else
            rm -rf "$dest"
            git clone --depth 1 -q "$repo" "$dest" || echo "警告：$name 拉取失败（界面将退回 bootstrap）"
        fi
    }
    if [ "${FZS_NO_ARGON:-0}" != "1" ]; then
        log "拉取 Argon 主题（第三方）"
        clone_third luci-theme-argon     https://github.com/jerrykuku/luci-theme-argon.git
        clone_third luci-app-argon-config https://github.com/jerrykuku/luci-app-argon-config.git
        # ★ 摘掉 +wget-any 依赖（2026-10-22 实证踩坑）：
        #   Argon 的 Makefile 里 DEPENDS 含 +wget-any，但它是 OpenWrt **24.x 才引入**
        #   的虚拟包（见 openwrt/openwrt 提交 "uclient: provide virtual wget-any"），
        #   23.05 的 feeds 里没有任何包提供它。结果 defconfig 只打一句
        #     WARNING: Makefile 'package/luci-theme-argon/Makefile' has a
        #              dependency on 'wget-any', which does not exist
        #   然后【静默丢弃整个包】——编译日志里再没有一行 make -C package/luci-theme-argon，
        #   固件里自然没有 /www/luci-static/argon。
        #   我们已在 99-fzs-branding 关掉在线壁纸（online_wallpaper='none'），
        #   压根不需要 wget，直接把这个依赖摘掉即可。
        for mf in luci-theme-argon luci-app-argon-config; do
            f="$SRC/package/$mf/Makefile"
            if [ -f "$f" ] && grep -q 'wget-any' "$f"; then
                sed -i -E 's/[[:space:]]*\+wget-any//g' "$f"
                log "  已摘除 $mf 的 +wget-any 依赖"
            fi
        done
    fi
}

# ---------------------------------------------------------------------------
inject(){
    log "注入板级适配 + 内置面板"
    cd "$SRC"

    # 1) 设备树
    cp "$HERE/src/mt7628an_mibox_668m2.dts" target/linux/ramips/dts/

    # 2) 设备定义（追加到 mt76x8.mk，幂等）
    if ! grep -q "Device/mibox_668m2" target/linux/ramips/image/mt76x8.mk 2>/dev/null; then
        cat >> target/linux/ramips/image/mt76x8.mk <<'EOF'

define Device/mibox_668m2
  SOC := mt7628an
  DEVICE_VENDOR := MIBOX
  DEVICE_MODEL := 668M2 (FZS S1)
  DEVICE_PACKAGES := kmod-usb2 kmod-usb-ohci kmod-usb-net-rndis \
      kmod-usb-serial-option kmod-i2c-mt7628 i2c-tools \
      kmod-mt76 kmod-mt7603
  SUPPORTED_DEVICES += mibox,668m2
  # 16MiB flash：firmware 分区 @0x50000 尺寸 0xfb0000 = 16060KiB
  # 不得再放大（曾误抄 32MB 模板写成 16128k，超出分区 → 编译时 bin 过大
  # 且运行时 mtd 写入会顶坏相邻空间）
  IMAGE_SIZE := 16060k
endef
TARGET_DEVICES += mibox_668m2
EOF
    fi

    # 3) 板级文件（02_network / 01_leds 等由 board-files 覆盖）
    cp -a "$HERE/src/board-files/." target/linux/ramips/mt76x8/base-files/

    # 4) 内置面板包（luci-app-fzs）
    mkdir -p package/luci-app-fzs
    cp "$HERE/src/luci-app-fzs.Makefile" package/luci-app-fzs/Makefile
    #   面板内容：一律从面板项目 Release 下载 ipk 解包 —— 本仓不存面板源码副本。
    #   ★ 曾存在 src/luci-app-fzs-files/ 副本且 inject 时优先，导致面板源码两处分叉、
    #     版本对不上难排查；现已彻底改为只走 Release（开发期想塞自己的包，
    #     用 FZS_PANEL_IPK=xxx.ipk bash build.sh inject）。
    if [ -d "$HERE/src/luci-app-fzs-files" ]; then
        echo "面板内容：忽略 src/luci-app-fzs-files/（面板源码请以 fzs1-panel 为准，见 README）"
    fi
    local ipk; ipk="$(fetch_panel)"
    local n
    rm -rf /tmp/fzs_panel && mkdir -p /tmp/fzs_panel
    ( cd /tmp/fzs_panel && tar xzf "$ipk" && mkdir -p data && tar xzf data.tar.gz -C data ) \
        || die "面板 ipk 解包失败：$ipk"
    rm -rf package/luci-app-fzs/files
    mkdir -p package/luci-app-fzs/files
    cp -a /tmp/fzs_panel/data/. package/luci-app-fzs/files/
    n="$(find package/luci-app-fzs/files -type f | wc -l)"
    [ "$n" -gt 0 ] || die "面板 ipk 解包后没有文件：$ipk"
    echo "面板内容：来自 $(basename "$ipk")（$n 个文件）"
    # ★ 闸门：0.3.2-1 修的就是 file.exec 不可用，模块缺失等于整个面板取不到数据。
    #   宁可在这里失败，也不要刷进设备才发现"页面全是空白"。
    [ -f package/luci-app-fzs/files/www/luci-static/resources/fzsexec.js ] \
        || die "面板缺少 www/luci-static/resources/fzsexec.js —— 版本太低（<$PANEL_VER？），OpenWrt 23.05 上面板将全部失效"

    # 5) ★ 强制执行位 —— 本仓在 Windows 上维护，git 不记录可执行位
    #    （core.filemode=false），提交时脚本一律退化成 100644。
    #    后果（2026-10-22 事故）：CI 在 Linux 检出后 /etc/init.d/ims、
    #    /usr/bin/imsengine、/usr/libexec/ims-data 全都不可执行，
    #    刷进真机后 IMS 服务起不来、没有默认路由、上不了网。
    #    这里按目录统一补，不再依赖提交者记得 chmod。
    for d in \
        target/linux/ramips/mt76x8/base-files/etc/init.d \
        target/linux/ramips/mt76x8/base-files/etc/uci-defaults \
        target/linux/ramips/mt76x8/base-files/etc/chatscripts \
        target/linux/ramips/mt76x8/base-files/usr/bin \
        target/linux/ramips/mt76x8/base-files/usr/libexec \
        target/linux/ramips/mt76x8/base-files/usr/lib/imsengine \
        package/luci-app-fzs/files/etc/init.d \
        package/luci-app-fzs/files/usr/bin \
        package/luci-app-fzs/files/usr/libexec
    do
        [ -d "$d" ] && find "$d" -type f -exec chmod 755 {} + 2>/dev/null
    done
    echo "执行位已按目录统一补齐（防御 Windows 提交丢失 mode）"

    echo "注入完成。src/board-files → target/.../base-files；src/luci-app-fzs.Makefile → package/luci-app-fzs/"
}

# ---------------------------------------------------------------------------
# 用仓库内的 Go 源码交叉编译 imsengine（可选，默认用预编译二进制）
#   目标：ramips/mt76x8 = MIPS 24Kc little-endian，无硬件浮点
build_engine(){
    log "交叉编译 imsengine（linux/mipsle, softfloat）"
    command -v go >/dev/null 2>&1 || die "未安装 Go，无法编译引擎（或不设 FZS_BUILD_ENGINE）"
    local out="$HERE/src/board-files/usr/bin/imsengine"
    ( cd "$HERE/imsengine" && GOOS=linux GOARCH=mipsle GOMIPS=softfloat CGO_ENABLED=0 \
        go build -trimpath -ldflags "-s -w" -o "$out" . ) || die "imsengine 编译失败"
    log "引擎已重建 → $out"
}

config(){
    log "生成 .config（基于 src/config.seed.v2）"
    cd "$SRC"
    cp "$HERE/src/config.seed.v2" .config
    # CI 提速 / 定制：FZS_EXTRA_CONFIG 可追加 .config 选项（可多行）
    if [ -n "${FZS_EXTRA_CONFIG:-}" ]; then
        printf '%s\n' "$FZS_EXTRA_CONFIG" >> .config
        log "追加 .config 选项: $FZS_EXTRA_CONFIG"
    fi
    # 引擎：默认用仓库内预编译二进制；设 FZS_BUILD_ENGINE=1 则就地用 Go 重新编译
    if [ "${FZS_BUILD_ENGINE:-0}" = "1" ]; then
        build_engine
    fi
    make defconfig
}

# ---------------------------------------------------------------------------
build(){
    log "编译（首次会下载大量源码，耗时较长）"
    cd "$SRC"
    make -j"$JOBS" download
    make -j"$JOBS"
    mkdir -p "$OUT"
    log "收集产物 → $OUT"
    cp bin/targets/ramips/mt76x8/openwrt-ramips-mt76x8-mibox_668m2-*.bin "$OUT/" 2>/dev/null || true
    cp bin/targets/ramips/mt76x8/sha256sums "$OUT/sha256sums" 2>/dev/null || true
    ls -la "$OUT"

    # ---- 闸门：确认关键包真的进了镜像，而不是被 defconfig 静默丢弃 ----
    # 教训：CONFIG_PACKAGE_xxx=y 写在 .config 里【不代表】它会编进固件。
    #   包不在任何 feed（Argon 第一版）、或 DEPENDS 指向不存在的包（wget-any），
    #   make 都只是打一行 WARNING 然后跳过，产物照出、编译照 success。
    # 所以必须显式核一次 rootfs 与 ipk，否则永远发现不了"界面没主题"这种问题。
    if [ "${FZS_NO_ARGON:-0}" != "1" ]; then
        argon_root="$(ls -d "$SRC"/build_dir/target-*/root-ramips/www/luci-static/argon 2>/dev/null | head -1)"
        argon_ipk="$(ls "$SRC"/bin/packages/*/luci/luci-theme-argon_*.ipk 2>/dev/null | head -1)"
        echo "================ 关键包入镜像核实 ================"
        echo "  rootfs 目录 : ${argon_root:-（未找到）}"
        echo "  生成的 ipk  : ${argon_ipk:-（未找到）}"
        if [ -n "$argon_root" ] || [ -n "$argon_ipk" ]; then
            echo "  >>> PASS：Argon 已编入固件"
            echo "=================================================="
        else
            echo "::error::Argon 未编入固件（rootfs/ipk 都没有）。99-fzs-branding 会自动退回 bootstrap，界面可用但不是预期观感"
            echo "  >>> FAIL：Argon 未编入固件"
            echo "=================================================="
        fi
    fi
}

# ---------------------------------------------------------------------------
clean(){
    log "清理"
    rm -rf "$OUT"
    [ -d "$SRC" ] && ( cd "$SRC" && make clean ) || true
}

case "${1:-all}" in
    clone)  clone ;;
    inject) inject ;;
    config) config ;;
    build)  build ;;
    clean)  clean ;;
    all)    clone; inject; config; build ;;
    *) echo "用法: $0 {clone|inject|config|build|clean|all}"; exit 1 ;;
esac
