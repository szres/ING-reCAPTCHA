// Mini App verification frontend.
//
// Talks to the bot's HTTP server at /v/init and /v/answer. Every request
// includes the Telegram WebApp initData blob; the server validates the
// HMAC plus user_id binding, so this frontend code is fully untrusted
// from the server's perspective — it's UI only.

(function () {
  const tg = window.Telegram && window.Telegram.WebApp;
  if (!tg) {
    showError("This page must be opened from Telegram.");
    return;
  }
  tg.ready();
  tg.expand();
  // Wire BackButton explicitly — Telegram doesn't bind it by default and
  // the browser's history stack is unreliable inside the web view.
  if (tg.BackButton) {
    tg.BackButton.onClick(() => tg.close());
  }

  const nonce = new URLSearchParams(location.search).get("n");
  if (!nonce) {
    showError("Missing session token.");
    return;
  }

  const $progress = document.getElementById("progress");
  const $progressbar = document.getElementById("progressbar");
  const $progressbarFill = document.getElementById("progressbar-fill");
  const $prompt = document.getElementById("prompt");
  const $img = document.getElementById("challenge-image");
  const $options = document.getElementById("options");
  const $secLoading = document.getElementById("state-loading");
  const $secChallenge = document.getElementById("challenge");
  const $secPassed = document.getElementById("state-passed");
  const $secFailed = document.getElementById("state-failed");
  const $secError = document.getElementById("state-error");
  const $errorDetail = document.getElementById("error-detail");

  let totalSteps = 0;
  let currentStep = 0;
  let options = [];

  // Theme / viewport awareness. Telegram's web view changes both during
  // expansion and when the soft keyboard appears.
  function applyTheme() {
    document.documentElement.style.setProperty(
      "color-scheme",
      tg.colorScheme === "dark" ? "dark" : "light"
    );
  }
  applyTheme();
  tg.onEvent("themeChanged", applyTheme);
  tg.onEvent("viewportChanged", () => {
    // Use Telegram-provided viewport height instead of 100vh, which lies
    // on mobile web views.
    document.documentElement.style.setProperty(
      "--tg-viewport-height", tg.viewportHeight + "px");
  });

  init().catch((e) => showError(e && e.message ? e.message : "Init failed."));

  async function init() {
    const body = JSON.stringify({ nonce, init_data: tg.initData });
    const resp = await fetch("/v/init", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body,
    });
    if (!resp.ok) {
      throw new Error(`Server rejected the session (HTTP ${resp.status}).`);
    }
    const data = await resp.json();
    totalSteps = data.total_steps;
    currentStep = data.current_step;
    options = data.options;
    $img.src = data.image_url;
    renderStep();
    show($secChallenge);
  }

  function renderStep() {
    $progress.textContent = `${currentStep + 1} / ${totalSteps}`;
    setProgress((currentStep + 1) / totalSteps);
    $prompt.textContent = `Tap the label that matches picture ${currentStep + 1}.`;
    $options.innerHTML = "";
    options.forEach((opt) => {
      const b = document.createElement("button");
      b.type = "button";
      b.textContent = opt;
      b.addEventListener("click", () => submitAnswer(opt, b));
      $options.appendChild(b);
    });
  }

  async function submitAnswer(label, btn) {
    // Disable all buttons during in-flight submission to avoid double-taps.
    Array.from($options.children).forEach((el) => (el.disabled = true));
    btn.classList.add("primary");

    let data;
    try {
      const resp = await fetch("/v/answer", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          nonce,
          init_data: tg.initData,
          step: currentStep,
          label,
        }),
      });
      if (!resp.ok) {
        throw new Error(`Answer rejected (HTTP ${resp.status}).`);
      }
      data = await resp.json();
    } catch (e) {
      showError(e.message);
      return;
    }

    if (data.status === "next") {
      currentStep = data.current_step;
      renderStep();
    } else if (data.status === "passed") {
      setProgress(1);
      show($secPassed);
      // Tell Telegram the WebApp finished; the client will close itself
      // after the bot's approveChatJoinRequestQuery lands and the user is
      // admitted to the group.
      setTimeout(() => tg.close(), 1200);
    } else {
      setProgress(1);
      show($secFailed);
      setTimeout(() => tg.close(), 1800);
    }
  }

  function setProgress(ratio) {
    const pct = Math.max(0, Math.min(1, ratio)) * 100;
    $progressbarFill.style.width = pct + "%";
    $progressbar.setAttribute("aria-valuenow", Math.round(pct));
  }

  function show(node) {
    [$secLoading, $secChallenge, $secPassed, $secFailed, $secError].forEach(
      (el) => (el.hidden = el !== node)
    );
  }

  function showError(msg) {
    $errorDetail.textContent = msg || "Unknown error.";
    show($secError);
  }
})();
