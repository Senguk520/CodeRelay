/// <reference types="vite/client" />

// 由 Vite define 注入的应用版本号（唯一来源为 package.json 的 version），
// 见 vite.config.ts。前端代码直接使用该全局常量即可，无需手动同步版本号。
declare const __APP_VERSION__: string;
