# 小网页书架（Caddy）

云吞的系统插件 `app.wonton.static-host`：静态网页插件（`runtime: static_web`）没有自己的容器，核心把它们的文件交给本插件提供；
页面经 `/_wonton/data/` 存的小块数据也由本插件读写。通用的开发流程和规则见[开发指南](https://github.com/wontonapp/wonton-docs/blob/main/develop.md)
和[插件规范](https://github.com/wontonapp/wonton-docs/blob/main/plugin-spec.md)。

| 能力 | 服务 | 端口 | 核心挂进来的目录 |
| --- | --- | --- | --- |
| `static_web` | `static`（Caddy） | 8080 | `/srv/static`，只读 |
| `web_storage` | `storage`（`images/wonton-web-storage/`） | 8081 | `/srv/data`，读写 |

两个目录里每个插件一个子目录，名字就是插件 ID，由核心建和替换。核心网关转发前删掉请求里所有 `X-Wonton-*` 头，再写入
`X-Wonton-Plugin: <插件 ID>`；两个服务只按这个头选子目录，不看路径和 `Host`。头没有或不合法回 400，子目录不在回 404。
鉴权、CSP、`nosniff`、隔离头和 SPA 的 `fallback` 都在网关，这里不做。

## static_web

RFC 9110 的静态文件语义：`GET`、`HEAD`，按扩展名的 `Content-Type`，`ETag` 和条件请求，`Range`；目录取 `index.html`，
不列目录；别的方法 405。核心用 `renameat2(RENAME_EXCHANGE)` 对换插件子目录，Caddy 每个请求都重新 stat，换完下一个请求就是新内容。

Caddy 的 `ETag` 由文件的修改时间（纳秒）和大小算出，修改时间是 0 或 1 秒的文件不给 `ETag`。

## web_storage

WebDAV（RFC 4918）整文件读写的子集，加 RFC 9110 的条件请求。网关去掉 `/_wonton/data` 前缀再转发，页面的
`fetch('/_wonton/data/todo.json')` 到这里是 `/todo.json`。

| 方法 | 行为 |
| --- | --- |
| `GET`、`HEAD` | 整个文件，带 `ETag` 和 `Content-Type`（不认识的扩展名是 `application/octet-stream`，不嗅探）；目录 405 |
| `PUT` | 写临时文件再改名，不留半个文件；新建 201、覆盖 204，回新的 `ETag`。`If-Match` 不符、`If-None-Match: *` 而文件在回 412；父目录不在 409；带 `Content-Range` 501 |
| `DELETE` | 文件或空目录 204；非空目录 409；不在 404 |
| `MKCOL` | 201；已在 405；父目录不在 409；带请求体 415 |
| `PROPFIND` | 只收 `Depth: 0`、`1`，别的 501；不看请求体，回 `getcontentlength`、`getetag`、`getlastmodified`、`resourcetype` |
| `LOCK`、`UNLOCK`、`MOVE`、`COPY`、`PROPPATCH` | 405 |
| 其他 | 501 |

- 上限：每个插件 64 MiB、1024 个文件和目录（目录也算，否则 `MKCOL` 没有上限），单个文件 8 MiB。超了回 507，原文件不动；
  有 `Content-Length` 先查，边收边查。
- `ETag` 是内容的 SHA-256。用 inode、大小和修改时间拼的话，同样大小的两次写落在同一个时间戳刻度、又复用了刚释放的 inode
  时会重复，过期的 `If-Match` 就能通过。
- `PROPFIND` 的 `href` 是相对请求地址的引用（RFC 4918 §8.3），因为网关去掉了前缀，这里看到的绝对路径对页面是错的。
- 路径逐段解码：编码的 `/`（`%2f`）、`..`、`.` 和以 `.` 开头的段一律 400，所以碰不到别的插件和临时文件。

### 为什么自己写

现成的 WebDAV 服务端（Caddy 的 webdav 模块、`rclone serve webdav`、Go 的 `x/net/webdav`）都没有按目录的配额，做不到
“超了回 507、原文件不动”；SFTPGo 的配额按用户，不能按请求头选根目录，而且是 AGPL。缺的正好是契约里最要紧的一项，
在它们前面再加一层也绕不开（流式写入时要边收边查），所以写了一个只用 Go 标准库的小服务（`images/wonton-web-storage/`），
`go test` 覆盖上面的表。

## 验证

```bash
scripts/check.sh                                   # 插件包
(cd images/wonton-web-storage && go test ./...)    # 存储服务的单元测试
scripts/contract.sh                                # 对着真的容器跑两份契约（要 Docker）
```

`scripts/contract.sh` 照核心的布局建临时目录，起 Caddy 和存储服务两个容器，带上 `X-Wonton-Plugin` 逐项请求；只建、只删
`sh-test-` 开头的容器，没给 `STORAGE_IMAGE` 时自己构建 `sh-test-storage` 并在结束时删掉。

内存：2.11.9 的 Caddy 空闲约 12 MiB，存储服务约 2 MiB（amd64，`docker stats`），清单写 64M。

## 许可证

插件包是 MIT（[LICENSE](LICENSE)）；Caddy 是 Apache-2.0；清单的 `license` 写 `Apache-2.0 AND MIT`。详见 [NOTICE](NOTICE)。
