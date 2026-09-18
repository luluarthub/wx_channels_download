# localfix7 合并说明

版本：`260919-localfix7`。本次以用户指定仓库 `luluarthub/wx_channels_download` 的 `124f044235bc706fe796c043a14ff442a150ae5d` 为基线，迁移 `260916-localfix5` 的本地修复。旧源码保留；原安装目录在备份后切换新版，并保留用户配置和数据库。

## 合并内容

- 保留线上新版界面、多平台适配、直播、更新与 MCP 功能。
- 视频号下载以点击时的卡片身份定位，遇到身份不明确时停止，避免滚动后下载旧视频。
- 原画保留原始签名 URL；指定清晰度仅替换对应参数。
- 默认保留本地并发设置：1 个任务、每资源 2 个分段、每分段 4 MiB/s；保留线上独立资源并发配置。
- 暂停、删除等待下载工作退出，超时返回错误；工作尚未退出时保留文件和记录。
- 异常退出后将中断任务恢复为暂停，保留已下载分段；追加写入应用日志。
- Windows 代理守护使用父进程句柄和实例归属标识，避免旧实例清除新实例代理；附带停止脚本。
- 修复 Windows 注册表显示代理开启、实际连接配置仍为直连时，微信没有下载按钮的问题；通过 WinINet 连接配置 API 同步代理状态，并保留事务回滚和实例归属保护。
- 数据库实例锁阻止新版本重复启动并修改活动任务；端口检查保护旧版的常规重复启动。
- 修复 URL 下载缺少资源 ID 的问题，并在历史 URL 任务启动、恢复、重试时补齐。
- 修复 MCP HTTP 传输漏挂知乎与 Worker 后端的问题。
- 统一构建入口至 Go 1.24.1。

## 构建与验证

在仓库根目录运行（需 Go 1.24.1、Node.js；首次构建需下载 Go 依赖）：

```powershell
.\build\build-localfix.ps1
go test -tags with_gvisor,embed_inject,sqlite_only,embed_frontend_inject ./...
node --test pkg/scraper/wxchannels/channels.download.test.cjs
go run ./tools/runtimeverify -exe dist/localfix7/wx_video_download.exe -output F:/workspace/localfix7-runtime-check
```

运行验收工具的输出目录必须尚不存在。工具创建自己的数据库、端口和 HTTP 测试文件，配置 `proxy.system=false`，不替换安装版或使用用户下载目录。验证真实程序启动、UI 静态文件、代理转发、文件哈希、暂停恢复、删除、失败重试、强杀恢复、重复实例保护、日志追加和 SQLite 完整性。Python 版等价脚本为 `tools/verify_runtime.py`。

本地环境若通过受限沙盒运行测试，请把 `TEMP`、`TMP` 指向可访问的工作区临时目录；必要时对当前仓库使用命令级 Git `safe.directory`。无需修改全局 Git 配置。

## 安装切换与回退

产物包含可执行文件、默认配置、LICENSE、停止脚本和 SHA-256 清单。旧安装目录仍为：

`C:\Users\LCQ\AppData\Local\LuluTools\wx_channels_download`

切换前应完整退出旧主进程及旧 guardian，并保留原程序、原配置和数据库的一致性备份。不要覆盖用户的配置、cookies 或下载目录；数据库应在停服后备份，或使用 SQLite 在线备份接口，不能在有 WAL 写入时只复制主文件。首次上线保留旧配置的路径和功能开关，并核对 API 与代理端口。

若需回退，先停止新实例，再恢复备份的程序与数据库；不要将已迁移的数据库直接交给旧版使用。FFmpeg/FFprobe 沿用本机安装，供 MP3、直播和相关后处理调用。

真实微信登录态下的滚动选片、原画下载、解密播放，以及外部平台账号、直播和云端部署仍需对应环境验收；本地自动测试通过不等于这些外部流程已实测。
