// Container Ops — small client helpers. No inline scripts (CSP: script-src 'self').
(function () {
  "use strict";

  var drawer = function () { return document.getElementById("drawer"); };
  var source = null; // active EventSource for followed logs

  function stopFollow() {
    if (source) { source.close(); source = null; }
  }

  function closeDrawer() {
    stopFollow();
    var d = drawer();
    if (d) d.innerHTML = "";
  }

  document.addEventListener("click", function (e) {
    var t = e.target.closest("[data-close-drawer]");
    if (t) { closeDrawer(); return; }
    var c = e.target.closest("[data-clear]");
    if (c) {
      var target = document.querySelector(c.getAttribute("data-clear"));
      if (target) target.innerHTML = "";
    }
  });

  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape" && drawer() && drawer().firstElementChild) closeDrawer();
  });

  // Stop any log stream before the drawer content is replaced.
  document.addEventListener("htmx:beforeSwap", function (e) {
    if (e.detail.target && e.detail.target.id === "drawer") stopFollow();
  });

  document.addEventListener("htmx:afterSwap", function (e) {
    if (!e.detail.target || e.detail.target.id !== "drawer") return;
    var panel = e.detail.target.querySelector(".drawer");
    if (panel) panel.focus();
    var view = e.detail.target.querySelector("[data-logview]");
    if (view) initLogs(view);
  });

  // ---- Log viewer ----
  var MAX_LINES = 5000;

  function initLogs(view) {
    var lines = view.querySelector("[data-lines]");
    var empty = view.querySelector("[data-empty]");
    var search = view.querySelector("[data-search]");
    var followBtn = view.querySelector("[data-follow]");
    var state = { level: "all", task: "all", q: "" };

    function visible(el) {
      if (state.level !== "all" && el.getAttribute("data-level") !== state.level) return false;
      if (state.task !== "all" && el.getAttribute("data-task") !== state.task) return false;
      if (state.q) {
        var m = el.querySelector(".lm");
        if (!m || m.textContent.toLowerCase().indexOf(state.q) < 0) return false;
      }
      return true;
    }
    function apply() {
      var any = false;
      lines.querySelectorAll(".ll").forEach(function (el) {
        var v = visible(el);
        el.hidden = !v;
        if (v) any = true;
      });
      empty.hidden = any;
    }
    function nearBottom() { return lines.scrollHeight - lines.scrollTop - lines.clientHeight < 40; }
    function toBottom() { lines.scrollTop = lines.scrollHeight; }

    view.querySelectorAll("[data-level]").forEach(function (b) {
      b.addEventListener("click", function () {
        state.level = b.getAttribute("data-level");
        view.querySelectorAll("[data-level]").forEach(function (x) { x.classList.toggle("on", x === b); });
        apply();
      });
    });
    view.querySelectorAll("button[data-task]").forEach(function (b) {
      b.addEventListener("click", function () {
        state.task = b.getAttribute("data-task");
        view.querySelectorAll("button[data-task]").forEach(function (x) { x.classList.toggle("on", x === b); });
        apply();
      });
    });
    if (search) search.addEventListener("input", function () { state.q = search.value.trim().toLowerCase(); apply(); });

    function addLine(l) {
      var row = document.createElement("div");
      row.className = "ll";
      row.setAttribute("data-level", l.Level || "");
      row.setAttribute("data-task", l.Task || "");
      [["lt", l.Time], ["lk", l.Task], ["lv lv-" + (l.Level || "").toLowerCase(), l.Level], ["lm", l.Msg]].forEach(function (p) {
        var s = document.createElement("span");
        s.className = p[0];
        s.textContent = p[1] || "";
        row.appendChild(s);
      });
      var stick = nearBottom();
      lines.insertBefore(row, empty);
      row.hidden = !visible(row);
      if (!row.hidden) empty.hidden = true;
      var all = lines.querySelectorAll(".ll");
      if (all.length > MAX_LINES) all[0].remove();
      if (stick) toBottom();
    }

    function setFollow(on) {
      followBtn.setAttribute("aria-pressed", on ? "true" : "false");
      followBtn.textContent = on ? "Following" : "Paused";
      if (!on) { stopFollow(); return; }
      stopFollow();
      source = new EventSource(view.getAttribute("data-stream"));
      source.onmessage = function (ev) {
        try { addLine(JSON.parse(ev.data)); } catch (_) { /* ignore malformed line */ }
      };
      source.addEventListener("end", function () { setFollow(false); });
      source.onerror = function () {
        // The browser retries automatically; show paused if the server refused.
        if (source && source.readyState === EventSource.CLOSED) setFollow(false);
      };
    }
    followBtn.addEventListener("click", function () {
      setFollow(followBtn.getAttribute("aria-pressed") !== "true");
    });

    view.querySelector("[data-download]").addEventListener("click", function () {
      var text = [];
      lines.querySelectorAll(".ll").forEach(function (el) {
        if (el.hidden) return;
        text.push(Array.prototype.map.call(el.children, function (c) { return c.textContent; }).join(" "));
      });
      var blob = new Blob([text.join("\n") + "\n"], { type: "text/plain" });
      var a = document.createElement("a");
      a.href = URL.createObjectURL(blob);
      a.download = "logs.txt";
      document.body.appendChild(a);
      a.click();
      setTimeout(function () { URL.revokeObjectURL(a.href); a.remove(); }, 1000);
    });

    apply();
    toBottom();
    setFollow(true);
  }

  // ---- Deploy form: "All" environments ----
  document.addEventListener("change", function (e) {
    var t = e.target;
    if (t.matches("[data-env-all]")) {
      document.querySelectorAll("[data-env]").forEach(function (c) { c.checked = t.checked; });
      return;
    }
    if (t.matches("[data-env]")) {
      var all = document.querySelector("[data-env-all]");
      var boxes = document.querySelectorAll("[data-env]");
      if (all) all.checked = Array.prototype.every.call(boxes, function (c) { return c.checked; });
    }
  });
})();
