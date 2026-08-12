FROM node:24-alpine AS frontend-build
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM golang:1.25-alpine AS backend-build
WORKDIR /src
COPY go.mod ./
COPY backend/ ./backend/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/ai-video ./backend/src/cmd/server

FROM nginx:1.29-alpine
RUN mkdir -p /app /data /tmp/nginx/client_temp /tmp/nginx/proxy_temp \
        /tmp/nginx/fastcgi_temp /tmp/nginx/uwsgi_temp /tmp/nginx/scgi_temp \
    && chown -R nginx:nginx /app /data /tmp/nginx
COPY --from=backend-build --chown=nginx:nginx /out/ai-video /app/ai-video
COPY --from=frontend-build --chown=nginx:nginx /src/frontend/dist /usr/share/nginx/html
COPY --chown=nginx:nginx deploy/nginx.conf /etc/nginx/nginx.conf
COPY --chown=nginx:nginx deploy/entrypoint.sh /app/entrypoint.sh
RUN chmod 0555 /app/ai-video /app/entrypoint.sh
USER nginx
ENV DATA_DIR=/data \
    LISTEN_ADDR=127.0.0.1:8780 \
    WORKER_COUNT=2 \
    POLL_INTERVAL=10s \
    ALLOW_PAID_GENERATION=false
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/app/entrypoint.sh"]
