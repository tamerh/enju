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

  // Minimal, dependency-free YAML highlighter for read-only
  // <pre data-lang="yaml"> blocks (the run page's Workflow
  // YAML). Deliberately conservative: a line that doesn't match
  // is emitted escaped + uncolored — never wrong text, just
  // less color. textContent is preserved (escaped entities
  // decode back), so the auto-attached copy button still yields
  // the exact original. Idempotent via data-hl.
  function esc(s) {
    return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  }
  function hlValue(v) {
    var t = v.trim();
    if (t === '') return esc(v);
    // quoted string
    if (/^(".*"|'.*')$/.test(t)) {
      return esc(v).replace(esc(t), '<span class="y-str">' + esc(t) + '</span>');
    }
    if (/^(true|false|null|~|-?\d+(\.\d+)?)$/.test(t)) {
      return esc(v).replace(esc(t), '<span class="y-num">' + esc(t) + '</span>');
    }
    return esc(v);
  }
  function hlLine(line) {
    // Split off a trailing comment (# at line start or after
    // whitespace, not inside quotes).
    var inS = false, inD = false, ci = -1;
    for (var i = 0; i < line.length; i++) {
      var c = line[i];
      if (c === "'" && !inD) inS = !inS;
      else if (c === '"' && !inS) inD = !inD;
      else if (c === '#' && !inS && !inD && (i === 0 || /\s/.test(line[i - 1]))) { ci = i; break; }
    }
    var code = ci >= 0 ? line.slice(0, ci) : line;
    var comment = ci >= 0 ? line.slice(ci) : '';
    var out;
    // key:  (optionally after indentation + a list dash)
    var m = code.match(/^(\s*(?:-\s+)?)([^\s#'":][^:#]*?)(:)(\s.*|)$/);
    if (m) {
      out = esc(m[1]) +
        '<span class="y-key">' + esc(m[2]) + '</span>' +
        '<span class="y-punct">' + esc(m[3]) + '</span>' +
        hlValue(m[4]);
    } else {
      out = esc(code);
    }
    if (comment) out += '<span class="y-comment">' + esc(comment) + '</span>';
    return out;
  }
  function highlightYAML() {
    document.querySelectorAll('pre[data-lang="yaml"]:not([data-hl="1"])').forEach(function (pre) {
      pre.dataset.hl = '1';
      var lines = pre.textContent.split('\n');
      pre.innerHTML = lines.map(hlLine).join('\n');
    });
  }

  function init() {
    prefetchIcons();
    attachAll();
    initThemeToggle();
    highlightYAML();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
  document.addEventListener('htmx:afterSwap', function () {
    attachAll();
    highlightYAML();
  });
})();
