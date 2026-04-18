# Resin 本地运行与测试指南

这份文档面向第一次接触 Go 项目的同学，重点说明：

- 当前项目在本地怎么启动
- 启动后怎么验证 HTTP / SOCKS5 是否可用
- 怎么验证粘性代理是否正常
- 常用的开发、构建、测试命令有哪些
- 常见报错该怎么处理

本文档以 **Windows + PowerShell** 为主，因为这是你当前实际使用的环境。

---

## 1. 这是什么项目

Resin 是一个代理池网关。

它当前支持：

- HTTP 正向代理
- SOCKS5 正向代理
- URL 形式的反向代理
- 粘性代理（同一个业务账号尽量绑定同一个出口 IP）

你这次本地已经实际验证过：

- HTTP 可用
- SOCKS5 可用
- HTTP 和 SOCKS5 共用同一个 `RESIN_PORT`
- SOCKS5 粘性账号可用

---

## 2. 本地运行前需要准备什么

建议你本地至少准备以下环境：

- Go 1.25+
- Node.js
- npm
- PowerShell

你可以用下面命令检查：

```powershell
go version
node --version
npm --version
```

如果 `go` 提示找不到：

```powershell
$env:Path += ";C:\Program Files\Go\bin"
go version
```

如果这样能用，说明是当前终端没有刷新 PATH。通常重新打开 PowerShell 即可。

---

## 3. 项目目录里哪些东西最重要

在项目根目录 `E:\script\Resin` 里，最常用的是这些：

- `cmd/resin`
  Resin 主程序入口
- `webui`
  前端管理界面源码
- `README.zh-CN.md`
  项目主文档
- `docker-compose.yml.example`
  Docker 启动示例
- `.env.example`
  最基础的环境变量示例

---

## 4. 推荐的本地运行方式

### 4.1 为什么推荐这种方式

推荐用源码直接运行：

- 最适合本地开发和调试
- 改完代码后可以马上重新启动验证
- 你已经用这种方式成功跑通了

### 4.2 第一次运行前，先构建前端

因为后端会嵌入 `webui/dist`，如果这个目录不存在，`go run -tags "with_quic with_wireguard with_grpc with_utls" ./cmd/resin` 也会失败。

在项目根目录执行：

```powershell
cd "E:\script\Resin"
```

然后构建前端：

```powershell
cd "E:\script\Resin\webui"
npm ci
npm run build
cd "E:\script\Resin"
```

构建成功后，项目里会出现：

```text
webui/dist
```

---

## 5. 本地启动 Resin

### 5.1 最推荐的启动步骤

先进入项目目录：

```powershell
cd "E:\script\Resin"
```

然后执行下面这整段命令：

> 这里推荐直接带上 `with_quic with_wireguard with_grpc with_utls`。这样本地运行时可以兼容更多常见节点，尤其是 `VLESS + REALITY` 这类依赖 `uTLS` 的节点；否则你可能会看到类似 `rebuild with -tags with_utls` 的错误。

```powershell
New-Item -ItemType Directory -Force -Path ".tmp\cache",".tmp\state",".tmp\log" | Out-Null
$env:RESIN_AUTH_VERSION="V1"
$env:RESIN_ADMIN_TOKEN="admin-123"
$env:RESIN_PROXY_TOKEN="my-token"
$env:RESIN_LISTEN_ADDRESS="127.0.0.1"
$env:RESIN_PORT="2260"
$env:RESIN_SOCKS5_TIMEOUT="3s"
$env:RESIN_CACHE_DIR=(Resolve-Path ".tmp\cache").Path
$env:RESIN_STATE_DIR=(Resolve-Path ".tmp\state").Path
$env:RESIN_LOG_DIR=(Resolve-Path ".tmp\log").Path
go run -tags "with_quic with_wireguard with_grpc with_utls" ./cmd/resin
```

### 5.2 这些命令是什么意思

- `New-Item ... .tmp\cache .tmp\state .tmp\log`
  在项目目录下创建临时目录，避免写系统目录时遇到权限问题
- `$env:RESIN_AUTH_VERSION="V1"`
  使用新认证格式
- `$env:RESIN_ADMIN_TOKEN="admin-123"`
  后台登录密码
- `$env:RESIN_PROXY_TOKEN="my-token"`
  代理认证密码
- `$env:RESIN_LISTEN_ADDRESS="127.0.0.1"`
  只监听本机
- `$env:RESIN_PORT="2260"`
  服务端口
- `$env:RESIN_SOCKS5_TIMEOUT="3s"`
  SOCKS 握手超时
- `go run -tags "with_quic with_wireguard with_grpc with_utls" ./cmd/resin`
  带常用可选能力运行项目主程序，避免 `uTLS` / `REALITY` 之类节点因为编译标签缺失而启动失败

### 5.3 这些环境变量是不是永久的

不是。

上面这些 `$env:XXX=...` 只对 **当前 PowerShell 窗口** 生效：

- 关闭这个窗口后就没了
- 不会修改你的系统环境变量
- 不会永久影响电脑

### 5.4 启动成功时会看到什么

正常启动后，日志里通常会出现类似信息：

```text
Resin server starting on http://127.0.0.1:2260 (HTTP + SOCKS5)
```

这表示：

- Resin 已经启动
- 当前端口是 `2260`
- HTTP 和 SOCKS5 都已经挂在这个端口上
- SOCKS4 默认关闭；仅当 `RESIN_ALLOW_INSECURE_SOCKS4=true` 且 `RESIN_PROXY_TOKEN=""` 时才允许使用

### 5.5 启动后这个窗口要不要关

不要关。

这个窗口现在就是 Resin 进程本体。  
你应该保持它开着，然后再开一个新的 PowerShell 窗口做测试。

---

## 6. 登录后台并导入节点

启动后，在浏览器打开：

```text
http://127.0.0.1:2260
```

后台登录信息：

- Admin Token：`admin-123`

登录后：

1. 进入订阅管理
2. 添加你的可用订阅
3. 等待节点刷新

如果你不导入任何节点，HTTP / SOCKS5 虽然能握手，但请求会返回：

- `503`
- `NO_AVAILABLE_NODES`

这不代表代理入口坏了，只代表 **当前没有可用出口节点**。

---

## 7. 怎么验证 HTTP 代理

### 7.1 打开第二个 PowerShell 窗口

不要关闭 Resin 正在运行的窗口。  
新开一个 PowerShell，再执行：

```powershell
curl.exe -x http://127.0.0.1:2260 -U ":my-token" https://api.ipify.org
```

### 7.2 成功时的表现

如果返回一个 IP，例如：

```text
38.xxx.xxx.200
```

说明：

- HTTP 代理已经可用
- 认证已经可用
- Resin 已经能拿到可用节点并出网

---

## 8. 怎么验证 SOCKS5 代理

在第二个 PowerShell 窗口执行：

```powershell
curl.exe -x socks5h://127.0.0.1:2260 --proxy-user ":my-token" https://api.ipify.org
```

如果返回一个 IP，例如：

```text
50.xxx.xxx.26
```

说明：

- SOCKS5 代理已经可用
- 说明 HTTP 和 SOCKS5 确实共用了同一个 `2260` 端口

### 8.1 为什么两次 SOCKS5 返回的 IP 可能不一样

因为下面这种命令：

```powershell
curl.exe -x socks5h://127.0.0.1:2260 --proxy-user ":my-token" https://api.ipify.org
```

只带了 token，没有带业务账号，所以它属于：

- 非粘性请求

非粘性请求可以被 Resin 分配到不同节点，因此返回不同 IP 是正常的。

---

## 9. 怎么验证 SOCKS5 粘性代理

你已经实际测通过了，推荐按下面方式继续使用。

### 9.1 同一个账号连续请求

```powershell
curl.exe -x socks5h://127.0.0.1:2260 --proxy-user "Default.user_tom:my-token" https://api.ipify.org
curl.exe -x socks5h://127.0.0.1:2260 --proxy-user "Default.user_tom:my-token" https://api.ipify.org
```

如果两次返回相同 IP，例如：

```text
85.xxx.xxx.45
85.xxx.xxx.45
```

说明：

- 同一个账号被稳定绑定到了同一个出口 IP
- 粘性代理正常

### 9.2 换另一个账号再试

```powershell
curl.exe -x socks5h://127.0.0.1:2260 --proxy-user "Default.user_jerry:my-token" https://api.ipify.org
```

如果返回另一个 IP，例如：

```text
72.xxx.xxx.50
```

说明：

- 不同账号可以分配到不同出口
- 粘性账号隔离正常

### 9.3 这条命令里各部分是什么意思

```text
Default.user_tom:my-token
```

在 `V1` 模式下表示：

- `Default`
  平台名
- `user_tom`
  业务账号
- `my-token`
  代理密码

也就是：

- `Platform.Account:RESIN_PROXY_TOKEN`

---

## 10. 常用命令速查

### 10.1 进入项目目录

```powershell
cd "E:\script\Resin"
```

### 10.2 构建前端

```powershell
cd "E:\script\Resin\webui"
npm ci
npm run build
cd "E:\script\Resin"
```

### 10.3 运行后端

```powershell
go run -tags "with_quic with_wireguard with_grpc with_utls" ./cmd/resin
```

### 10.4 测试 HTTP 代理

```powershell
curl.exe -x http://127.0.0.1:2260 -U ":my-token" https://api.ipify.org
```

### 10.5 测试 SOCKS5 代理

```powershell
curl.exe -x socks5h://127.0.0.1:2260 --proxy-user ":my-token" https://api.ipify.org
```

### 10.6 测试 SOCKS5 粘性代理

```powershell
curl.exe -x socks5h://127.0.0.1:2260 --proxy-user "Default.user_tom:my-token" https://api.ipify.org
```

### 10.7 跑本次功能相关测试

```powershell
go test ./internal/config ./internal/proxy ./cmd/resin
```

### 10.8 跑全量测试

```powershell
go test ./...
```

说明：

- 这条命令会跑整个仓库所有测试
- 截至当前工作树，`internal/routing` 和 `internal/topology` 里仍有历史失败项
- 这些失败项与本次 SOCKS5 功能无直接关系

### 10.9 代码格式化

```powershell
gofmt -w "cmd/resin/app_runtime.go" "internal/config/env.go" "internal/proxy/socks5.go"
```

如果你是格式化整个项目里的 Go 文件，可以自行配合 `rg`、`Get-ChildItem` 使用。

---

## 11. 常见报错与处理方式

### 11.1 `go: The term 'go' is not recognized`

说明当前 PowerShell 里找不到 Go。

先执行：

```powershell
$env:Path += ";C:\Program Files\Go\bin"
go version
```

如果能显示版本号，就继续运行。

---

### 11.2 `webui/dist` 不存在

说明前端还没构建。

执行：

```powershell
cd "E:\script\Resin\webui"
npm ci
npm run build
cd "E:\script\Resin"
```

---

### 11.3 `CONNECT tunnel failed, response 503`

这通常不是 HTTP/SOCKS5 本身坏了，而是：

- 当前没有可用节点

先去后台导入订阅，等节点刷新后再试。

---

### 11.4 GeoIP 下载报 403

类似：

```text
[geoip] initial download failed: ... 403 ...
```

这通常不会阻止 Resin 启动。

影响是：

- GeoIP 数据没有成功拉下来
- 但 HTTP / SOCKS5 代理功能依然可以继续验证

所以这个报错在本地调试阶段通常可以先忽略。

---

### 11.5 HTTP 能用，SOCKS5 不能用

先检查你是不是写成了下面这种格式：

```powershell
curl.exe -x socks5h://127.0.0.1:2260 --proxy-user ":my-token" https://api.ipify.org
```

如果仍然不通，优先检查：

- Resin 启动日志里是否有 `HTTP + SOCKS5`
- 当前端口是否仍然是 `2260`
- 是否已经导入有效节点

---

## 12. 如何停止本地服务

如果 Resin 正在 PowerShell 前台运行：

- 直接按 `Ctrl + C`

就可以停止当前服务。

---

## 13. 如何清理本地临时数据

如果你只是本地调试结束，想删除本次临时运行留下的数据：

```powershell
Remove-Item -Recurse -Force ".tmp"
```

说明：

- `.tmp` 是本地运行时创建的临时目录
- 删除它不会影响 Git 仓库代码
- 但会清掉你这次本地运行产生的缓存、状态和日志

---

## 14. 最短使用流程

如果你已经装好 Go 和 Node.js，最短流程就是：

### 第一步：构建前端

```powershell
cd "E:\script\Resin\webui"
npm ci
npm run build
cd "E:\script\Resin"
```

### 第二步：启动 Resin

```powershell
New-Item -ItemType Directory -Force -Path ".tmp\cache",".tmp\state",".tmp\log" | Out-Null
$env:RESIN_AUTH_VERSION="V1"
$env:RESIN_ADMIN_TOKEN="admin-123"
$env:RESIN_PROXY_TOKEN="my-token"
$env:RESIN_LISTEN_ADDRESS="127.0.0.1"
$env:RESIN_PORT="2260"
$env:RESIN_SOCKS5_TIMEOUT="3s"
$env:RESIN_CACHE_DIR=(Resolve-Path ".tmp\cache").Path
$env:RESIN_STATE_DIR=(Resolve-Path ".tmp\state").Path
$env:RESIN_LOG_DIR=(Resolve-Path ".tmp\log").Path
go run -tags "with_quic with_wireguard with_grpc with_utls" ./cmd/resin
```

### 第三步：导入节点

- 浏览器打开 `http://127.0.0.1:2260`
- 用 `admin-123` 登录
- 导入订阅

### 第四步：验证 HTTP

```powershell
curl.exe -x http://127.0.0.1:2260 -U ":my-token" https://api.ipify.org
```

### 第五步：验证 SOCKS5

```powershell
curl.exe -x socks5h://127.0.0.1:2260 --proxy-user ":my-token" https://api.ipify.org
```

### 第六步：验证粘性账号

```powershell
curl.exe -x socks5h://127.0.0.1:2260 --proxy-user "Default.user_tom:my-token" https://api.ipify.org
curl.exe -x socks5h://127.0.0.1:2260 --proxy-user "Default.user_tom:my-token" https://api.ipify.org
curl.exe -x socks5h://127.0.0.1:2260 --proxy-user "Default.user_jerry:my-token" https://api.ipify.org
```

---

## 15. 当前这次改动你应该怎么判断算“完成”

就当前这次 SOCKS5 功能来说，满足下面几点就可以认为本地验证通过：

- `go test ./internal/config ./internal/proxy ./cmd/resin` 通过
- HTTP 测试能返回 IP
- SOCKS5 测试能返回 IP
- 同账号连续两次请求返回相同 IP
- 不同账号能拿到不同 IP

你已经实际完成过这一整套验证，所以这次功能已经可以视为：

- 本地实现完成
- 本地功能验证通过
