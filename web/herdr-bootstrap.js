const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.11-417-16d387dd7cc7e536/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
