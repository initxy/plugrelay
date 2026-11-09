// Applies the saved theme before first paint so dark mode never flashes light.
try {
  var t = localStorage.getItem("pr.theme");
  var dark = t === "dark" || (t !== "light" && matchMedia("(prefers-color-scheme: dark)").matches);
  document.documentElement.classList.toggle("dark", dark);
} catch (e) {}
