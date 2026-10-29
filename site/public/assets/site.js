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

// The Mac mini's power light spells out "BERTH IS A DEAD SIMPLE PAAS" in
// Morse, over and over: a dot is one unit on, a dash three, with one unit
// between them, three between letters and seven between words.
const led = document.querySelector(".led");
// It's a pinprick of light that doesn't move, so it blinks even with
// reduced motion on.
if (led) {
  const morse = {
    A: ".-", B: "-...", D: "-..", E: ".", H: "....", I: "..", L: ".-..",
    M: "--", P: ".--.", R: ".-.", S: "...", T: "-",
  };
  const unit = 150; // ms, about 8 words a minute
  const steps = []; // [on?, units]
  for (const word of "BERTH IS A DEAD SIMPLE PAAS".split(" ")) {
    for (const letter of word) {
      for (const sign of morse[letter]) steps.push([true, sign === "." ? 1 : 3], [false, 1]);
      steps[steps.length - 1][1] = 3;
    }
    steps[steps.length - 1][1] = 7;
  }
  steps[steps.length - 1][1] = 20; // a pause before it starts again
  let i = 0;
  const next = () => {
    const [on, units] = steps[i];
    led.classList.toggle("on", on);
    i = (i + 1) % steps.length;
    setTimeout(next, units * unit);
  };
  next();
}
