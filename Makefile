BIN := bin/claudego
PREFIX ?= /opt/homebrew/bin

build:
	go build -o $(BIN) .

test: build
	bash test/integration.sh

vet:
	go vet ./...

install: build
	# 先删再拷（新 inode）：macOS 上 cp 原位覆盖已签名二进制会让签名缓存失效，
	# 新起的进程被 AMFI 直接 SIGKILL（RC=137）。正在运行的旧映像不受影响。
	rm -f $(PREFIX)/claudego
	cp $(BIN) $(PREFIX)/claudego
	@echo "已安装到 $(PREFIX)/claudego（launchd 请在安装后重新运行 claudego install-launchd）"

.PHONY: build test vet install
