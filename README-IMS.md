# FZS S1 / MIBOX 668M2 固件（内置 IMS 短信通道）

本仓库 = 上游 [zzt718/fzs1-openwrt](https://github.com/zzt718/fzs1-openwrt) ＋ **内置 IMS 短信引擎**
＋ [zzt718/fzs1-panel](https://github.com/zzt718/fzs1-panel) 面板 **0.3.1（原生带 IMS 通道）**。

编译产出的是**真正的 sysupgrade 固件**（`.bin`），可以直接用 LuCI 的「刷写固件」装上，
装完 IMS 就随系统在一起 —— **恢复出厂设置也不会丢**，比"装 ipk / 恢复备份"两种路线都稳。

---

## 1. 为什么改用固件内置

| 路线 | 做法 | 问题 |
|---|---|---|
| ipk 安装 | 网页上传 `imsengine_*.ipk` + `luci-app-fzs_*.ipk` | 重置系统后要重装；面板常因厂商预装缺 `.list` 装不上 |
| 恢复备份 | 上传 `fzs-ims-suite-*.tar.gz` | 是 tar 解压、**不写 flash**；包较大时 LuCI 上传易 `XHR request timed out`；opkg 无记录 |
| **固件内置（本仓库）** | 刷 `.bin` | 一次性；IMS 进 squashfs，**恢复出厂仍保留**；sysupgrade 自动保留 `/etc/config` |

> ⚠️ 注意区分：LuCI「升级」页有两个上传框。
> **刷固件** 走上面「刷写固件」（本仓库的 `.bin`）；
> `fzs-ims-suite-*.tar.gz` 走下面「恢复备份」（那是另一条路线，别混用）。

---

## 2. 拿固件

- **Actions 产物**：仓库 → Actions → 最新的 `Build FZS-S1 IMS Firmware` → 底部 Artifacts
  → `mibox-668m2-sysupgrade`（保留 30 天）
- **Release**：打 tag（`git tag v0.1.7 && git push --tags`）后自动发 Release，附件即固件

文件名：`openwrt-ramips-mt76x8-mibox_668m2-squashfs-sysupgrade.bin`

---

## 3. 刷机

1. LuCI → **系统 → 升级**（`System → Flash Firmware`）
2. 「刷写固件」→ 上传 `.bin`
3. **务必勾选「保留配置」**（默认勾选）。`lib/upgrade/keep.d/imsengine` 已声明
   `/etc/config/ims`、`/etc/pcscf_learned.json` 等需要保留
4. 校验和出现后点「继续」→ 等待约 2~3 分钟，设备自动重启

等价命令行：

```sh
sysupgrade -v /tmp/openwrt-ramips-mt76x8-mibox_668m2-squashfs-sysupgrade.bin
```

### 刷完自检

```sh
fzs-ims-check            # 自检脚本：关键文件 / 补丁 / 服务
/etc/init.d/ims status   # 或 fzs-at ims status
logread | grep -E 'ims|bypass'
```

- 卡在 **EC200N（自带 IMS 栈）** → 状态应为 `原生 IMS 已注册（旁路）`，进程只有 `ims-data`
- 卡在 **EC200T（无 IMS 栈）** → 状态应为 `已注册`，进程是 `imsengine` + `ims-ppp`
  - 电信卡首次使用需学一次 P-CSCF：把卡换到 200N 上跑 `fzs-at ims learn`，再插回 200T

---

## 4. 相对上游加了什么

```
src/board-files/
  usr/bin/imsengine                     # IMS/SIP 引擎（Go 交叉编译, linux/mipsle softfloat）
  etc/init.d/ims                        # 服务脚本（含"自带 IMS 栈自动旁路" + 开机守望）
  etc/config/ims                        # 默认配置
  etc/operators/{46000,46001,46011}.json# 移动/联通/电信 IMS 参数
  usr/libexec/ims-data                  # 旁路模式上网守护（绑数据 CID + 修路由 + 修 DNS）
  usr/libexec/ims-ppp                   # EC200T 场景的 PPP 上网守护
  usr/lib/imsengine/...                 # patch-fzs + 注入片段（给非本面板的机器兜底）
  etc/uci-defaults/97-ims-engine        # ★ 固件内置不走 postinst，靠它 enable 开机自启
  lib/upgrade/keep.d/imsengine          # sysupgrade 时保留 IMS 配置与学到的 P-CSCF
src/luci-app-fzs.Makefile               # 内置面板的打包 Makefile（内容取自面板 ipk）
#  ★ 面板源码不在本仓！真源是独立的 fzs1-panel 项目，本仓只把它发布的
#    luci-app-fzs_x.y.z_all.ipk 拉来内置（build.sh/tools/mkpkg.py 自动下载）。
#    曾在此处放过一份 src/luci-app-fzs-files/ 源码副本且 inject 时优先，
#    造成面板源码两处分叉、版本对不上极难排查 —— 已移除，请勿恢复。
#    当前内置版本 0.3.2-1：修了 OpenWrt 23.05 上面板所有 AT 交互失效
#    （rpcd file.c:812 的 `sid && env` 让 file.exec 一律 Permission denied）。
#    开发期想塞自己的包：FZS_PANEL_IPK=/path/to/x.ipk bash build.sh inject
imsengine/                              # 引擎 Go 源码（可审计；默认用预编译二进制）
.github/workflows/build.yml             # GitHub Actions 自动编译
```

---

## 5. 自己编译

### GitHub Actions（推荐）

fork 本仓库 → Actions 里手动 `Run workflow`，或 push / 打 tag 自动触发。

- 仓库设为 **public**：GitHub 对公开仓库不限制 Actions 分钟数（私有仓库 2000 分钟/月，
  一次完整编译就可能用掉大半）
- 已启用 ccache 缓存，二次构建明显更快
- 首次编译约 1.5~3 小时（拉 OpenWrt 源码 + 编 toolchain + kernel + packages）

### 本地 Linux / WSL2

```sh
sudo apt-get install -y build-essential clang flex bison g++ gawk gcc-multilib \
  g++-multilib gettext git libncurses-dev libssl-dev python3-setuptools rsync \
  swig unzip zlib1g-dev file wget ccache
bash build.sh            # clone → inject → config → build
```

产物：`build/openwrt-ramips-mt76x8-mibox_668m2-squashfs-sysupgrade.bin`

可选：`FZS_BUILD_ENGINE=1 bash build.sh` 会用仓库内 Go 源码重新交叉编译引擎
（默认使用 `src/board-files/usr/bin/imsengine` 预编译二进制）。

---

## 6. 已知注意事项

- **IMAGE_SIZE 16 MB**：内置 IMS 后 squashfs 约 6.8 MB（原 5.3 MB），rootfs 上限 13.5 MB，
  空间充足；若以后继续加包，编译阶段会因超限报错，需精简包列表
- 引擎只占 `ttyUSB4`，原生 IMS 探测走 `ttyUSB5`/`ttyUSB2`，两者不冲突
- 开机时模组可能还没完成 IMS 注册 → 服务内置 **bypass-watch 守望**，
  发现"该旁路却进了引擎模式"会自动 restart 纠正（等 10 s 一轮，最多守 5 分钟）
- 换运营商后 `ims-data` 会自动把 DNS 统一成 `223.5.5.5 / 119.29.29.29`
  （否则用联通卡问电信 DNS 会解析超时，表现为"能 ping 通但网页打不开"）
