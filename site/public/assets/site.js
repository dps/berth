// Copy buttons on code blocks, and the docs menu on small screens.
for (const pre of document.querySelectorAll("pre:not(.no-copy)")) {
  const wrap = document.createElement("div");
  wrap.className = "codeblock";
  pre.replaceWith(wrap);
  wrap.append(pre);
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "copy";
  btn.textContent = "Copy";
  btn.addEventListener("click", async () => {
    // Drop the comments after a #, so a pasted block runs as is.
    const text = pre.innerText.replace(/[ \t]+#(?![!{]).*$/gm, "").replace(/\s+$/, "") + "\n";
    try {
      await navigator.clipboard.writeText(text);
      btn.textContent = "Copied";
    } catch {
      btn.textContent = "Select and copy";
    }
    btn.classList.add("done");
    setTimeout(() => { btn.textContent = "Copy"; btn.classList.remove("done"); }, 1600);
  });
  wrap.append(btn);
}

const menu = document.querySelector(".menu");
const sidebar = document.getElementById("sidebar");
if (menu && sidebar) {
  menu.addEventListener("click", () => {
    const open = sidebar.classList.toggle("open");
    menu.setAttribute("aria-expanded", String(open));
    menu.textContent = open ? "Close" : "Contents";
  });
  // Keep the current page in view in a long sidebar, without scrolling the page.
  const cur = sidebar.querySelector('[aria-current="page"]');
  if (cur && cur.offsetTop > sidebar.clientHeight - 80) sidebar.scrollTop = cur.offsetTop - sidebar.clientHeight / 2;
}
