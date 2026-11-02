# 部署

- [本机运行](#本机运行)
- [局域网共享](#局域网共享)
- [Docker](#docker)
- [出口代理](#出口代理)
- [数据目录](#数据目录)
- [升级](#升级)

## 本机运行

只需要 Python 3.9+，不用 `pip install` 任何依赖。

| 脚本 | 平台 | 行为 |
|---|---|---|
| `start-qoder-proxy.bat [端口]` | Windows | 后台分离运行。可以关掉启动窗口，日志写入 `qoder-proxy.log` / `qoder-proxy.err.log` |
| `stop-qoder-proxy.bat` | Windows | 按 `qoder-proxy.pid` 停止。只停命令行里含 `qoder_proxy.py` 的那个进程，不会误杀其他 Python |
| `./start-qoder-proxy.sh [端口]` | Linux / macOS | 前台运行，终端就是进程的生命周期，`Ctrl+C` 或关闭终端即停止；同时写 `qoder-proxy.pid` |
| `./stop-qoder-proxy.sh [端口]` | Linux / macOS | 按 `qoder-proxy.pid` 停止（先 TERM，10s 不退再 KILL）。只停命令行里含 `qoder_proxy.py` 的那个进程；pid 文件缺失时按「进程名 + --port」兜底，不会误杀编辑器或另一份检出 |

默认端口是 **8790**，要改就在后面跟端口号，例如 `start-qoder-proxy.bat 8791`。Linux / macOS 首次使用前先执行 `chmod +x *.sh`。

脚本会自动寻找 Python，查找顺序如下：

1. 脚本目录下的便携运行时 `python/`
2. PATH 上的 `python3` / `python`；Windows 上还会找 `py` 启动器
3. Codex 自带的运行时

Linux / macOS 可以用 `QD_PROXY_PYTHON=/usr/bin/python3.12 ./start-qoder-proxy.sh` 指定解释器。

也可以直接运行：

```bash
python qoder_proxy.py --port 8790
```

## 局域网共享

| 平台 | 命令 |
|---|---|
| Windows | `start-qoder-proxy-lan.bat [端口] [API Key]` |
| Linux / macOS | `./start-qoder-proxy-lan.sh [端口] [API Key]` |

两个脚本都在**前台**运行，关闭窗口即停止。

- 监听所有网卡，Base URL 是 `http://<本机局域网 IP>:8790/v1`
- 局域网模式必须使用 API Key。不指定时，首次启动会生成一个 `qd-` 开头的随机 Key，保存到 `accounts/settings.json` 并打印在终端，之后重启继续使用同一个
- 其他设备可以通过 `http://<IP>:8790/?key=<API Key>` 直接打开看板
- 如果面板还在用默认密码，启动时会打印警告，看板也会拒绝显示明文 Key。请先修改面板密码

其他设备连不上时，放行防火墙：

| 平台 | 命令 |
|---|---|
| Windows | 运行 `allow-firewall.bat [端口]`（会弹出 UAC 请求管理员权限） |
| Linux / macOS | `sudo ./allow-firewall.sh [端口]`（自动识别 ufw / firewalld / nftables / iptables） |
| WSL2 | 在 WSL 内运行 `allow-firewall.sh`，它会打印 Windows 侧还需要执行的防火墙规则和 `netsh portproxy` 命令 |

## Docker

镜像发布在 GHCR，支持 `linux/amd64` 和 `linux/arm64`。

```bash
# 使用预构建镜像（docker-compose.yml 的默认配置）
docker compose pull
docker compose up -d
docker compose logs -f
```

从源码构建：把 `docker-compose.yml` 里的 `image:` 注释掉，改用 `build: .`，然后执行 `docker compose up -d --build`。

不用 compose 的话：

```bash
docker run -d --name qoder-proxy --restart unless-stopped \
  -p 8790:8790 \
  -v "$(pwd)/accounts:/app/accounts" \
  -v "$(pwd)/usage:/app/usage" \
  ghcr.io/shuishuipingan/qoder2api-hub:latest
```

**镜像 tag**（[包页面](https://github.com/shuishuipingan/qoder2api-hub/pkgs/container/qoder2api-hub)）：

| Tag | 含义 |
|---|---|
| `latest` | 最新发布版 |
| `1.5` | 1.5.x 系列的最新版 |
| `1.5.11` | 固定版本 |
| `sha-<短提交>` | 固定到某次构建 |

**容器配置：**

- 容器以 `--lan` 启动，会强制使用 API Key：要么通过 `API_KEY` 环境变量传入，要么在首次启动时自动生成，用 `docker compose logs qoder-proxy | grep -i "api key"` 查看
- 环境变量：`HOST`（默认 `0.0.0.0`）、`PORT`（默认 `8790`）、`API_KEY`、`TZ`（默认 `Asia/Shanghai`）。改 `PORT` 时要同步修改 `ports:` 映射
- 需要持久化的两个目录：`/app/accounts` 和 `/app/usage`。NAS 或长期部署建议挂载绝对路径
- 镜像自带健康检查，探测的是 `/ping`

### 容器启动自检

有几类问题会让网关「莫名失效」，在容器里尤其常见。网关每次启动都会检查，发现问题就在日志里打印 WARN，结果也可以在 `/health` 的 `env` 字段中查看：

| 检查 | 为什么重要 |
|---|---|
| 与上游的时钟偏差超过 120 秒 | COSY 签名带时间戳，时钟偏差太大会让所有请求直接 401。同步宿主机时间即可 |
| `accounts/` 不可写 | 设备身份无法落盘，每次重建容器都会换一台「设备」 |
| 容器里启用了国际版账号（只在容器中检查） | 数据中心出口访问国际版网关容易被风控，表现为聊天一直排队。建议改用住宅出口，或改用国内版，见[风控处置手册](risk-control-playbook.md) |

### 容器里的设备身份

推理请求使用按账号 UID 派生的设备身份，跟容器无关。**只有签到 / 活动链路**需要官方 UMID 组件给出的真实机器身份：镜像构建时已经从官方 npm 包里提取了这个组件。

这个身份是「容器级」的，容器一重建就会变。为此网关会把首次拿到的身份缓存到 `accounts/machine_identity.json`，之后一直复用。所以：

- 只要 `accounts/` 是挂载卷，重启、重建、升级容器都不会更换设备身份
- 不要在多台机器之间共享同一个 `accounts/`，这个文件代表的是一台物理设备

相关开关：

| 变量 | 作用 |
|---|---|
| `QD_MACHINE_IDENTITY_CACHE=off` | 关闭落盘缓存 |
| `QD_MACHINE_IDENTITY_RESET=1` | 启动时清空缓存并换一个新身份（直接删除缓存文件效果相同） |
| `QD_MACHINE_IDENTITY_CACHE_TTL=<秒>` | 定期轮换身份，默认 `0` 表示永不过期 |

如果你不挂载 `accounts/`，又希望重启后身份不变，可以在 compose 里固定网卡 MAC（`mac_address:`，请自己生成一个，不要照抄示例），或者改用 `network_mode: host`。这两种做法都只能保证「重启不变」，重建容器还是会换身份。

## 出口代理

网关访问上游时，**只读取环境变量里的代理**：`HTTPS_PROXY` / `HTTP_PROXY` / `ALL_PROXY`（大小写均可）。没有设置时直接连接。每个请求都会重新读取，所以开关代理不需要重启网关。

- **Windows 不读取系统代理**（即注册表 / WinINET 里的设置）。如果你的机器必须通过系统代理才能上网，需要显式设置一次，然后重启网关：
  ```bat
  setx HTTPS_PROXY http://127.0.0.1:7897
  ```
- TUN / fake-ip 模式的透明代理（例如 Clash）无需任何设置，网关能识别 fake-ip 网段
- Docker 用 `-e HTTPS_PROXY=...` 传入

## 数据目录

| 路径 | 内容 |
|---|---|
| `accounts/*.json` | 每个账号一个文件，**Token 以明文保存**，不要外传或同步到网盘 |
| `accounts/settings.json` | 面板密码摘要、API Key、模型闸门、限额配置 |
| `accounts/machine_identity.json` | 缓存的机器身份 |
| `accounts/models-cache-<区域>.json` | 上次成功获取的官方模型清单，7 天内有效。上游暂时不可用时用它兜底 |
| `accounts/agent-backups/` | 智能体一键配置写入前的原文件备份 |
| `accounts/scheduler/state.json` | 调度器状态 |
| `usage/usage.jsonl` | 请求流水 |
| `usage/usage-summary.json` | 用量汇总 |

两个目录的位置可以分别用 `--accounts-dir`（或 `ACCOUNTS_DIR`）和 `--usage-dir`（或 `QD_PROXY_USAGE_DIR`）修改。

## 升级

| 部署方式 | 升级步骤 |
|---|---|
| **一键更新（源码部署）** | 看板「运行信息」→ 有新版本时点 **一键更新**，见下 |
| 源码（手动） | `git pull`，然后重启网关：Windows 先 `stop-qoder-proxy.bat` 再 `start-qoder-proxy.bat`；
Linux / macOS 先 `./stop-qoder-proxy.sh` 再 `./start-qoder-proxy.sh`（常驻/launchd 场景直接 restart 也可） |
| Docker | `docker compose pull && docker compose up -d` |

各版本的变更见[更新日志](../CHANGELOG.md)。

### 一键更新（看板按钮）

检测到新版本后，「运行信息」卡片会出现 **一键更新** 按钮。点击后网关自己完成：

1. 从 **GitHub 官方 release** 下载源码 tarball（只认这一个来源）；
2. 解压到临时目录，**逐个 `.py` 编译校验**，并核对 `VERSION` 必须等于目标 tag；
3. 把本次会被覆盖的文件**先备份**到 `backups/update-<旧版本>-<时间戳>/`；
4. 覆盖代码文件后**自动重启**（按原启动参数拉起新进程）；
5. 页面轮询 `/health`，版本变化后自动刷新。

**不会被覆盖的**：`accounts/`（账号与设置）、`python/`（内置运行时）、`backups/`、`.git/`、日志与 PID 文件。

任一步失败都会**从备份回滚**并保留旧版本运行；失败原因会直接显示在卡片上。

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `QD_UPDATE_DISABLE` | 关 | 设为 `1` 完全禁用更新端点（只保留「检查新版本」） |
| `QD_UPDATE_ALLOW_REMOTE` | 关 | 更新端点默认**只允许本机**访问；设为 `1` 才允许远程触发（面板会话仍然是必需的） |
| `QD_UPDATE_ALLOW_CONTAINER` | 关 | 容器里默认拒绝自更新（镜像重建会覆盖），改为在宿主机 `docker compose pull` |

> 容器部署请用 `docker compose pull`：镜像里的代码由镜像决定，容器内自更新会被下次重建覆盖。
