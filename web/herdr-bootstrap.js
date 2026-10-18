const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.10-411-f3727f4e66450076/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
