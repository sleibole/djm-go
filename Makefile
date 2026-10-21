.PHONY: dev run build test docker-build docker-run

# Air is a developer tool, run by version so it is not a module dependency.
dev:
	go run github.com/air-verse/air@v1.67.4

run:
	go run .

build:
	CGO_ENABLED=0 go build -o djm .

test:
	go test ./...

docker-build:
	docker build -t djm .

# Without S3_BUCKET the entrypoint skips Litestream and runs the app directly.
docker-run: docker-build
	docker run --rm -p 8080:8080 -e BASE_URL=https://localhost -v djm-data:/data djm
