BIN := bin/claudego
PREFIX ?= /opt/homebrew/bin

build:
	go build -o $(BIN) .

test: build
	bash test/integration.sh

vet:
	go vet ./...

install: build
	cp $(BIN) $(PREFIX)/claudego
	@echo "已安装到 $(PREFIX)/claudego（launchd 请在安装后重新运行 claudego install-launchd）"

.PHONY: build test vet install
