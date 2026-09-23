.PHONY: build run serve test vet fmt build-ui dev-backend dev-frontend clean

build:
	go build -o bin/agentevals ./cmd/agentevals

run: build
	./bin/agentevals run $(ARGS)

serve: build
	./bin/agentevals serve --ui ui/dist

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l .

build-ui:
	cd ui && npm ci && npm run build

dev-backend:
	go run ./cmd/agentevals serve --ui ui/dist

dev-frontend:
	cd ui && npm run dev

clean:
	rm -rf bin ui/dist
