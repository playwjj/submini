# submini

为初代 iPad mini（iOS 9.3.5 的 Safari）准备的极简 Subsonic 播放器。

现代前端框架（Vue/React + ES6）在这台机器的 Safari 上跑不起来，所以这个项目的页面是**手写 ES5 单文件**：没有构建步骤、没有框架、没有外部资源。服务端做三件事：托管这个页面、把 `/rest/*` 反向代理到 Subsonic 服务端（Navidrome 等），让页面与 API 同源绕开老浏览器的 CORS 限制；保存连接配置；并在转发时注入鉴权参数。音频流的 Range 请求由代理原样透传，拖进度条靠它。

## 运行

镜像由 GitHub Actions 在打 `v*` 标签时构建，推送到 Docker Hub（`passport888/submini`）与 GHCR。家里部署只需拉镜像：

```bash
mkdir -p data && sudo chown 65534:65534 data   # 容器以 nobody(65534) 运行，数据目录要归它
docker compose pull
docker compose up -d
```

compose 用的是 host 网络模式：Docker 默认网桥只有 IPv4，上游如果是纯 IPv6 地址（如公网 AAAA-only 域名）容器里会连不上；host 模式下容器直接用宿主机的网络栈，IPv6 照常，9898 端口也直接落在宿主机上。

然后打开 `http://<家里服务器>:9898`，**第一次打开会看到连接设置表单**：填服务器地址（如 `http://192.168.1.10:4533`）、Subsonic 用户名和密码，点「连接并保存」。后端会先向服务器验证一次，成功才保存；之后任何设备打开这个页面都直接进播放器，不用再输。

本地构建也可以：`docker build -t submini .`，或直接 `go run .`（见下文）。

## 配置与数据

连接信息存在后端的 SQLite 数据库里（`submini.db`），compose 把它挂在工作目录的 `./data` 下（容器内 `/data`），升级镜像不丢，备份也方便。**存的是 Subsonic 的 token 和 salt，不是密码原文**——密码只在提交表单时用一次，算完 token 就丢弃。点页面右上角「退出」会清掉已保存的连接，回到设置表单。

环境变量只剩两个基础设施项：

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `LISTEN` | `:9898` | 监听地址 |
| `DATA_DIR` | `.`（镜像里是 `/data`） | `submini.db` 所在目录 |

注意：设置接口（`/api/*`）本身没有鉴权，这是给可信局域网用的家用小工具，别把 9898 端口直接暴露到公网。

## 功能

- 歌单 / 专辑（最新）/ 歌手 / 随机歌曲 / 搜索（歌曲·专辑·歌手）
- 连续播放：点任意一首，所在列表整列连播
- 随机播放：播放条上的 🔀 开关，打开后在当前队列里乱序连播；另有「随机」页签随机抽 50 首
- 上一首/下一首，进度条可点选拖动
- 播放请求带 `format=mp3`：Navidrome 会把 FLAC 等格式实时转码，老 Safari 只认 MP3/AAC 也能放

## iOS 9 约束（改页面时注意）

- 只用 ES5：不用箭头函数、`let/const`、模板字符串、Promise、`fetch`（用 XMLHttpRequest）
- 布局只用 float/block，不用 flex/gap/grid
- 播放必须由点击手势直接触发，`playAt()` 不要隔异步回调调用

## 本地开发

```bash
go run .            # 需要 Go 1.26+
DATA_DIR=/tmp/submini-dev go run .
```
