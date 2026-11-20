# Traefik Request ID

为 HTTP 请求生成新的 `X-Request-ID`，覆盖客户端原值，并写入请求和响应头。无第三方 Go 依赖。

## ID 格式

ID 固定为 **32 个字符**，由 UTC 日期时间和随机十六进制字符串组成：

```text
20250101192959ABCDEF1234567890AB
```

| 部分 | 长度 | 规则 |
| --- | --- | --- |
| 日期时间 | 14 位 | UTC+0，格式为 `YYYYMMDDhhmmss` |
| 随机部分 | 18 位 | 随机数，编码为大写十六进制 |

## 工作方式

- 每个请求重新生成 ID，不信任或复用客户端提供的值。
- 同一次请求的后端重试沿用同一个 ID；客户端重新发起请求时生成新的 ID。
- 后端读取请求 ID 用于日志关联，响应 ID 由插件设置。
- 不读取或缓冲请求体、响应体。
- 随机数生成失败时返回 HTTP 500，不转发请求，不使用客户端 ID 或时间戳替代。

## 配置

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `headerName` | `X-Request-ID` | 请求和响应使用的头名称，空值使用默认值 |

自定义头名称须以 `X-` 开头且后缀非空，仅允许字母、数字和连字符，名称不区分大小写。
请使用专门的 ID 头，避免使用 `X-Forwarded-*` 等已有用途的头名称。

## 安装与使用

### 1. 克隆插件源码

在 Traefik 部署目录中执行，以下源码路径均相对于该目录：

```sh
mkdir -p ./plugins/src/github.com/xuewenG
git clone https://github.com/xuewenG/traefik-plugin-request-id.git \
  ./plugins/src/github.com/xuewenG/traefik-plugin-request-id
```

在 Docker Compose 的 Traefik 服务中配置挂载，将宿主机的 `./plugins`
映射到容器内的 `/plugins-local`：

```yaml
working_dir: /
volumes:
  - ./plugins:/plugins-local:ro
```

### 2. 配置 Traefik 静态配置

`traefik.yml`：

```yaml
entryPoints:
  web:
    address: ':80'

experimental:
  localPlugins:
    request-id:
      moduleName: github.com/xuewenG/traefik-plugin-request-id

providers:
  file:
    filename: /etc/traefik/dynamic.yml
```

### 3. 配置路由和中间件

将 `dynamic.yml` 放到静态配置指定的 `/etc/traefik/dynamic.yml`，
使用 Docker 时需挂载到容器内该路径。后端地址按实际部署修改：

```yaml
http:
  routers:
    app:
      rule: PathPrefix(`/`)
      entryPoints: [web]
      service: app
      middlewares: [request-id, backend-retry]

  middlewares:
    request-id:
      plugin:
        request-id:
          headerName: X-Request-ID
    backend-retry:
      retry:
        attempts: 3

  services:
    app:
      loadBalancer:
        servers:
          - url: http://backend:8080
```

**中间件顺序：** 每个请求仅经过一个 Request ID 实例，并将其放在 Retry
及可能提前返回的中间件之前。插件外侧提前返回的请求不会经过本插件。

## 使用限制

### 后端响应头

后端及后续中间件应保留请求中的 ID，**不要再次设置同名响应头**，
否则客户端可能收到重复或不同的 ID。自行接管连接并写出的原始响应不受插件控制。

### 103 Early Hints

后端发送 `103 Early Hints` 时，最终响应可能缺少 ID，但后端收到的请求 ID 不受影响。
**如果需要确保最终响应携带 ID，请在后端关闭 Early Hints。**

### Connection 头配置

**不要将 ID 头名称加入入口的 `forwardedHeaders.connection` 放行列表**，
否则客户端的 `Connection` 声明可能导致后续代理删除请求 ID。
Traefik 默认入口配置不受此问题影响，自定义 ID 头也应遵守此规则。

### 多层 Traefik 转发

仅在最外层可信入口启用插件，内部 Traefik 透传 ID。
多层同时启用会逐层覆盖请求 ID，还可能产生重复或不一致的响应 ID。

## 部署验证

连续发送两次请求，并在后端确认每次收到的 ID 与响应一致：

```sh
curl -i -H 'X-Request-ID: forged' http://localhost/
curl -i -H 'X-Request-ID: forged' http://localhost/
```

预期结果：

- 两次返回不同的 32 位 ID，均不为 `forged`。
- 每次后端收到的请求 ID 与客户端收到的响应 ID 一致，特殊场景见[使用限制](#使用限制)。
- 普通 HTTP 场景的响应中只有一个 ID 值。
