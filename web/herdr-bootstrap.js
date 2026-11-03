const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.24.0-486-efc9836c01f76236/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
