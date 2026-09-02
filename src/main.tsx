import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import './styles.css';
import { App } from './App';
import { applyTheme, getStoredTheme } from './theme';

// 在 React 挂载前同步应用主题，避免首屏闪烁。
applyTheme(getStoredTheme());

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);