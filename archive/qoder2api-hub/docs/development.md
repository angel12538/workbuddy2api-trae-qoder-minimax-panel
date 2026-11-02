# 开发与测试

- [模块结构](#模块结构)
- [测试](#测试)
- [维护脚本](#维护脚本)
- [发版](#发版)

## 模块结构

| 文件 | 职责 |
|---|---|
| `qoder_proxy.py` | 主程序：HTTP 路由、上游请求、协议转换、用量统计、看板鉴权 |
| `qoder_anthropic.py` | Anthropic Messages 与 Chat 请求之间的双向转换 |
| `qoder_sign.py` | COSY 签名、自定义 Base64；AES / GCM / RSA / DPAPI / QMC 的纯 Python 实现 |
| `qoder_accounts.py` | 双区账号池、OAuth 设备授权、PAT、Token 生命周期、本机凭证扫描、机器身份 |
| `qoder_catalog.py` | 加载官方模型快照，维护别名表和区域独占表 |
| `qoder_catalog_cn.json` / `qoder_catalog_intl.json` | 两个区域的官方模型快照 |
| `qoder_tasks.py` | 签到、活动领取、Pro 福利包、批量执行、Token 保活 |
| `qoder_scheduler.py` | 整点调度器 |
| `qoder_settings.py` | 面板密码、API Key、模型闸门、限额护栏、面板会话 |
| `qoder_agents.py` | 智能体一键配置；也可以作为独立的 CLI 运行 |
| `qoder_fingerprint.py` | 按 UID 派生设备指纹（`derive_id`） |
| `baseprompt.json` | 官方推理请求体模板 |
| `dashboard.html` | 单文件 Web 看板 |

## 测试

测试全部使用标准库，前端测试需要 Node.js（没有 Node.js 时自动跳过）。

```bash
python tests/run_all.py                          # 全部套件，并发执行
python tests/run_all.py guards                   # 只跑名字中含 guards 的套件
python tests/run_all.py --jobs 4 --logs suite-logs   # 与 CI 相同的命令
python scripts/check_clean_checkout.py           # 确认测试没有在仓库里留下文件
```

- `python _test_qoder.py` 是旧的单文件测试入口，仍然可以使用；`tests/_test_legacy.py` 也会把它纳入 `run_all.py` 一起执行
- `python _test_leak_guard.py` 把真实出现过的泄漏样本送进完整的流式管线，在发布镜像里也能直接运行
- 有 3 条密码学 KAT 依赖官方 fixture；缺少 fixture 时会显式打印 `[SKIP]`，不算失败。用 `QD_TEST_FIXTURE_DIR` 指定 fixture 目录后可以完整执行

CI（`.github/workflows/tests.yml`）在 Ubuntu Python 3.9、Ubuntu Python 3.12、Windows Python 3.12 上运行全部套件，并检查测试结束后仓库是否干净。

测试的写法约定（桩的保真度、迁移纪律、基线数字等）见 [tests/README.md](../tests/README.md)。

## 维护脚本

| 命令 | 用途 |
|---|---|
| `python _refresh_catalog.py` | 官方客户端更新后，从本机客户端的模型目录缓存重新导出两个区域的快照，并打印价格、上下文、思考档位的变化。加 `--dry-run` 只看差异，不写文件 |
| `python _verify_models.py --base http://127.0.0.1:8790` | 网关运行时，逐个对比模型清单和官方数据 |
| `python _install_umid.py` | 从官方 npm 包 `@qoder-ai/qodercli` 中提取当前平台的 UMID 组件，写到 `umid/runtime-info`（Windows 为 `runtime-info.exe` + `sgsdk.dll`）。可用 `--platform` / `--arch` / `--dest` 指定 |
| `python _diag_gateway.py` / `python _diag_campaign.py` | 诊断脚本，见[故障排查](troubleshooting.md#诊断脚本) |

## 发版

1. 更新 `qoder_proxy.py` 中的 `VERSION`，并在 [CHANGELOG.md](../CHANGELOG.md) 中添加这个版本的说明
2. 打 `v<VERSION>` tag 并推送。CI 会检查 tag 和 `VERSION` 是否一致
3. `docker-publish.yml` 构建 amd64 / arm64 镜像并推送到 GHCR，tag 为 `latest`、`<major>.<minor>`、`<version>`、`sha-<短提交>`。推送前会做冒烟测试：检查所有模块都已打进镜像、能正常导入、容器启动后 `/ping` 有响应

新增 `qoder_*.py` 模块时，记得把它加进 `Dockerfile` 的 `COPY` 清单。冒烟测试会根据源码中的 `import` 检查模块是否缺失。
