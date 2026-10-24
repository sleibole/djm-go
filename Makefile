.PHONY: dev dev-agent dev-list dev-stop dev-stop-all run build test docker-build docker-run

# Personal server on port 8200. Not tracked by the dev-agent commands.
# ADDR is what the process binds. PORT is set to the same port so the two
# cannot disagree. loadDotEnv leaves variables that are already set alone.
# Air is a developer tool, run by version so it is not a module dependency.
dev:
	ADDR=:8200 PORT=8200 BASE_URL=http://localhost:8200 go run github.com/air-verse/air@v1.67.4

dev-agent:
	@test -n "$(NAME)" || { echo "usage: make dev-agent NAME=<unique-name>" >&2; exit 1; }
	./scripts/dev-server.sh start "$(NAME)"

dev-list:
	./scripts/dev-server.sh list

dev-stop:
	@test -n "$(NAME)" || { echo "usage: make dev-stop NAME=<name>" >&2; exit 1; }
	./scripts/dev-server.sh stop "$(NAME)"

dev-stop-all:
	./scripts/dev-server.sh stop-all

run:
	ADDR=:8200 PORT=8200 BASE_URL=http://localhost:8200 go run .

build:
	CGO_ENABLED=0 go build -o djm .

test:
	go test ./...

docker-build:
	docker build -t djm .

# The process inside the container keeps the image default, port 8080.
# The host port is 8200 so this does not take track-anything's port.
# Do not pass ADDR or PORT into the container.
docker-run: docker-build
	docker run --rm -p 8200:8080 -e BASE_URL=https://localhost:8200 -v djm-data:/data djm
