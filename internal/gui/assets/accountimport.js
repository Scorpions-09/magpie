export function drawWorkspaces(box, flow, { el, t, api, cancel }) {
  const workspaces = flow.workspaces || [];
  const busy = flow.state === "importing_workspaces";
  const heading = el("span", "n", t("Codex workspaces"));
  const attempted = workspaces.filter(({ status }) => status);
  const finished = attempted.filter(({ status }) => status !== "pending").length;
  const failed = attempted.filter(({ status }) => status === "failed").length;
  const progress = el("div", "workspace-progress");
  if (attempted.length) {
    const label = t("Processed {done}/{total}", { done: finished, total: attempted.length });
    const bar = el("progress");
    bar.max = attempted.length;
    bar.value = finished;
    bar.setAttribute("aria-label", label);
    progress.setAttribute("role", "status");
    progress.append(el("span", "s", label + (failed ? " · " + t("{n} workspaces failed", { n: failed }) : "")), bar);
  }
  const message = el("span", "s why", flow.error ? t("Workspace discovery or import failed.") + " " + flow.error : "");
  message.setAttribute("role", "status");
  const form = el("form");
  const options = el("div", "workspace-choices");
  const all = el("input");
  all.type = "checkbox";
  const allLabel = el("label", "s");
  allLabel.append(all, " ", t("Select all"));
  const confirm = el("button", "text primary", t(busy ? "Importing workspaces…" : "Import"));
  confirm.type = "submit";
  const close = el("button", "text", t(attempted.length && !busy ? "Done" : "Cancel"));
  close.type = "button";
  close.onclick = cancel;
  const choices = [];
  for (const [index, workspace] of workspaces.entries()) {
    const label = el("label", "s");
    const check = el("input");
    check.type = "checkbox";
    const kept = workspace.status === "added" || workspace.status === "exists" || workspace.status === "verified";
    check.checked = !kept && (flow.workspaceSelection?.has(workspace.accountId) ?? (workspace.current || workspace.status === "failed"));
    check.disabled = busy || kept;
    const state = { added: "Added", exists: "Already in magpie", failed: "Not added", verified: "Verified", pending: "Waiting…" }[workspace.status];
    const number = el("span", "account-number", String(index + 1));
    number.setAttribute("aria-hidden", "true");
    label.append(check, number, workspace.name, state ? " · " + t(state) : "");
    const line = el("div", "workspace-choice");
    line.append(label);
    if (!busy && workspace.status === "failed") {
      const retry = el("button", "text", t("Try again"));
      retry.type = "button";
      retry.onclick = () => submit([workspace.accountId]);
      line.append(retry);
    }
    if (workspace.error) line.append(el("div", "s why", workspace.error));
    options.append(line);
    if (!kept) choices.push({ check, id: workspace.accountId });
  }
  function update() {
    flow.workspaceSelection = new Set(choices.filter(({ check }) => check.checked).map(({ id }) => id));
    const checked = flow.workspaceSelection.size;
    all.checked = !!checked && checked === choices.length;
    all.indeterminate = checked > 0 && checked < choices.length;
    confirm.disabled = busy || checked === 0;
  }
  all.disabled = busy || choices.length === 0;
  all.onchange = () => {
    for (const { check } of choices) check.checked = all.checked;
    update();
  };
  options.onchange = update;
  let posting = false;
  async function submit(ids) {
    if (posting || busy) return;
    posting = true;
    confirm.disabled = true;
    try {
      await api("signin/" + flow.id + "/workspaces", { accountIDs: ids });
    } catch (err) {
      message.textContent = err.message;
      posting = false;
      update();
    }
  }
  form.onsubmit = (event) => {
    event.preventDefault();
    if (!confirm.disabled) submit(choices.filter(({ check }) => check.checked).map(({ id }) => id));
  };
  update();
  if (choices.length > 1) form.append(allLabel);
  const actions = el("div", "acts");
  actions.append(close, confirm);
  form.append(options, actions);
  const title = el("div", "tt");
  title.append(heading, progress, message, form);
  box.append(title);
  options.scrollTop = flow.workspaceScroll || 0;
  options.onscroll = () => { flow.workspaceScroll = options.scrollTop; };
  if (!busy && !flow.workspaceShown) requestAnimationFrame(() => {
    if (!box.isConnected || flow.workspaceShown) return;
    flow.workspaceShown = true;
    box.scrollIntoView({ block: "nearest", inline: "nearest" });
  });
}

const importStatus = { added: "Added", updated: "Updated with this sign-in", exists: "Already in magpie", failed: "Not added" };

export function drawLoginImportResults(box, sub, signing, { el, t, startImport, cancelSignIn }) {
  const tt = el("span", "tt");
  box.append(el("span", "mark", "✓"));
  tt.append(el("span", "n", t("Import finished")));
  const rs = el("span", "results");
  for (const r of signing.results || []) {
    const row = el("span", "res " + r.status);
    // a file that held no account is named, not passed over
    const who = r.user || (r.file && signing.names?.[r.file - 1]) || "";
    row.append(el("span", "u", who), el("span", "st", t(importStatus[r.status] || r.status)));
    if (r.error) { row.title = r.error; row.append(el("span", "why", r.error)); }
    rs.append(row);
  }
  tt.append(rs);
  box.append(tt);
  const done = el("button", "text", t("Done"));
  done.onclick = cancelSignIn;
  // the next batch is one click away, beside the last one's results,
  // rather than behind Done (#2131: several Antigravity exports in a row)
  const more = el("button", "text primary", t("Import more…"));
  more.onclick = () => startImport(sub.agent, signing.accessOnly);
  box.append(done, more);
  return box;
}

export function drawLoginImportInput(box, tt, sub, signing, say, { el, t, tick, status, runImport, cancelSignIn }) {
  if (say.spent) {
    const spent = el("span", "s", signing.accessOnly ? say.kept : say.spent);
    tt.append(spent);
    if (say.kept) {
      const [ao, cb] = tick(t("Access token only"), !!signing.accessOnly);
      ao.classList.add("import-access-only");
      cb.onchange = () => { signing.accessOnly = cb.checked; spent.textContent = cb.checked ? say.kept : say.spent; };
      tt.append(ao);
    }
  }
  if (sub.risk) tt.append(el("span", "s", t(sub.riskNote || "Google may suspend an Antigravity account it sees used outside Antigravity. Use one you can afford to lose.")));
  const area = el("textarea");
  area.rows = 3;
  area.spellcheck = false;
  area.autocomplete = "off";
  area.placeholder = t("…or paste it here");
  area.value = signing.text || "";
  const file = el("input");
  file.type = "file";
  file.multiple = true;
  file.accept = ".json,.txt,application/json,text/plain";
  file.hidden = true;
  const names = el("span", "fname", signing.files.map((f) => f.name).join(", "));
  const go = el("button", "text primary", t("Import"));
  const ready = () => { go.disabled = !(signing.files.length || signing.text.trim()); };
  area.oninput = () => { signing.text = area.value; ready(); };
  file.onchange = async () => {
    const picked = [];
    for (const f of file.files) {
      if (f.size > 4 << 20) { status(t("{name} is too big to be an export", { name: f.name }), "err"); continue; }
      picked.push({ name: f.name, text: await f.text() });
    }
    signing.files = picked;
    names.textContent = picked.map((f) => f.name).join(", ");
    ready();
  };
  const pick = el("button", "link", t("Choose files…"));
  pick.onclick = () => file.click();
  const acts = el("span", "acts");
  acts.append(pick, names, file);
  tt.append(acts, area);
  if (signing.error) tt.append(el("span", "s why", signing.error));
  go.onclick = () => runImport(sub.agent);
  ready();
  const close = el("button", "text", t("Cancel"));
  close.onclick = cancelSignIn;
  box.append(close, go);
  return box;
}
