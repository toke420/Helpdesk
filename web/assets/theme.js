// Applies the saved light/dark choice before the page paints, and keeps the
// browser's theme colour (address bar, task switcher) matching the page.
(function () {
  var root = document.documentElement;
  try { var t = localStorage.getItem('hd_theme'); if (t === 'light' || t === 'dark') root.setAttribute('data-theme', t); } catch (e) { }
  var BG = { light: '#f4f5f7', dark: '#0e1014' };
  function sync() {
    var t = root.getAttribute('data-theme');
    var metas = document.querySelectorAll('meta[name="theme-color"]');
    for (var i = 0; i < metas.length; i++) {
      var m = metas[i];
      var own = m.getAttribute('data-scheme');
      if (!own) { own = /dark/.test(m.media) ? 'dark' : 'light'; m.setAttribute('data-scheme', own); }
      // A manual choice wins over the system setting: point both tags at it.
      m.content = BG[t || own];
    }
  }
  sync();
  if (window.MutationObserver) new MutationObserver(sync).observe(root, { attributes: true, attributeFilter: ['data-theme'] });
})();
