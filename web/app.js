"use strict";

// Couleurs d'écuries connues ; les autres reçoivent une couleur calculée.
const TEAM_COLORS = {
  red_bull: "#3671c6", ferrari: "#e8002d", mercedes: "#27f4d2", mclaren: "#ff8000",
  aston_martin: "#229971", alpine: "#ff87bc", williams: "#64c4ff", rb: "#6692ff",
  alphatauri: "#5e8faa", sauber: "#52e252", haas: "#b6babd", audi: "#f50537",
  cadillac: "#9aa0a6", alfa: "#c92d4b", renault: "#fff500",
};

function teamColor(id) {
  if (TEAM_COLORS[id]) return TEAM_COLORS[id];
  let h = 0;
  for (const c of id) h = (h * 31 + c.charCodeAt(0)) % 360;
  return `hsl(${h} 65% 58%)`;
}

const $ = (sel) => document.querySelector(sel);

// Création d'éléments sans innerHTML (les noms viennent d'une API externe).
function el(tag, props = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (k === "class") node.className = v;
    else if (k === "style") node.style.cssText = v;
    else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else node[k] = v;
  }
  for (const c of children) node.append(c);
  return node;
}

const state = { meta: null, picks: new Map() }; // picks : driverId -> constructorId choisi

function setStatus(msg, isError = false) {
  const s = $("#status");
  s.textContent = msg;
  s.hidden = !msg;
  s.classList.toggle("error", isError);
}

async function api(path, options) {
  const res = await fetch(path, options);
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `Erreur ${res.status}`);
  return data;
}

function selectedEntries() {
  return [...state.picks].map(([driverId, constructorId]) => ({ driverId, constructorId }));
}

function updateCount() {
  const n = state.picks.size;
  $("#count").textContent = `${n} pilote${n > 1 ? "s" : ""} sélectionné${n > 1 ? "s" : ""}`;
  $("#go").disabled = n < 2;
}

function renderDrivers() {
  const box = $("#drivers");
  box.replaceChildren();
  for (const d of state.meta.drivers) {
    const card = el("div", { class: "card", style: `--team:${teamColor(d.constructorId)}` });

    const check = el("input", { type: "checkbox", id: `pick-${d.id}` });
    const car = el("select", { "aria-label": `Voiture de ${d.name}` });
    for (const c of state.meta.constructors) {
      car.append(el("option", { value: c.id, textContent: c.name, selected: c.id === d.constructorId }));
    }

    const apply = () => {
      card.classList.toggle("on", check.checked);
      car.disabled = !check.checked;
      if (check.checked) state.picks.set(d.id, car.value);
      else state.picks.delete(d.id);
      card.style.setProperty("--team", teamColor(car.value));
      updateCount();
    };
    check.addEventListener("change", apply);
    car.addEventListener("change", apply);

    card.append(
      el("label", { class: "pick" }, check,
        el("span", { class: "code", textContent: d.code || "—" }),
        el("span", { class: "name", textContent: d.name })),
      car,
    );
    card._set = (on, resetCar) => {
      check.checked = on;
      if (resetCar) car.value = d.constructorId;
      apply();
    };
    box.append(card);
  }
  box.querySelectorAll(".card").forEach((c) => c._set(true, true));
}

function forEachCard(fn) {
  document.querySelectorAll("#drivers .card").forEach(fn);
}

const fmt = (x, digits = 1) => (x == null ? "—" : x.toFixed(digits));
const pct = (x) => `${Math.round(x * 100)} %`;
const CONF = { high: "fiabilité élevée", medium: "fiabilité moyenne", low: "peu de données" };

function detailText(p) {
  const parts = [
    `forme pilote P${fmt(p.formPos)} (${p.formRaces} courses)`,
    `voiture P${fmt(p.carForm)}`,
    p.circuitRaces ? `sur ce circuit P${fmt(p.circuitPos)} (${p.circuitRaces} course${p.circuitRaces > 1 ? "s" : ""})` : "jamais couru ici récemment",
    `abandons ${pct(p.dnfRate)}`,
  ];
  return parts.join(" · ");
}

function renderResult(data) {
  const preds = data.predictions;
  $("#context").textContent =
    `${data.circuit.name} — ${data.circuit.locality}, ${data.circuit.country}. ` +
    `Basé sur la forme ${data.season - 1}–${data.season} et l'historique du circuit (${data.history}).`;

  const podium = $("#podium");
  podium.replaceChildren();
  preds.slice(0, 3).forEach((p, i) => {
    podium.append(
      el("div", { class: `pod p${i + 1}`, style: `--team:${teamColor(p.constructorId)}` },
        el("div", { class: "pos", textContent: `P${p.rank}` }),
        el("div", {},
          el("div", { class: "nm", textContent: p.driverName }),
          el("div", { class: "car", textContent: p.constructorName }),
          el("div", { class: "pr", textContent: `Victoire ≈ ${pct(p.winProb)}` })),
      ),
    );
  });

  const table = $("#table");
  table.replaceChildren();
  const maxProb = Math.max(...preds.map((p) => p.winProb)) || 1;
  for (const p of preds) {
    const badge = el("span", { class: `badge ${p.confidence}`, textContent: CONF[p.confidence] });
    table.append(
      el("li", { class: "row", style: `--team:${teamColor(p.constructorId)}` },
        el("span", { class: "rk", textContent: p.rank }),
        el("span", { class: "who" }, p.driverName, el("small", { textContent: p.constructorName })),
        el("span", { class: "sc" }, `${fmt(p.score)} `, el("small", { textContent: `· ${pct(p.winProb)}` })),
        el("span", { class: "bar" }, el("i", { style: `width:${(p.winProb / maxProb) * 100}%` })),
        el("span", { class: "det" }, detailText(p), badge),
      ),
    );
  }
  $("#step3").hidden = false;
  $("#step3").scrollIntoView({ behavior: "smooth", block: "start" });
}

async function predict() {
  const btn = $("#go");
  btn.disabled = true;
  btn.textContent = "Calcul en cours… (première fois : quelques secondes)";
  try {
    const data = await api("/api/predict", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ circuitId: $("#circuit").value, entries: selectedEntries() }),
    });
    setStatus("");
    renderResult(data);
  } catch (err) {
    setStatus(err.message, true);
  } finally {
    btn.textContent = "Prédire le classement";
    updateCount();
  }
}

async function init() {
  try {
    state.meta = await api("/api/meta");
  } catch (err) {
    setStatus(err.message, true);
    return;
  }
  const sel = $("#circuit");
  for (const c of state.meta.circuits) {
    sel.append(el("option", { value: c.id, textContent: `${c.name} — ${c.country}` }));
  }
  renderDrivers();
  $("#all").addEventListener("click", () => forEachCard((c) => c._set(true, false)));
  $("#none").addEventListener("click", () => forEachCard((c) => c._set(false, false)));
  $("#reset").addEventListener("click", () => forEachCard((c) => c._set(c.classList.contains("on"), true)));
  $("#go").addEventListener("click", predict);
  setStatus("");
  $("#step1").hidden = false;
  $("#step2").hidden = false;
}

init();
