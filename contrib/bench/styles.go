package main

// reportCSS defines the validated reference palette as CSS custom properties
// (light + dark, both selected) plus report layout and chart-element classes.
// Chart SVGs reference these vars by role, so a single theme swap recolors all.
const reportCSS = `
:root, .viz {
  --surface-1:#fcfcfb; --page:#f9f9f7;
  --ink:#0b0b0b; --ink2:#52514e; --muted:#898781;
  --grid:#e1e0d9; --axis:#c3c2b7; --border:rgba(11,11,11,0.10);
  --rt-systemd:#2a78d6; --rt-runc:#eb6834;
  --cat-1:#2a78d6; --cat-2:#008300; --cat-3:#e87ba4; --cat-4:#eda100; --cat-5:#1baf7a; --cat-6:#eb6834;
  color-scheme:light;
}
@media (prefers-color-scheme: dark) {
  :root:where(:not([data-theme="light"])) , :root:where(:not([data-theme="light"])) .viz {
    --surface-1:#1a1a19; --page:#0d0d0d;
    --ink:#ffffff; --ink2:#c3c2b7; --muted:#898781;
    --grid:#2c2c2a; --axis:#383835; --border:rgba(255,255,255,0.10);
    --rt-systemd:#3987e5; --rt-runc:#d95926;
    --cat-1:#3987e5; --cat-2:#008300; --cat-3:#d55181; --cat-4:#c98500; --cat-5:#199e70; --cat-6:#d95926;
    color-scheme:dark;
  }
}
:root[data-theme="dark"], :root[data-theme="dark"] .viz {
  --surface-1:#1a1a19; --page:#0d0d0d;
  --ink:#ffffff; --ink2:#c3c2b7; --muted:#898781;
  --grid:#2c2c2a; --axis:#383835; --border:rgba(255,255,255,0.10);
  --rt-systemd:#3987e5; --rt-runc:#d95926;
  --cat-1:#3987e5; --cat-2:#008300; --cat-3:#d55181; --cat-4:#c98500; --cat-5:#199e70; --cat-6:#d95926;
  color-scheme:dark;
}

* { box-sizing:border-box; }
body { margin:0; background:var(--page); color:var(--ink);
  font-family: system-ui, -apple-system, "Segoe UI", sans-serif; font-size:14px; line-height:1.5; }
h1 { font-size:22px; margin:0; }
h2 { font-size:16px; margin:28px 20px 8px; }
h3 { font-size:14px; margin:0 0 2px; }
.sub { color:var(--ink2); margin:2px 0; }
.top { display:flex; align-items:center; justify-content:space-between;
  padding:20px; border-bottom:1px solid var(--border); }
.toggle { background:var(--surface-1); color:var(--ink); border:1px solid var(--border);
  border-radius:8px; padding:8px 12px; cursor:pointer; font:inherit; }
.prov dl { display:grid; grid-template-columns:repeat(auto-fill,minmax(200px,1fr));
  gap:2px 16px; margin:0 20px; }
.prov dt { color:var(--muted); font-size:12px; }
.prov dd { margin:0 0 6px; font-variant-numeric:tabular-nums; }
.charts { display:grid; grid-template-columns:repeat(auto-fill,minmax(380px,1fr));
  gap:16px; padding:8px 20px 20px; }
.card { margin:0; background:var(--surface-1); border:1px solid var(--border);
  border-radius:12px; padding:14px 14px 4px; }
.card figcaption p { color:var(--ink2); font-size:12px; margin:0 0 6px; }
.chart { width:100%; height:auto; display:block; }
.chart .grid { stroke:var(--grid); stroke-width:1; }
.chart .axis { stroke:var(--axis); stroke-width:1; }
.chart .xlabel { fill:var(--ink2); font-size:11px; }
.chart .ytick { fill:var(--muted); font-size:10px; font-variant-numeric:tabular-nums; }
.chart .axislabel { fill:var(--muted); font-size:10px; }
.chart .legend { fill:var(--ink2); font-size:11px; }
.chart .vlabel { fill:var(--ink2); font-size:9px; font-variant-numeric:tabular-nums; }
.tablewrap { padding:0 20px 20px; overflow-x:auto; }
table { border-collapse:collapse; width:100%; font-size:12px; }
th, td { text-align:right; padding:5px 8px; border-bottom:1px solid var(--border);
  font-variant-numeric:tabular-nums; white-space:nowrap; }
th:first-child, td:first-child, th:nth-child(2), td:nth-child(2), th:nth-child(3), td:nth-child(3) { text-align:left; }
thead th { color:var(--muted); font-weight:600; border-bottom:1px solid var(--axis); }
.caveats { padding:0 20px 40px; color:var(--ink2); max-width:70ch; }
.caveats li { margin:6px 0; }
code { background:var(--surface-1); border:1px solid var(--border); border-radius:4px; padding:1px 5px; }
`

// themeJS toggles between OS-follow, forced light, and forced dark by setting
// data-theme on <html>.
const themeJS = `
(function(){
  var btn = document.getElementById('themeToggle');
  if(!btn) return;
  var order = [null, 'light', 'dark'];
  var i = 0;
  btn.addEventListener('click', function(){
    i = (i+1) % order.length;
    if(order[i]) document.documentElement.setAttribute('data-theme', order[i]);
    else document.documentElement.removeAttribute('data-theme');
    btn.textContent = '◐ ' + (order[i] || 'auto');
  });
})();
`
