// Applies the saved light/dark choice before the page paints.
try { var t = localStorage.getItem('hd_theme'); if (t === 'light' || t === 'dark') document.documentElement.setAttribute('data-theme', t); } catch (e) { }
