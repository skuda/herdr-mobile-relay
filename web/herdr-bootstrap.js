const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.13-421-820f52631a4b1918/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
