# AI Video

本地短剧分镜生产台。Go 服务管理分镜审核、素材、试片和异步生成任务；React 提供操作界面；单个 Docker 镜像内由 nginx 提供前端并代理 Go API。

## 启动

~~~bash
docker compose up --build
~~~

打开 <http://127.0.0.1:8081>。服务监听 `0.0.0.0:8081`，同一内网机器可用宿主机 IP 访问。运行数据保存在 Docker volume ai-video-data。

修改端口时使用 `APP_PORT=端口 docker compose up --build`。

默认监听 0.0.0.0，内网机器可访问。服务未实现账号认证，不应直接暴露到公网；内网应仅限可信用户与网段。

默认只能运行 Mock 预演，不调用付费模型。启用 MiniMax：

~~~bash
# 当前 book2video 根目录的 .env 会注入容器：
# MINIMAX_API_KEY=...
# OPENAI_API_KEY=... # 可直接提供 CLIProxyAPI 的 API Key
# 或把本机 CLIProxyAPI key 放入 /mnt/ssd/cli-proxy-api/ai-video-openai.key，Compose 默认作为 secret 挂载
# 如需单独配置，复制 ai_video/.env.example 到 book2video/.env。
# 编辑 .env，并明确设置：
# ALLOW_PAID_GENERATION=true
docker compose up --build
~~~

MiniMax 使用 H3 V2 异步接口：提交任务、轮询状态、下载视频到本地数据卷。分镜页可独立排入首帧图、末帧图、预览图三项 `gpt-image-2` 低质量任务，不会生成视频；素材库也可为每个角色模型排入预览图、正面效果图、侧面效果图、动作效果图四项任务。图片结果会关联回镜头或角色素材。全部图片走独立图片队列与 `OPENAI_IMAGE_BASE_URL`（默认本机 CLIProxyAPI）。图片不受 `ALLOW_PAID_GENERATION` 限制，不计入 MiniMax 付费确认。密钥不写入状态文件或日志。

注意：界面“取消”只停止本地轮询，远端任务和费用不会停止。

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
| MINIMAX_BASE_URL | https://api.minimaxi.com | MiniMax H3 V2 API 地址 |
| OPENAI_API_KEY | 空 | CLIProxyAPI/OpenAI 兼容接口密钥 |
| OPENAI_API_KEY_FILE | /run/secrets/openai_api_key | 容器内密钥文件；`OPENAI_API_KEY` 非空时优先 |
| OPENAI_API_KEY_SECRET_FILE | /mnt/ssd/cli-proxy-api/ai-video-openai.key | 宿主机 Compose secret 文件 |
| OPENAI_IMAGE_BASE_URL | http://host.docker.internal:8317/v1 | GPT Image 2 接口地址 |
| ALLOW_PAID_GENERATION | false | 付费调用总开关 |
| APP_PORT | 8081 | 宿主机监听端口，绑定 `0.0.0.0` |
| MAX_UPLOAD_BYTES | 262144000 | 单文件上传上限 |
| MAX_DOWNLOAD_BYTES | 2147483648 | 生成视频下载上限 |

ponytail: 首版使用单机 JSON 原子落盘和进程内 worker；多实例或任务量明显增长时换 SQLite/Postgres 与独立队列。
