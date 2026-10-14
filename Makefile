.PHONY: dev run build test docker-build docker-run

dev:
	bin/dev

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
