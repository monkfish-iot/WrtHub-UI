# Makefile - WrtHub-UI（开发技术文档 §8）
#
# 主编译入口：Linux 本机直接编译运行（开发与部署同环境）。
# Windows 交叉编译见 build.ps1（产物路径一致，均输出到 ./dist/）。
#
# 常用：
#   make            # vet + build（产物在 ./dist/，已含 config.yaml）
#   make build      # 编译到 ./dist/wrthub-ui + 复制 config.yaml
#   make run        # 编译并从 dist/ 运行
#   make package    # 打 tar.gz 部署包（解压即用）
#   make vet        # 仅 go vet
#   make clean      # 清理 dist/
#
# 部署：
#   make package
#   scp dist/wrthub-ui-linux-amd64.tar.gz root@host:~/
#   tar -xzf wrthub-ui-linux-amd64.tar.gz   # 解出 wrthub-ui / config.yaml
#   ./wrthub-ui                              # data/ 由 store.Open 启动时自动创建

BINARY   := wrthub-ui
OUT_DIR  := dist
OUT      := $(OUT_DIR)/$(BINARY)
PKG      := ./cmd/wrthub-ui
GOFLAGS  := -trimpath -ldflags "-s -w"
ENV      := CGO_ENABLED=0
TARNAME  := $(BINARY)-linux-amd64.tar.gz

.PHONY: all build vet run package clean

all: vet build

vet:
	go vet ./...

# build 是 .PHONY，每次都重编；$(OUT_DIR) 仅在缺失时创建。
build: $(OUT_DIR)
	$(ENV) go build $(GOFLAGS) -o $(OUT) $(PKG)
	@cp -f config.yaml $(OUT_DIR)/config.yaml 2>/dev/null || \
		echo "warn: config.yaml not found at project root"
	@echo "built: $(OUT)"

$(OUT_DIR):
	mkdir -p $(OUT_DIR)

# 切到 dist/ 运行：让 wrthub-ui 在二进制同级目录读 config.yaml / 写 data/。
run: build
	cd $(OUT_DIR) && ./$(BINARY)

# Linux 上 go build 产物默认带 +x，tar 直接保留执行位。
package: build
	cd $(OUT_DIR) && tar -czf $(TARNAME) $(BINARY) config.yaml
	@echo "packaged: $(OUT_DIR)/$(TARNAME)"

clean:
	rm -rf $(OUT_DIR)
