# GoSeek for Windows 构建脚本。
#
# 与上游 macOS 版的差异：
#   - 所有发布产物以 GOOS=windows 交叉编译（任意平台可构建，纯 Go 无 CGO）；
#   - goseek-browser.exe（内置 browser 插件的 bin 工具）必须在主程序之前、
#     以 Windows 为目标编译并放进 cmd/goseek/plugins/browser/bin/ ——
#     它会被 go:embed 打进 goseek.exe，运行时自动解包到数据目录；
#   - 额外构建 mock-provider.exe（离线自检用），随安装包分发。
#
# 常用：
#   make release   构建三件套 + mock（产物在 backend/release/）
#   make smoke     （macOS/Linux）对当前源码跑离线自检
#   make test      后端全量测试
#   make vet-win   GOOS=windows 静态检查
#   make clean

GOBUILD_WIN = GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath

.PHONY: release front smoke test vet-win clean

front:
	cd front && bun install && bun run build
	rm -rf backend/cmd/goseek/dist
	mkdir -p backend/cmd/goseek/dist
	cp -r front/dist/* backend/cmd/goseek/dist/

release: front
	cd backend && $(GOBUILD_WIN) -o cmd/goseek/plugins/browser/bin/goseek-browser.exe ./cmd/goseek-browser
	cd backend && $(GOBUILD_WIN) -ldflags "-s -w" -o release/goseek.exe ./cmd/goseek
	cd backend && $(GOBUILD_WIN) -ldflags "-s -w" -o release/GoSeek-win-launcher.exe ./cmd/goseek-win-launcher
	cd backend && $(GOBUILD_WIN) -o release/mock-provider.exe ./cmd/mock-provider
	cp backend/cmd/goseek/plugins/browser/bin/goseek-browser.exe backend/release/goseek-browser.exe
	@echo "release 产物就绪: backend/release/"

smoke:
	scripts/smoke-test.sh

test:
	cd backend && go test ./...

vet-win:
	cd backend && GOOS=windows GOARCH=amd64 go vet ./...

clean:
	rm -rf backend/release backend/cmd/goseek/dist front/dist
	rm -f backend/cmd/goseek/plugins/browser/bin/goseek-browser.exe
