// Applied before first paint so a remembered theme doesn't flash.
try {
  const t = localStorage.getItem("berth-theme");
  if (t === "light" || t === "dark") document.documentElement.dataset.theme = t;
} catch {}
