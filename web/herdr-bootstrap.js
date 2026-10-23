const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.11-418-8f7678900ed7d292/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
