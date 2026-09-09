# xhttp-go

Xray-core 的 VLESS + XHTTP 协议的最小化零依赖 Go 重写（仅服务端）。

- **零第三方依赖**：全部基于 Go 标准库（`net/http`、`crypto` 等）。
- **单二进制**：静态编译、无运行时依赖，提供 Linux amd64 / arm64 预编译下载。
- **与真实 Xray 客户端互通**：已验证 `mode=auto`（GET/POST）、`stream-up`、`packet-up`、`stream-one` 四种模式，TCP 与 UDP（含 DNS）均正常。

## 下载预编译二进制

从 [Releases](../../releases) 页面下载对应架构的预编译二进制 tar.gz 包（由 GitHub Actions 自动构建，内含可执行文件并附带 sha256 校验和）：

| 文件 | 适用平台 |
|------|---------|
| `xhttp-go-linux-amd64.tar.gz` | x86_64 Linux |
| `xhttp-go-linux-arm64.tar.gz` | ARM64 Linux（如 Oracle/AWS Graviton、树莓派 4+） |

```bash
wget https://github.com/OWNER/xhttp-go/releases/latest/download/xhttp-go-linux-amd64.tar.gz
tar -xzf xhttp-go-linux-amd64.tar.gz
chmod +x xhttp-go-linux-amd64
sudo mv xhttp-go-linux-amd64 /usr/local/bin/xhttp-go
```

打 `v*` 标签即触发自动发布；推送到 `main` 也会在 Actions 中构建产物（Artifacts）供直接下载。

## 快速开始

```bash
# 编译
go build -trimpath -ldflags "-s -w" -o xhttp-go .

# 运行（参数可由环境变量覆盖）
./xhttp-go -port 3000 -uuid 5efabea4-f6d4-91fd-b8f0-17e004c89c60 -path /xtp

# 或使用环境变量
UUID=5efabea4-f6d4-91fd-b8f0-17e004c89c60 PORT=3000 XPATH=/xtp ./run.sh
```



## 配置项

| 环境变量 | 命令行参数 | 默认值 | 说明 |
|---------|-----------|--------|------|
| `UUID` | `-uuid` | （必填） | VLESS 用户 UUID |
| `PORT` | `-port` | `3000` | HTTP 监听端口 |
| `XPATH` | `-path` | `/xtp` | XHTTP 路径 |
| `XHOST` | `-host`  | `127.0.0.1` | HTTP 监听地址；直连公网设 `0.0.0.0`，IPv6-only 服务器设 `::` |
| `HEALTHY_PAGE` | `-camo` | `false` | 启用伪装页：根路径返回编译时嵌入的 `index.html`（无则 "Hello world"） |
| `PADDING_REQUIRED` | `-padding-required` | `false` | 强制校验 `x_padding`（长度 100–1000，Referer 或 URL 查询参数）。套 CDN 时建议开启以拦截边缘扫描器；Xray 客户端总是填充，可正常互通 |

## 伪装页

设置 `HEALTHY_PAGE=true`（或 `-camo`）后，访问根路径 `/` 返回伪装内容而非 `OK`：

```bash
HEALTHY_PAGE=true ./xhttp-go -uuid ...
```

伪装页内容来自 `internal/page/index.html`，编译时通过 `go:embed` 嵌入二进制：

```bash
# 自定义伪装页：替换文件后重新编译
cp your-page.html internal/page/index.html
go build -o xhttp-go .
```

- 文件为 HTML 文档（含 `<!DOCTYPE html>` 或 `<html>`）时按 `text/html` 返回；
- 纯文本（默认内容 `Hello world`）按 `text/plain` 返回；
- 关闭（默认）时根路径仅返回 `OK`；XHTTP 路径不受伪装页影响。

## 客户端配置示例

客户端配置见 [sample-client-config.json](sample-client-config.json)。关键点：

```json
{
  "network": "xhttp",
  "security": "none",
  "xhttpSettings": {
    "path": "/xtp",
    "host": "your.domain.com",
    "mode": "auto"
  }
}
```

`mode` 可选 `auto` / `packet-up` / `stream-up` / `stream-one`，四种模式均已验证互通（TCP/UDP）。

## 协议实现范围

对齐 Xray-core 服务端默认配置下的行为：

- **XHTTP 传输**（`transport/internet/splithttp`）
  - 路径布局 `/{path}/{sid}[/{seq}]`（session/seq 在路径中）
  - `GET  /{path}/{sid}` → stream-down（SSE 头 + 流式响应体）
  - `POST /{path}/{sid}/{seq}` → packet-up（按 seq 重排，1MB 包上限，30 包重排缓冲）
  - `POST /{path}/{sid}` → stream-up（请求体即上行流，独占，重复 push 返回 409）
  - `POST /{path}/` → stream-one（请求体即上行流，响应体即下行流，单连接双向）
  - 会话 TTL：创建后 30 秒内未完成 GET 即回收
  - `x_padding` 校验（Referer 或 URL 查询参数，长度 100–1000）；默认放行，`PADDING_REQUIRED=true` 时强制
  - 响应头：`X-Accel-Buffering: no`、`Cache-Control: no-store`、`Content-Type: text/event-stream`、CORS
  - OPTIONS 预检返回 200（浏览器 dialer 支持）
- **VLESS 协议**（`proxy/vless`）
  - 请求头：版本(0) + UUID + addons(跳过) + 指令 + 端口(BE) + 地址类型 + 地址
  - 指令：TCP(0x01)、UDP(0x02)；MUX(0x03) 不支持  - 响应头：版本(0) + addons 长度(0)，共 2 字节
  - UDP 双向 2 字节大端长度分帧（LengthPacket）
- **出站（freedom）**
  - TCP 直接拨号；UDP 用 connected socket（cone NAT 语义）
  - 不支持 blackhole 路由/拦截规则（如需封禁请用防火墙）

## 部署（Nginx/CDN 反向代理）

XHTTP 设计上通过 CDN 或 Nginx 前置，本服务端监听纯 HTTP。Nginx 示例：

```nginx
location /xtp {
    proxy_pass http://127.0.0.1:3000;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header Referer $http_referer;
    # 关键：关闭缓冲，否则下行流会被 Nginx 缓存
    proxy_buffering off;
    proxy_request_buffering off;
    proxy_read_timeout 300s;
    proxy_send_timeout 300s;
}
```

客户端侧把 `security` 设为 `tls`、端口设为 443 即可经由 CDN。响应中已带
`X-Accel-Buffering: no`，多数反代会自动禁用缓冲。

## 与 Xray 的差异

- 仅支持 HTTP/1.1（Xray 服务端还支持 h2c/h3）
- 不支持 XTLS Vision flow / REALITY / TLS（如需 TLS 请用 Nginx/Caddy 前置）
- 不支持 MUX、浏览器 dialer、DownloadSettings 分离下载通道
- 出站无路由/规则引擎（等价于 freedom + 无规则）

## 目录结构

```
main.go                    入口：参数、组装、freedom 出站、VLESS 桥接
internal/uuid/uuid.go      RFC 4122 UUID 解析
internal/vless/vless.go    VLESS 协议头解析/编码
internal/xhttp/server.go   XHTTP 路由 + padding 校验 + CORS
internal/xhttp/session.go  会话生命周期 + 三种上行/下行模式
internal/xhttp/queue.go    上行重排队列（packet-up）
internal/xhttp/conn.go     HTTP 请求 ↔ 双向连接适配
internal/xhttp/bridge.go   XHTTP 会话 → VLESS net.Conn 桥接
```
