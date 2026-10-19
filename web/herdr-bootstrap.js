const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.10-412-12f227535c524957/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
