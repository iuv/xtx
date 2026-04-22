APP_NAME := xtx
VERSION := 0.1.0
BUILD_DIR := build

# 当前系统
HOST_OS := $(shell go env GOOS)
HOST_ARCH := $(shell go env GOARCH)

# Go 工具路径（确保 GOBIN 在 PATH 中）
GOBIN := $(shell go env GOPATH)/bin
FYNE_CROSS := $(GOBIN)/fyne-cross
export PATH := $(GOBIN):$(PATH)

# 全平台列表（用于 fyne-cross 交叉编译）
PLATFORMS := \
	darwin/amd64 \
	darwin/arm64 \
	linux/amd64 \
	linux/arm64 \
	windows/amd64 \
	windows/arm64

.PHONY: build run clean all package package-all cross cross-linux cross-windows cross-darwin

# 本地构建（当前平台）
build:
	CGO_ENABLED=1 go build -ldflags="-s -w" -o $(BUILD_DIR)/$(APP_NAME) .

# 运行
run:
	go run .

# 构建当前 OS 的所有架构
all: clean
	@echo "Building for $(HOST_OS)..."
	@CGO_ENABLED=1 go build -ldflags="-s -w" -o $(BUILD_DIR)/$(APP_NAME)-$(HOST_OS)-$(HOST_ARCH) .
	@echo "  → $(BUILD_DIR)/$(APP_NAME)-$(HOST_OS)-$(HOST_ARCH)"

# 打包当前平台（tar.gz / zip）
package: all
	@echo "Packaging..."
	@if [ "$(HOST_OS)" = "windows" ]; then \
		cd $(BUILD_DIR) && zip $(APP_NAME)-$(VERSION)-$(HOST_OS)-$(HOST_ARCH).zip $(APP_NAME)-$(HOST_OS)-$(HOST_ARCH).exe; \
		echo "  → $(BUILD_DIR)/$(APP_NAME)-$(VERSION)-$(HOST_OS)-$(HOST_ARCH).zip"; \
	else \
		tar -czf $(BUILD_DIR)/$(APP_NAME)-$(VERSION)-$(HOST_OS)-$(HOST_ARCH).tar.gz -C $(BUILD_DIR) $(APP_NAME)-$(HOST_OS)-$(HOST_ARCH); \
		echo "  → $(BUILD_DIR)/$(APP_NAME)-$(VERSION)-$(HOST_OS)-$(HOST_ARCH).tar.gz"; \
	fi

# 打包为 macOS .app（图标正确显示在 Dock）
package-app:
	$(GOBIN)/fyne package -os darwin -icon logo.png -name XTX

# ============================================================
# 交叉编译（需要 Docker + fyne-cross）
# 安装: go install github.com/fyne-io/fyne-cross@latest
# ============================================================

# 全平台交叉编译
cross: clean cross-darwin cross-linux cross-windows
	@echo ""
	@echo "All cross-compilation done! Artifacts in $(BUILD_DIR)/fyne-cross/"

# macOS 交叉编译
cross-darwin:
	$(FYNE_CROSS) darwin -arch=amd64,arm64 -icon logo.png -app-id com.ixx.xtx -name XTX -output $(BUILD_DIR)
	@echo "  → darwin builds done"

# Linux 交叉编译
cross-linux:
	$(FYNE_CROSS) linux -arch=amd64,arm64 -icon logo.png -app-id com.ixx.xtx -name XTX -output $(BUILD_DIR)
	@echo "  → linux builds done"

# Windows 交叉编译
cross-windows:
	$(FYNE_CROSS) windows -arch=amd64 -icon logo.png -app-id com.ixx.xtx -name XTX -output $(BUILD_DIR)
	@echo "  → windows builds done"

# 全平台构建 + 打包压缩归档
package-all: cross
	@echo "Packaging archives..."
	@mkdir -p $(BUILD_DIR)/dist
	@for dir in $(BUILD_DIR)/fyne-cross/bin/*/; do \
		platform=$$(basename "$$dir"); \
		for bin in "$$dir"*; do \
			name=$$(basename "$$bin"); \
			pkg=$(APP_NAME)-$(VERSION)-$$platform; \
			case "$$platform" in \
				windows*) cd "$$dir" && zip ../../dist/$$pkg.zip "$$name" && cd -;; \
				*) tar -czf $(BUILD_DIR)/dist/$$pkg.tar.gz -C "$$dir" "$$name";; \
			esac; \
			echo "  → $(BUILD_DIR)/dist/$$pkg"; \
		done; \
	done
	@echo "Done! All packages in $(BUILD_DIR)/dist/"

clean:
	rm -rf $(BUILD_DIR)

# 单机双实例测试（开两个终端分别运行，共享 UDP 端口，不同 TCP 端口和数据目录）
# 终端1: make test-a
# 终端2: make test-b
test-a:
	go run . -nick "用户A" -tcp 9528 -udp 9527 -data /tmp/xtx-a

test-b:
	go run . -nick "用户B" -tcp 9529 -udp 9527 -data /tmp/xtx-b

test-clean:
	rm -rf /tmp/xtx-a /tmp/xtx-b

# 整理依赖
tidy:
	go mod tidy
