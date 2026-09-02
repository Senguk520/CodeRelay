import type { ThemeMode } from './types';

const PREFERENCES_KEY = 'coderelay-preferences';

export type EffectiveTheme = 'light' | 'dark';

const DARK_META_COLOR = '#1e1f1c';
const LIGHT_META_COLOR = '#f4f4f3';

function isThemeMode(value: unknown): value is ThemeMode {
  return value === 'light' || value === 'dark' || value === 'system';
}

/** 从本地偏好中读取用户选择的主题模式，缺省返回跟随系统。 */
export function getStoredTheme(): ThemeMode {
  try {
    const raw = localStorage.getItem(PREFERENCES_KEY);
    if (!raw) return 'system';
    const theme = (JSON.parse(raw) as { theme?: unknown }).theme;
    return isThemeMode(theme) ? theme : 'system';
  } catch {
    return 'system';
  }
}

/** 读取操作系统当前的深浅色偏好。 */
export function getSystemTheme(): EffectiveTheme {
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

function resolveTheme(mode: ThemeMode): EffectiveTheme {
  return mode === 'system' ? getSystemTheme() : mode;
}

function syncMeta(theme: EffectiveTheme): void {
  const meta = document.querySelector<HTMLMetaElement>('meta[name="theme-color"]');
  if (meta) meta.content = theme === 'dark' ? DARK_META_COLOR : LIGHT_META_COLOR;
}

let mediaQuery: MediaQueryList | null = null;
let mediaListener: ((event: MediaQueryListEvent) => void) | null = null;

function detachMediaListener(): void {
  if (mediaQuery && mediaListener) {
    mediaQuery.removeEventListener('change', mediaListener);
  }
  mediaQuery = null;
  mediaListener = null;
}

/**
 * 应用主题：设置 <html data-theme> 并同步 meta theme-color。
 * 当 mode 为 system 时监听系统深浅色变化，实时跟随。
 */
export function applyTheme(mode: ThemeMode): void {
  const theme = resolveTheme(mode);
  document.documentElement.dataset.theme = theme;
  syncMeta(theme);

  detachMediaListener();
  if (mode === 'system') {
    mediaQuery = window.matchMedia('(prefers-color-scheme: dark)');
    mediaListener = () => {
      const next = getSystemTheme();
      document.documentElement.dataset.theme = next;
      syncMeta(next);
    };
    mediaQuery.addEventListener('change', mediaListener);
  }
}
