.PHONY: build test vet fmt run ui docker

build:
	go build -trimpath -o bin/tokenanalyse ./cmd/tokenanalyse

test:
	go test -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -w cmd internal

run: build
	./bin/tokenanalyse node --db ./data/tokenanalyse.db --listen 127.0.0.1:8787

ui:
	wails build -clean

docker:
	docker build -t tokenanalyse:local .
