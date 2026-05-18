// Tiny client-side niceties for the Enju web UI.
//
// Currently:
//   - Auto-attach a copy button to every <pre> block on the
//     page (skip <pre class="mermaid"> — mermaid replaces its
//     content with SVG, copy doesn't make sense there).
//
// Icons come from /static/icons/*.svg — Lucide files vendored
// at static/icons/. Fetched once on first need, cached as raw
// SVG strings, inlined into the DOM so the SVG inherits text
// color via stroke="currentColor".
//
// Re-runs after every HTMX swap so partial-page navigation
// also gets the buttons. Safe to call repeatedly — guarded
// by a per-element flag.

(function () {
  'use strict';

  // Icon cache: name → SVG string. Populated lazily on first
  // use. Prefetch on DOMContentLoaded so the first copy click
  // doesn't block on a fetch.
  const ICONS = {};
  const ICON_NAMES = ['copy', 'check'];

  async function loadIcon(name) {
    if (ICONS[name]) return ICONS[name];
    const resp = await fetch('/static/icons/' + name + '.svg');
    if (!resp.ok) throw new Error('icon fetch failed: ' + name);
    ICONS[name] = (await resp.text()).trim();
    return ICONS[name];
  }

  function prefetchIcons() {
    ICON_NAMES.forEach((n) => { loadIcon(n).catch(() => {}); });
  }

  function attachCopyButton(pre) {
    if (pre.dataset.copyAttached === '1') return;
    pre.dataset.copyAttached = '1';

    // Wrap pre in a positioning container so the button can
    // be absolutely positioned at top-right.
    const wrap = document.createElement('div');
    wrap.className = 'copy-wrap';
    pre.parentNode.insertBefore(wrap, pre);
    wrap.appendChild(pre);

    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'copy-btn';
    btn.title = 'Copy to clipboard';
    btn.innerHTML = ICONS.copy || '';
    wrap.appendChild(btn);

    // Late-fill if the icon hadn't finished loading by the
    // time we created the button.
    if (!ICONS.copy) loadIcon('copy').then((svg) => { btn.innerHTML = svg; }).catch(() => {});

    btn.addEventListener('click', async function (ev) {
      ev.preventDefault();
      try {
        await navigator.clipboard.writeText(pre.innerText);
        const check = await loadIcon('check').catch(() => '');
        btn.innerHTML = check || btn.innerHTML;
        btn.classList.add('copy-btn-ok');
        setTimeout(() => {
          btn.innerHTML = ICONS.copy || btn.innerHTML;
          btn.classList.remove('copy-btn-ok');
        }, 1500);
      } catch (err) {
        // Clipboard API denied / unavailable — fall back to
        // selecting the text so the user can copy manually.
        const range = document.createRange();
        range.selectNodeContents(pre);
        const sel = window.getSelection();
        sel.removeAllRanges();
        sel.addRange(range);
        btn.title = 'Copy failed — text selected, use Ctrl-C';
      }
    });
  }

  function attachAll() {
    document.querySelectorAll('pre:not(.mermaid):not([data-copy-attached="1"])').forEach(attachCopyButton);
  }

  // Light/dark toggle. Light is the default (no attribute);
  // dark sets data-theme="dark" on <html> and persists to
  // localStorage. The no-flash head script in layout.html
  // applies the saved choice before paint; this only wires the
  // button + keeps its glyph in sync. The button lives in the
  // topnav (outside #main), so it survives HTMX swaps — wire
  // once.
  function currentTheme() {
    return document.documentElement.getAttribute('data-theme') === 'dark'
      ? 'dark' : 'light';
  }
  function syncToggleGlyph(btn) {
    // Show the action, not the state: 🌙 = "switch to dark",
    // ☀️ = "switch to light".
    btn.textContent = currentTheme() === 'dark' ? '☀️' : '🌙';
  }
  function initThemeToggle() {
    const btn = document.getElementById('theme-toggle');
    if (!btn || btn.dataset.wired === '1') return;
    btn.dataset.wired = '1';
    syncToggleGlyph(btn);
    btn.addEventListener('click', function () {
      const next = currentTheme() === 'dark' ? 'light' : 'dark';
      if (next === 'dark') {
        document.documentElement.setAttribute('data-theme', 'dark');
      } else {
        document.documentElement.removeAttribute('data-theme');
      }
      try { localStorage.setItem('enjuTheme', next); } catch (e) {}
      syncToggleGlyph(btn);
    });
  }

  // Syntax highlighting via vendored highlight.js (static/
  // highlight.min.js + hljs.css). Server emits the read-only
  // file blocks as <pre data-lang="..."> (the data-lang tokens
  // py/sh/ts/go/yaml/json/… are hljs language aliases, passed
  // straight through as a language-* class). hljs.highlightElement
  // preserves textContent, so the auto-attached copy button
  // still yields the exact original. Idempotent via data-hl;
  // graceful no-op if the script failed to load.
  var hljsConfigured = false;
  function highlightAll() {
    if (!window.hljs) return;
    if (!hljsConfigured) {
      window.hljs.configure({ ignoreUnescapedHTML: true });
      hljsConfigured = true;
    }
    document.querySelectorAll('pre[data-lang]:not([data-hl="1"])').forEach(function (pre) {
      var lang = pre.getAttribute('data-lang');
      if (!lang) return;
      pre.dataset.hl = '1';
      pre.classList.add('language-' + lang);
      try {
        window.hljs.highlightElement(pre);
      } catch (e) { /* leave plain on any grammar error */ }
    });
  }

  function init() {
    prefetchIcons();
    attachAll();
    initThemeToggle();
    highlightAll();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
  document.addEventListener('htmx:afterSwap', function () {
    attachAll();
    highlightAll();
  });
})();
