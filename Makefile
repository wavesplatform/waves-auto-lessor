PROJECT=waves-auto-lessor
SOURCE=$(shell find . -name '*.go' | grep -v vendor/)
VERSION=$(shell git describe --tags --always --dirty)

.PHONY: vendor vetcheck fmtcheck clean

all: vendor vetcheck fmtcheck mod-clean dist

ver:
	@echo Building version: $(VERSION)

fmtcheck:
	@gofmt -l -s $(SOURCE) | grep ".*\.go"; if [ "$$?" = "0" ]; then exit 1; fi

mod-clean:
	go mod tidy

clean:
	@rm -rf build
	go mod tidy

vendor:
	go mod vendor

vetcheck:
	go vet ./...
	golangci-lint run

build-linux:
	@CGO_ENABLE=0 GOOS=linux GOARCH=amd64 go build -o build/bin/linux-amd64/waves-auto-lessor -ldflags="-X main.version=$(VERSION)" $(SOURCE)
build-darwin:
	@CGO_ENABLE=0 GOOS=darwin GOARCH=amd64 go build -o build/bin/darwin-amd64/waves-auto-lessor -ldflags="-X main.version=$(VERSION)" $(SOURCE)
build-windows:
	@CGO_ENABLE=0 GOOS=windows GOARCH=amd64 go build -o build/bin/windows-amd64/waves-auto-lessor.exe -ldflags="-X main.version=$(VERSION)" $(SOURCE)
build-linux-arm64:
	@CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o build/bin/linux-arm64/waves-auto-lessor -ldflags="-X main.version=$(VERSION)" $(SOURCE)

release: ver build-linux build-darwin build-windows build-linux-arm64

dist: release
	@mkdir -p build/dist
	@cd ./build/; zip -j ./dist/waves-auto-lessor_$(VERSION)_Windows-amd64.zip ./bin/windows-amd64/waves-auto-lessor*
	@cd ./build/bin/linux-amd64/; tar pzcvf ../../dist/waves-auto-lessor_$(VERSION)_Linux-amd64.tar.gz ./waves-auto-lessor*
	@cd ./build/bin/darwin-amd64/; tar pzcvf ../../dist/waves-auto-lessor_$(VERSION)_macOS-amd64.tar.gz ./waves-auto-lessor*
	@cd ./build/bin/linux-arm64/; tar pzcvf ../../dist/waves-auto-lessor_$(VERSION)_Linux-arm64.tar.gz ./waves-auto-lessor*
