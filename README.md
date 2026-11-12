# submini

为初代 iPad mini（iOS 9.3.5 的 Safari）准备的极简 Subsonic 播放器。

现代前端框架（Vue/React + ES6）在这台机器的 Safari 上跑不起来，所以这个项目的页面是**手写 ES5 单文件**：没有构建步骤、没有框架、没有外部资源。服务端只做两件事——托管这个页面、把 `/rest/*` 反向代理到 Subsonic 服务端（Navidrome），让页面与 API 同源，绕开老浏览器的 CORS 限制；音频流的 Range 请求由代理原样透传，拖进度条、断点续播都靠它。

## 运行

镜像由 GitHub Actions 在打 `v*` 标签时构建，推送到 Docker Hub（`passport888/submini`）与 GHCR。家里部署只需拉镜像：

```bash
docker compose pull
docker compose up -d
# 打开 http://<家里服务器>:9898 ，用 Navidrome 的账号密码登录
```

本地构建也可以：`docker build -t submini .`，或直接 `go run .`（见下文）。

环境变量：

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `SUBSONIC_URL` | `http://localhost:4533` | 上游 Subsonic API 地址（Navidrome），如 `http://192.168.1.10:4533`。用 compose 部署时，在同目录建一个 `.env` 文件写 `SUBSONIC_URL=你的地址` 即可覆盖（`.env` 已被 gitignore，不会进仓库） |
| `LISTEN` | `:9898` | 监听地址 |

## 功能

- 登录（Subsonic token 鉴权，令牌存浏览器 localStorage，下次免登录）
- 歌单 / 专辑（最新）/ 歌手 / 随机歌曲 / 搜索（歌曲·专辑·歌手）
- 播放队列 = 所在列表，自动连播，上一首/下一首，进度条可点选拖动
- 播放请求带 `format=mp3`：Navidrome 会把 FLAC 等格式实时转码，老 Safari 只认 MP3/AAC 也能放全曲库

## iOS 9 约束（改页面时注意）

- 只用 ES5：不用箭头函数、`let/const`、模板字符串、Promise、`fetch`（用 XMLHttpRequest），MD5 为手写实现（token 鉴权要用）
- 布局只用 float/block，不用 flex/gap/grid
- 播放必须由点击手势直接触发，`playAt()` 不要隔异步回调调用

## 本地开发

```bash
go run .            # 需要 Go 1.21+
SUBSONIC_URL=http://localhost:4533 go run .
```
