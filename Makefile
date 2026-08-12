.PHONY: dev-backend dev-frontend test build docker

dev-backend:
	go run ./backend/src/cmd/server

dev-frontend:
	cd frontend && npm run dev

test:
	go test -race ./...
	cd frontend && npm run build

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -o bin/ai-video ./backend/src/cmd/server

docker:
	docker build -t ai-video:local .
