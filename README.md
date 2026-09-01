# CodeRelay

CodeRelay 是一个 Windows 桌面管理工具，用于管理 CodeBuddy 中国站账号池并运行本地 OpenAI 兼容反代服务。

## 当前状态

- 前端：React + TypeScript + Vite
- 桌面端：Tauri 2 + Rust
- 反代：Go sidecar，使用嵌入的 CLIProxyAPI v7.2.140 和 CodeBuddy 补丁
- 默认服务：`127.0.0.1:11435`
- 默认启动行为：应用打开时服务保持停止，必须由用户手动启动
- 凭据：账号元数据与 Token 分离保存，Token 不写入普通状态文件

## 开发命令

```powershell
npm install
npm run typecheck
npm run build
npm run build:sidecar
cargo check --manifest-path src-tauri/Cargo.toml
go test ./...
```

`npm run build:sidecar` 会根据 Rust target triple 生成
`sidecars/coderelay-proxy/bin/coderelay-proxy-<target>.exe`。

## 账号和服务配置

首次使用时，在“账号池”中通过浏览器认证、手动 Token 或配置文件导入添加 CodeBuddy 中国站账号，然后创建至少一个以 `sk-` 开头的 API Key，最后在“服务配置”中手动启动反代服务。

sidecar 由 Tauri 生成 `config.json`、`manifest.json` 和临时 `auths/` 目录。sidecar 仅通过标准输出发送结构化生命周期和请求事件，不输出 Token 内容。

## 第三方组件

CLIProxyAPI 的许可和归属信息见 `NOTICE.md` 以及
`sidecars/coderelay-proxy/third_party/CLIProxyAPI/LICENSE`。
