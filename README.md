# SeeBarSideCar

一套键鼠控制一台 Windows 和一台 Mac，局域网直连。

- 鼠标移过屏幕边缘切到另一台
- 剪贴板同步：文本、图片、文件
- Cmd / Ctrl 自动互换
- 窗口可以拖到另一台的屏幕上继续用
- ScrollLock 或 Ctrl+Alt+Shift+Esc：立即把键鼠还给本机

## 要求

- Windows 10 / 11 x64
- macOS 14+，Apple Silicon
- 同一局域网，UDP 24800 互通
- 两台机器版本一致

## 编译

需要 Go 1.26：

```bash
./build.sh
```

产物在 `dist/`：Windows 用 `magic-windows-amd64.exe`，Mac 用 `Magic.app`。

## 配置

两台各一份，`secret` 相同，`edge` 相反。

Mac（`~/magic.json`，Windows 在它右边）：

```json
{
  "name": "mac",
  "peer": "192.168.1.20:24800",
  "secret": "至少12个字符的随机密码",
  "edge": "right"
}
```

Windows（`config.json`）：

```json
{
  "name": "pc",
  "peer": "192.168.1.10:24800",
  "secret": "至少12个字符的随机密码",
  "edge": "left"
}
```

可选项：

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `swap_cmd_ctrl` | `true` | Cmd / Ctrl 互换 |
| `clipboard` | `true` | 剪贴板同步 |
| `file_clip_max_mb` | `2048` | 文件复制大小上限，`-1` 关闭 |
| `virtual_display` | `true` | 允许窗口拖到对方屏幕 |
| `switch_delay_ms` | `120` | 在边缘停多久才切换 |
| `mouse_scale` | `1.0` | 控制对方时的鼠标速度 |

## 运行

### Windows

```bat
:: 放行防火墙（管理员，一次）
netsh advfirewall firewall add rule name="magic" dir=in action=allow protocol=UDP localport=24800 profile=private

:: 要拖窗口过去需要这个驱动（一次）
winget install -e --id Parsec.ParsecVDD

magic-windows-amd64.exe -config config.json
```

### Mac

```bash
codesign --force --deep -s - /Applications/Magic.app
open /Applications/Magic.app --args -config ~/magic.json
```

在「系统设置 → 隐私与安全性」给 Magic 开：辅助功能、输入监控、屏幕录制。授权后重启程序。

状态页：<http://127.0.0.1:24801>

## 常见问题

- **连不上**：检查 `secret`、`peer`、防火墙；日志出现 `incompatible version` 就两边同时更新。
- **Mac 键盘没反应**：缺输入监控或辅助功能权限。
- **窗口拖不过去**：Windows 没装 Parsec 驱动。
- **鼠标偶尔卡**：多半是 Wi‑Fi，插一根网线最有效。

## 限制

- 锁屏、UAC 弹窗控制不了
- 视频、游戏等硬件加速画面拖过去可能是黑的
- 只支持局域网

## 开发

```bash
go test -race ./...
```

改了通信协议就把 `internal/proto` 里的 `Version` 加一。
