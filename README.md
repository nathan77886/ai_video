# AI Video

本地短剧生产管理台。Go 服务管理项目、剧本、资源、视频和异步生成任务；React 提供操作界面；单个 Docker 镜像内由 nginx 提供前端并代理 Go API。

## 启动

~~~bash
docker compose up --build
~~~

打开 <http://127.0.0.1:8080>。运行数据保存在 Docker volume ai-video-data。

若 8080 已占用，使用 APP_PORT=8081 docker compose up --build，并打开 <http://127.0.0.1:8081>。

默认监听 0.0.0.0，内网机器可访问。服务未实现账号认证，不应直接暴露到公网；内网应仅限可信用户与网段。

默认只能运行 Mock 预演，不调用付费模型。启用 MiniMax：

~~~bash
cp .env.example .env
# 编辑 .env，设置 MINIMAX_API_KEY，并明确设置：
# ALLOW_PAID_GENERATION=true
docker compose up --build
~~~

MiniMax 使用官方异步 v1 接口：提交任务、轮询状态、获取下载地址、下载视频到本地数据卷。密钥不写入状态文件或日志。

注意：MiniMax v1 没有取消接口。界面“取消”只停止本地轮询，远端任务和费用不会停止。

## 本地开发

~~~bash
make dev-backend
make dev-frontend
~~~

前端开发地址通常为 <http://127.0.0.1:5173>，Vite 将 /api 代理到 127.0.0.1:8780。

## 验证

~~~bash
make test
docker build -t ai-video:local .
~~~

## 配置

| 环境变量 | 默认值 | 用途 |
| --- | --- | --- |
| DATA_DIR | /data（镜像内） | JSON 状态和媒体目录 |
| WORKER_COUNT | 2 | 固定并发 worker 数 |
| POLL_INTERVAL | 10s | 异步任务查询间隔 |
| MINIMAX_API_KEY | 空 | MiniMax API 密钥 |
| MINIMAX_BASE_URL | https://api.minimax.io | MiniMax API 地址 |
| ALLOW_PAID_GENERATION | false | 付费调用总开关 |
| APP_PORT | 8080 | 宿主机监听端口 |
| MAX_UPLOAD_BYTES | 262144000 | 单文件上传上限 |
| MAX_DOWNLOAD_BYTES | 2147483648 | 生成视频下载上限 |

ponytail: 首版使用单机 JSON 原子落盘和进程内 worker；多实例或任务量明显增长时换 SQLite/Postgres 与独立队列。
