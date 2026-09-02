// 单一版本源同步脚本
//
// 版本号唯一来源是 package.json 的 "version" 字段。本脚本将其同步到：
//   - src-tauri/tauri.conf.json 的 version（Tauri 打包版本）
//   - src-tauri/Cargo.toml 的 version（Rust crate 版本）
// 前端展示的版本号由 vite.config.ts 通过 define 注入，无需在此处理；
// Cargo.lock 由 cargo 在下次编译时自动更新。
//
// 用法：node scripts/sync-version.mjs
import { readFileSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');

const pkg = JSON.parse(readFileSync(resolve(root, 'package.json'), 'utf-8'));
const version = pkg.version;
if (!/^\d+\.\d+\.\d+/.test(version)) {
  throw new Error(`package.json 的 version 非法: ${version}`);
}

// 1. 同步 tauri.conf.json
const tauriConfPath = resolve(root, 'src-tauri/tauri.conf.json');
const tauriConf = JSON.parse(readFileSync(tauriConfPath, 'utf-8'));
if (tauriConf.version !== version) {
  tauriConf.version = version;
  writeFileSync(tauriConfPath, JSON.stringify(tauriConf, null, 2) + '\n');
  console.log(`[sync-version] tauri.conf.json -> ${version}`);
} else {
  console.log(`[sync-version] tauri.conf.json 已是最新 (${version})`);
}

// 2. 同步 Cargo.toml（仅替换 [package] 段顶层首个 version 行）
const cargoPath = resolve(root, 'src-tauri/Cargo.toml');
let cargo = readFileSync(cargoPath, 'utf-8');
if (/^version\s*=\s*"[^"]*"$/m.test(cargo)) {
  cargo = cargo.replace(/^version\s*=\s*"[^"]*"$/m, `version = "${version}"`);
  writeFileSync(cargoPath, cargo);
  console.log(`[sync-version] Cargo.toml -> ${version}`);
} else {
  console.warn('[sync-version] 未在 Cargo.toml 中找到 [package] 的 version 行');
}

console.log(`[sync-version] 完成，版本号统一为 ${version}`);
