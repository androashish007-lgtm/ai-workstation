(() => {
  const messagesEl = document.getElementById('messages');
  const suggestionArea = document.getElementById('suggestionArea');
  const sessionListEl = document.getElementById('sessionList');
  const hwLineEl = document.getElementById('hwLine');
  const composer = document.getElementById('composer');
  const input = document.getElementById('input');
  const sendBtn = document.getElementById('sendBtn');
  const attachInput = document.getElementById('attachInput');
  const attachPreview = document.getElementById('attachPreview');
  const newChatBtn = document.getElementById('newChatBtn');
  const newProjectBtn = document.getElementById('newProjectBtn');
  const deleteAllBtn = document.getElementById('deleteAllBtn');
  const updateCheckBtn = document.getElementById('updateCheckBtn');
  const updateStatus = document.getElementById('updateStatus');
  const projectModal = document.getElementById('projectModal');
  const projectModalTitle = document.getElementById('projectModalTitle');
  const projectNameInput = document.getElementById('projectNameInput');
  const projectNotesInput = document.getElementById('projectNotesInput');
  const projectSaveBtn = document.getElementById('projectSaveBtn');
  const projectCancelBtn = document.getElementById('projectCancelBtn');
  const projectDeleteBtn = document.getElementById('projectDeleteBtn');
  const qrImg = document.getElementById('qrImg');
  const qrUrl = document.getElementById('qrUrl');
  const modelDirsError = document.getElementById('modelDirsError');
  const textDirsDefault = document.getElementById('textDirsDefault');
  const textDirsList = document.getElementById('textDirsList');
  const textDirInput = document.getElementById('textDirInput');
  const textDirAddBtn = document.getElementById('textDirAddBtn');
  const imageDirsDefault = document.getElementById('imageDirsDefault');
  const imageDirsList = document.getElementById('imageDirsList');
  const imageDirInput = document.getElementById('imageDirInput');
  const imageDirAddBtn = document.getElementById('imageDirAddBtn');
  const statusDot = document.getElementById('statusDot');
  const statusText = document.getElementById('statusText');
  const statusBadge = document.getElementById('statusBadge');
  const activeModelsBadge = document.getElementById('activeModelsBadge');
  const activeModelsText = document.getElementById('activeModelsText');
  const usageTable = document.getElementById('usageTable');
  const settingsBtn = document.getElementById('settingsBtn');
  const settingsModal = document.getElementById('settingsModal');
  const settingsCloseBtn = document.getElementById('settingsCloseBtn');
  const logsView = document.getElementById('logsView');
  const textModelSelect = document.getElementById('textModelSelect');
  const imageModelSelect = document.getElementById('imageModelSelect');

  // Sessions generate independently of whichever one is being viewed — the
  // backend keeps working regardless of what's on screen. currentStream is
  // just this tab's live window into whichever session is currently shown;
  // switching sessions closes it and opens a new one for the newly-viewed
  // session, but never touches what's running server-side for any session.
  let state = { sessions: [], projects: [], models: [], activeId: null, session: null, attachments: [], generating: false };
  let currentStream = null;
  let currentAssistantBubble = null;
  let editingProjectId = null; // null while the "New project" flow is open

  function loadCollapsedProjects() {
    try { return new Set(JSON.parse(localStorage.getItem('collapsedProjects') || '[]')); }
    catch (e) { return new Set(); }
  }
  function saveCollapsedProjects(set) {
    try { localStorage.setItem('collapsedProjects', JSON.stringify([...set])); } catch (e) { /* private mode etc: skip */ }
  }
  const collapsedProjects = loadCollapsedProjects();

  async function api(path, opts) {
    const res = await fetch(path, opts);
    if (!res.ok) throw new Error(await res.text());
    const ct = res.headers.get('content-type') || '';
    return ct.includes('application/json') ? res.json() : res.text();
  }

  function fmtBytes(n) {
    if (!n) return '0 B';
    const u = ['B', 'KB', 'MB', 'GB', 'TB'];
    let i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return n.toFixed(1) + ' ' + u[i];
  }

  async function boot() {
    const data = await api('/api/bootstrap');
    state.sessions = data.sessions || [];
    state.projects = data.projects || [];
    state.models = data.models || [];
    state.activeId = data.active_session_id;
    hwLineEl.textContent = `${data.hw.cpu_cores} cores · ${fmtBytes(data.hw.total_ram_bytes)} RAM` +
      (data.hw.gpu_vendor && data.hw.gpu_vendor !== 'none' ? ` · ${data.hw.gpu_vendor} GPU` : ' · CPU only');
    renderSessionList();
    await openSession(state.activeId);
    renderSuggestions(data.suggestions_text, data.suggestions_image, data.models);
    renderModelPickers();

    // Suggestions were previously only (re-)computed at page load or when
    // a chat request actually failed for lack of a model — so installing
    // a model (or the hardware/catalog otherwise changing) while this tab
    // stays open indefinitely was never reflected until a reload. Re-check
    // periodically instead of only on those two triggers.
    setInterval(refreshSuggestions, 30 * 60 * 1000);
  }

  async function refreshSuggestions() {
    try {
      const [models, textSug, imageSug] = await Promise.all([
        api('/api/registry'),
        api('/api/suggestions?kind=text'),
        api('/api/suggestions?kind=image'),
      ]);
      state.models = models || [];
      renderSuggestions(textSug, imageSug, models);
      renderModelPickers();
    } catch {
      // Best-effort background refresh — leave whatever's currently shown.
    }
  }

  // renderModelPickers fills the two composer dropdowns from whatever's
  // currently installed, always with "Auto" first (the router's normal
  // hardware/complexity-fit pick — see router.SelectTextModel/
  // SelectImageModel) so nothing changes for anyone who never touches
  // them. Re-run on every session switch/model-list refresh so a model
  // deleted (or added) elsewhere doesn't leave a stale/missing option
  // selected.
  function renderModelPickers() {
    const textModels = state.models.filter(m => m.kind === 'text' && !m.is_vision_projector);
    // image_role marks a FLUX.2 VAE/text-encoder file — a component paired
    // onto a diffusion-model checkpoint (see fillModelSelect's incomplete-
    // FLUX.2 handling below), never itself something to generate with.
    const imageModels = state.models.filter(m => m.kind === 'image' && !m.image_role);
    const selectedText = (state.session && state.session.last_text_model_id) || '';
    const selectedImage = (state.session && state.session.last_image_model_id) || '';
    fillModelSelect(textModelSelect, textModels, selectedText);
    fillModelSelect(imageModelSelect, imageModels, selectedImage);
    textModelSelect.disabled = textModels.length === 0;
    imageModelSelect.disabled = imageModels.length === 0;
  }

  // Mirrors router.LooksLikeEditingModel (Go) — these checkpoints expect an
  // existing photo plus an edit instruction (img2img/inpaint/pix2pix-style),
  // not a from-scratch text prompt, and produce garbled/blurry/duplicated
  // output when picked for one. The Model Usage tab already explains this,
  // but that's a separate panel from the picker where the choice actually
  // gets made — surfaced here too so picking one isn't a silent trap.
  const EDITING_MODEL_HINTS = ['pix2pix', 'inpaint', 'controlnet', 'img2img'];
  function looksLikeEditingModel(filename) {
    const f = (filename || '').toLowerCase();
    return EDITING_MODEL_HINTS.some(h => f.includes(h));
  }

  function fillModelSelect(select, models, selectedId) {
    select.innerHTML = '';
    const auto = document.createElement('option');
    auto.value = '';
    auto.textContent = 'Auto (recommended)';
    select.appendChild(auto);
    for (const m of models) {
      const opt = document.createElement('option');
      opt.value = m.id;
      opt.textContent = (m.name || m.filename) + ' — ' + fmtBytes(m.size_bytes);
      // A FLUX.2 checkpoint needs a separate VAE and text-encoder file
      // installed alongside it (see registry.Model.PairedVAE/
      // PairedTextEncoder) — surfaced here rather than just failing
      // silently if picked, since there's no other place a user would
      // see this.
      if (m.image_family === 'flux2' && (!m.paired_vae || !m.paired_text_encoder)) {
        const missing = [!m.paired_vae && 'VAE', !m.paired_text_encoder && 'text encoder'].filter(Boolean).join(' + ');
        opt.textContent += ` (missing ${missing})`;
        opt.disabled = true;
      }
      if (looksLikeEditingModel(m.filename)) {
        opt.textContent += ' (needs a reference image — not for text-to-image)';
      }
      select.appendChild(opt);
    }
    // Fall back to Auto if the previously-picked model was deleted since.
    select.value = models.some(m => m.id === selectedId) ? selectedId : '';
  }

  async function setSelectedModel(kind, modelId) {
    if (!state.activeId) return;
    const body = kind === 'text' ? { text_model_id: modelId } : { image_model_id: modelId };
    const updated = await api('/api/sessions/' + state.activeId, {
      method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
    });
    if (state.session && state.session.id === updated.id) state.session = updated;
  }

  textModelSelect.addEventListener('change', () => setSelectedModel('text', textModelSelect.value));
  imageModelSelect.addEventListener('change', () => setSelectedModel('image', imageModelSelect.value));

  function renderSessionList() {
    sessionListEl.innerHTML = '';

    const byProject = new Map();
    const ungrouped = [];
    for (const s of state.sessions) {
      if (s.project_id) {
        if (!byProject.has(s.project_id)) byProject.set(s.project_id, []);
        byProject.get(s.project_id).push(s);
      } else {
        ungrouped.push(s);
      }
    }

    for (const p of state.projects) {
      const group = document.createElement('div');
      group.className = 'project-group';

      const collapsed = collapsedProjects.has(p.id);
      const header = document.createElement('div');
      header.className = 'project-header' + (collapsed ? ' collapsed' : '');
      header.innerHTML = `<span class="caret">▾</span><span class="project-name">${escapeHtml(p.name)}</span>`;

      const addBtn = document.createElement('button');
      addBtn.className = 'icon-btn';
      addBtn.textContent = '+';
      addBtn.title = 'New chat in this project';
      addBtn.onclick = async (e) => {
        e.stopPropagation();
        const s = await api('/api/sessions', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ project_id: p.id }) });
        state.sessions.unshift({ id: s.id, title: s.title, updated_at: s.updated_at, project_id: p.id, generating: false });
        await openSession(s.id);
      };
      const editBtn = document.createElement('button');
      editBtn.className = 'icon-btn';
      editBtn.textContent = '✎';
      editBtn.title = 'Edit project';
      editBtn.onclick = (e) => { e.stopPropagation(); openProjectModal(p); };
      header.appendChild(addBtn);
      header.appendChild(editBtn);

      header.onclick = () => {
        if (collapsedProjects.has(p.id)) collapsedProjects.delete(p.id); else collapsedProjects.add(p.id);
        saveCollapsedProjects(collapsedProjects);
        renderSessionList();
      };
      group.appendChild(header);

      const chatsEl = document.createElement('div');
      chatsEl.className = 'project-chats' + (collapsed ? ' collapsed' : '');
      for (const s of (byProject.get(p.id) || [])) {
        chatsEl.appendChild(sessionItemEl(s));
      }
      group.appendChild(chatsEl);
      sessionListEl.appendChild(group);
    }

    if (state.projects.length && ungrouped.length) {
      const label = document.createElement('div');
      label.className = 'ungrouped-label';
      label.textContent = 'Chats';
      sessionListEl.appendChild(label);
    }
    for (const s of ungrouped) {
      sessionListEl.appendChild(sessionItemEl(s));
    }
  }

  let openMoveMenu = null;
  function closeMoveMenu() {
    if (openMoveMenu) { openMoveMenu.remove(); openMoveMenu = null; }
  }
  document.addEventListener('click', closeMoveMenu);

  function toggleMoveMenu(anchorBtn, s) {
    const wasOpen = !!openMoveMenu;
    closeMoveMenu();
    if (wasOpen) return; // clicking the same button again just closes it

    const menu = document.createElement('div');
    menu.className = 'move-menu';
    const addItem = (label, projectId) => {
      const item = document.createElement('div');
      item.className = 'move-menu-item' + ((s.project_id || '') === projectId ? ' current' : '');
      item.textContent = label;
      item.onclick = async (e) => {
        e.stopPropagation();
        closeMoveMenu();
        try {
          await api('/api/sessions/' + s.id, { method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ project_id: projectId }) });
        } catch (err) {
          alert('Could not move chat: ' + err.message);
        }
        await refreshSessionList();
      };
      menu.appendChild(item);
    };
    addItem('No project', '');
    for (const p of state.projects) addItem(p.name, p.id);

    document.body.appendChild(menu);
    const rect = anchorBtn.getBoundingClientRect();
    const menuWidth = 180;
    menu.style.top = (rect.bottom + 4) + 'px';
    menu.style.left = Math.min(rect.left, window.innerWidth - menuWidth - 8) + 'px';
    openMoveMenu = menu;
  }

  function sessionItemEl(s) {
    const el = document.createElement('div');
    el.className = 'session-item' + (s.id === state.activeId ? ' active' : '');
    const title = document.createElement('span');
    title.className = 'session-title';
    title.textContent = s.title || 'New chat';
    el.appendChild(title);
    if (s.generating) {
      const dot = document.createElement('span');
      dot.className = 'session-generating-dot';
      dot.title = 'Still working on a response';
      el.appendChild(dot);
    }
    if (state.projects.length) {
      const moveBtn = document.createElement('button');
      moveBtn.className = 'icon-btn';
      moveBtn.textContent = '📁';
      moveBtn.title = 'Move to project';
      moveBtn.onclick = (e) => { e.stopPropagation(); toggleMoveMenu(moveBtn, s); };
      el.appendChild(moveBtn);
    }
    const delBtn = document.createElement('button');
    delBtn.className = 'icon-btn';
    delBtn.textContent = '✕';
    delBtn.title = 'Delete this chat';
    delBtn.onclick = (e) => { e.stopPropagation(); deleteChat(s.id, s.title); };
    el.appendChild(delBtn);
    el.onclick = () => openSession(s.id);
    return el;
  }

  async function deleteChat(id, title) {
    if (!confirm(`Delete "${title || 'this chat'}"? This can't be undone.`)) return;
    try {
      await api('/api/sessions/' + id, { method: 'DELETE' });
    } catch (err) {
      alert('Could not delete: ' + err.message);
      return;
    }
    const wasActive = state.activeId === id;
    await refreshSessionList();
    if (wasActive) {
      const next = state.sessions[0];
      if (next) {
        await openSession(next.id);
      } else {
        const s = await api('/api/sessions', { method: 'POST' });
        state.sessions.unshift({ id: s.id, title: s.title, updated_at: s.updated_at, generating: false });
        await openSession(s.id);
      }
    }
  }

  deleteAllBtn.onclick = async () => {
    if (!confirm('Delete ALL chats? Chats currently generating a response will be skipped. This can\'t be undone.')) return;
    let result;
    try {
      result = await api('/api/sessions', { method: 'DELETE' });
    } catch (err) {
      alert('Could not delete: ' + err.message);
      return;
    }
    if (result.skipped) alert(`Deleted ${result.deleted} chat(s). Skipped ${result.skipped} still generating a response.`);
    const s = await api('/api/sessions', { method: 'POST' });
    state.sessions = [{ id: s.id, title: s.title, updated_at: s.updated_at, generating: false }];
    await openSession(s.id);
  };

  async function refreshSessionList() {
    try {
      state.sessions = await api('/api/sessions');
      renderSessionList();
    } catch (e) { /* non-fatal */ }
  }

  async function refreshProjects() {
    try {
      state.projects = await api('/api/projects');
      renderSessionList();
    } catch (e) { /* non-fatal */ }
  }

  // --- Projects ---
  function openProjectModal(project) {
    editingProjectId = project ? project.id : null;
    projectModalTitle.textContent = project ? 'Edit project' : 'New project';
    projectNameInput.value = project ? project.name : '';
    projectNotesInput.value = project ? project.notes : '';
    projectDeleteBtn.hidden = !project;
    projectModal.hidden = false;
    projectNameInput.focus();
  }
  function closeProjectModal() { projectModal.hidden = true; editingProjectId = null; }

  newProjectBtn.onclick = () => openProjectModal(null);
  projectCancelBtn.onclick = closeProjectModal;
  projectModal.onclick = (e) => { if (e.target === projectModal) closeProjectModal(); };

  projectSaveBtn.onclick = async () => {
    const name = projectNameInput.value.trim();
    if (!name) { projectNameInput.focus(); return; }
    const notes = projectNotesInput.value;
    try {
      if (editingProjectId) {
        await api('/api/projects/' + editingProjectId, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name, notes }) });
      } else {
        await api('/api/projects', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name, notes }) });
      }
    } catch (err) {
      alert('Could not save project: ' + err.message);
      return;
    }
    closeProjectModal();
    await refreshProjects();
  };

  projectDeleteBtn.onclick = async () => {
    if (!editingProjectId) return;
    if (!confirm('Delete this project? Its chats are kept, just ungrouped.')) return;
    try {
      await api('/api/projects/' + editingProjectId, { method: 'DELETE' });
    } catch (err) {
      alert('Could not delete project: ' + err.message);
      return;
    }
    closeProjectModal();
    await refreshSessionList();
    await refreshProjects();
  };

  // openSession is the single entry point for viewing a session: load its
  // saved history, render it, then (re)attach to its live stream — which
  // either immediately reports idle (nothing happening) or replays/follows
  // whatever's already in progress there, started from this tab or another.
  async function openSession(id) {
    if (!id) return;
    if (currentStream) { currentStream.close(); currentStream = null; }
    currentAssistantBubble = null;
    setComposerGenerating(false); // avoid a stale Stop label until this session's own stream reports in
    state.attachments = [];
    renderAttachPreview();

    state.session = await api('/api/sessions/' + id);
    state.activeId = id;
    renderSessionList();
    renderMessages();
    renderModelPickers();
    attachStream(id);
  }

  const TERMINAL_EVENTS = new Set(['idle', 'done', 'cancelled', 'error']);

  function attachStream(id) {
    const es = new EventSource('/api/sessions/' + encodeURIComponent(id) + '/stream');
    currentStream = es;
    es.onmessage = async (e) => {
      if (state.activeId !== id) return; // user switched away since this connected; ignore stale updates
      let evt;
      try { evt = JSON.parse(e.data); } catch { return; }
      if (evt.type === 'idle') {
        // Nothing was replayed, which means either nothing is running for
        // this chat, or — the case that matters here — a turn that fails
        // fast (e.g. no matching model installed, no real inference to
        // wait on) finished and was cleaned up before this connection even
        // reached the server. Either way, re-fetching picks up whatever
        // was actually saved instead of silently showing a stale view.
        try {
          const fresh = await api('/api/sessions/' + encodeURIComponent(id));
          if (state.activeId === id) { state.session = fresh; renderMessages(); }
        } catch {}
      }
      setComposerGenerating(!TERMINAL_EVENTS.has(evt.type));
      handleEvent(evt);
      if (TERMINAL_EVENTS.has(evt.type)) {
        setComposerGenerating(false);
        es.close();
        if (currentStream === es) currentStream = null;
        refreshSessionList();
        // The backend replaces a new chat's truncated placeholder title
        // with an LLM-generated one in the background after the first
        // exchange, with no live event of its own — so a fixed delay
        // after "done" is the only way this view picks it up promptly
        // instead of waiting for some unrelated later refresh.
        if (evt.type === 'done') setTimeout(refreshSessionList, 4000);
      }
    };
    es.onerror = () => {
      setComposerGenerating(false);
      es.close();
      if (currentStream === es) currentStream = null;
    };
  }

  function renderMessages() {
    messagesEl.innerHTML = '';
    for (const m of (state.session.messages || [])) {
      const urls = m.image_paths && m.image_paths.length
        ? m.image_paths.map(p => '/images/' + p)
        : (m.image_path ? ['/images/' + m.image_path] : []);
      const bubble = appendMessageEl(m.role, m.content, urls, m.timestamp);
      if (m.role === 'assistant' && m.notice) attachNoticeCard(bubble, m.notice);
    }
    messagesEl.scrollTop = messagesEl.scrollHeight;
  }

  // A persisted notice's text is a snapshot of why a past turn failed —
  // whether it still needs action (and what button to show for it) has to
  // be checked live, since the user may well have already fixed it.
  async function attachNoticeCard(bubble, notice) {
    let status;
    try { status = await api('/api/notice-status?notice=' + encodeURIComponent(notice)); } catch { return; }
    if (!bubble.isConnected || status.resolved) return;
    const NOTICE_LABELS = {
      no_text_model: 'No chat model installed yet',
      no_vision_model: 'No vision-capable chat model installed yet',
      no_image_model: 'No image model installed yet',
    };
    let card;
    if ('suggestion' in status) {
      if (!status.suggestion) return; // nothing catalog-suggested fits this hardware
      card = suggestionCard(status.suggestion, NOTICE_LABELS[notice] || 'No matching model installed yet');
    } else if ('component' in status) {
      card = approvalCard(status.component, status.engine_status);
    } else {
      return;
    }
    bubble.insertAdjacentElement('afterend', card);
  }

  function fmtTime(ts) {
    const d = ts ? new Date(ts) : new Date();
    if (isNaN(d.getTime())) return '';
    return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  }

  // A streaming assistant bubble is stamped with "now" the moment it's
  // first created (ensureAssistantBubble), which is when the response
  // STARTED, not when it finished — for anything but an instant reply
  // that's the wrong time to show. Call this once the response actually
  // completes so the displayed time matches what gets saved to history
  // (the backend timestamps the message after generation finishes, not
  // before), instead of the two silently disagreeing until the next reload.
  function refreshMessageTime(bubble) {
    if (!bubble || !bubble.parentElement) return;
    const timeEl = bubble.parentElement.querySelector('.msg-time');
    if (timeEl) timeEl.textContent = fmtTime(new Date().toISOString());
  }

  // timestamp is an ISO string when rendering saved history (the backend's
  // own record of when it happened); live messages don't have one from the
  // server yet, so the client's own clock at render time is used instead —
  // close enough for a timestamp label, and gets replaced with the real
  // saved value next time this session is loaded from disk.
  function appendMessageEl(role, text, imageUrls, timestamp) {
    const wrap = document.createElement('div');
    wrap.className = 'msg ' + role;
    const col = document.createElement('div');
    col.className = 'msg-col';
    const bubble = document.createElement('div');
    bubble.className = 'bubble';
    if (text) bubble.appendChild(document.createTextNode(text));
    for (const url of (imageUrls || [])) {
      const img = document.createElement('img');
      img.className = 'gen-image';
      img.src = url;
      bubble.appendChild(img);
    }
    col.appendChild(bubble);
    const timeEl = document.createElement('div');
    timeEl.className = 'msg-time';
    timeEl.textContent = fmtTime(timestamp);
    col.appendChild(timeEl);
    wrap.appendChild(col);
    messagesEl.appendChild(wrap);
    messagesEl.scrollTop = messagesEl.scrollHeight;
    return bubble;
  }

  function renderSuggestions(textSug, imageSug, models) {
    suggestionArea.innerHTML = '';
    const hasText = (models || []).some(m => m.kind === 'text' && !m.is_vision_projector);
    const hasImage = (models || []).some(m => m.kind === 'image' && !m.image_role);
    if (!hasText && textSug && textSug.length) suggestionArea.appendChild(suggestionCard(textSug[0], 'No chat model installed yet'));
    if (!hasImage && imageSug && imageSug.length) suggestionArea.appendChild(suggestionCard(imageSug[0], 'No image model installed yet'));
  }

  function suggestionCard(entry, label) {
    const card = document.createElement('div');
    card.className = 'suggest-card';
    const left = document.createElement('div');
    left.innerHTML = `<strong>${label}</strong><br><span style="color:var(--text-dim)">${entry.name} — ${fmtBytes(entry.size_bytes)}</span>`;
    const btn = document.createElement('button');
    btn.textContent = 'Download';
    btn.onclick = async () => {
      btn.disabled = true;
      btn.textContent = 'Starting…';
      await api('/api/downloads/model', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ catalog_id: entry.id }) });
      pollDownload(card, btn);
    };
    card.appendChild(left);
    card.appendChild(btn);
    return card;
  }

  async function pollDownload(card, btn) {
    const bar = document.createElement('div');
    bar.className = 'progress-bar';
    const fill = document.createElement('div');
    fill.className = 'progress-fill';
    bar.appendChild(fill);
    card.appendChild(bar);
    const tick = async () => {
      const st = await api('/api/downloads/status');
      if (st.total_bytes) fill.style.width = Math.min(100, 100 * st.done_bytes / st.total_bytes) + '%';
      btn.textContent = fmtBytes(st.done_bytes) + (st.total_bytes ? ' / ' + fmtBytes(st.total_bytes) : '');
      if (st.done) {
        btn.textContent = st.error ? 'Failed: ' + st.error : 'Installed ✓';
        if (!st.error) setTimeout(() => card.remove(), 1500);
        return;
      }
      setTimeout(tick, 800);
    };
    tick();
  }

  function approvalCard(kind, statusObj) {
    const card = document.createElement('div');
    card.className = 'approval-card';
    const msg = (statusObj && statusObj.message) || `Approve downloading the ${kind} engine?`;
    card.innerHTML = `<span>${msg}</span>`;
    const btn = document.createElement('button');
    btn.textContent = 'Approve';
    btn.onclick = async () => {
      btn.disabled = true;
      btn.textContent = 'Working…';
      await api('/api/engine/approve', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ component: kind }) });
      setTimeout(() => card.remove(), 400);
    };
    card.appendChild(btn);
    return card;
  }

  function autoGrow() {
    input.style.height = 'auto';
    input.style.height = Math.min(200, input.scrollHeight) + 'px';
  }
  input.addEventListener('input', autoGrow);
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); composer.requestSubmit(); }
  });

  function renderAttachPreview() {
    attachPreview.innerHTML = '';
    attachPreview.hidden = state.attachments.length === 0;
    state.attachments.forEach((dataURI, i) => {
      const item = document.createElement('div');
      item.className = 'attach-item';
      const img = document.createElement('img');
      img.src = dataURI;
      const rm = document.createElement('button');
      rm.textContent = '✕';
      rm.type = 'button';
      rm.title = 'Remove';
      rm.onclick = () => {
        state.attachments.splice(i, 1);
        renderAttachPreview();
      };
      item.appendChild(img);
      item.appendChild(rm);
      attachPreview.appendChild(item);
    });
  }

  attachInput.addEventListener('change', () => {
    const files = Array.from(attachInput.files || []);
    if (!files.length) return;
    Promise.all(files.map(file => new Promise((resolve, reject) => {
      const reader = new FileReader();
      reader.onload = () => resolve(reader.result);
      reader.onerror = reject;
      reader.readAsDataURL(file);
    }))).then(dataURIs => {
      state.attachments.push(...dataURIs);
      renderAttachPreview();
      attachInput.value = '';
    });
  });

  newChatBtn.onclick = async () => {
    const s = await api('/api/sessions', { method: 'POST' });
    state.sessions.unshift({ id: s.id, title: s.title, updated_at: s.updated_at, generating: false });
    await openSession(s.id);
  };

  async function loadModelUsage() {
    usageTable.innerHTML = '<div class="usage-empty">Loading…</div>';
    let groups;
    try { groups = await api('/api/usage/models'); } catch (e) { groups = { text: [], image: [] }; }
    const text = groups.text || [], image = groups.image || [];
    if (!text.length && !image.length) {
      usageTable.innerHTML = '<div class="usage-empty">No models installed — drop a .gguf into models/text/ or a .gguf/.safetensors into models/image/.</div>';
      return;
    }
    usageTable.innerHTML = '';
    usageTable.appendChild(renderModelGroup('models/text/', text));
    usageTable.appendChild(renderModelGroup('models/image/', image));
  }

  function renderModelGroup(folderLabel, entries) {
    const section = document.createElement('div');
    section.className = 'usage-group';
    const heading = document.createElement('h3');
    heading.className = 'usage-group-heading';
    heading.textContent = folderLabel + ' (' + entries.length + ')';
    section.appendChild(heading);
    if (!entries.length) {
      const empty = document.createElement('div');
      empty.className = 'usage-empty';
      empty.textContent = 'Nothing installed here yet.';
      section.appendChild(empty);
      return section;
    }
    const maxCount = Math.max(1, ...entries.map(r => r.count));
    entries.forEach(r => {
      const row = document.createElement('div');
      row.className = 'usage-row';
      const pct = r.count ? Math.max(4, Math.round(100 * r.count / maxCount)) : 0;
      const last = r.last_used_at ? new Date(r.last_used_at).toLocaleString() : 'never';
      row.innerHTML = `
        <div class="usage-row-main">
          <div class="name">${escapeHtml(r.name)}<span class="size">${fmtBytes(r.size_bytes)}</span></div>
          <div class="use-case">${escapeHtml(r.use_case)}</div>
          <div class="usage-stats">
            <div class="bar-wrap"><div class="bar-fill" style="width:${pct}%"></div></div>
            <div class="count">${r.count} use${r.count === 1 ? '' : 's'} · last ${last}</div>
          </div>
        </div>
        <button class="model-delete-btn" title="Delete this model file">🗑</button>
      `;
      row.querySelector('.model-delete-btn').onclick = () => deleteModel(r, row);
      section.appendChild(row);
    });
    return section;
  }

  async function deleteModel(entry, row) {
    if (!confirm(`Delete "${entry.name}" (${entry.filename}, ${fmtBytes(entry.size_bytes)}) from disk? This can't be undone.`)) return;
    const btn = row.querySelector('.model-delete-btn');
    btn.disabled = true;
    btn.textContent = '…';
    try {
      await api('/api/models/' + encodeURIComponent(entry.id), { method: 'DELETE' });
      row.remove();
    } catch (e) {
      alert('Could not delete: ' + e.message);
      btn.disabled = false;
      btn.textContent = '🗑';
    }
  }

  function escapeHtml(s) {
    const d = document.createElement('div');
    d.textContent = s;
    return d.innerHTML;
  }

  // --- Settings modal (Model usage / Model folders / Phone access / Updates / Data) ---
  // One modal, opened by a single sidebar button, with its own sub-tabs —
  // consolidates what used to be five separate sidebar buttons/tabs into
  // one place, each section loading its data lazily the first time it's
  // switched to rather than all up front.
  const settingsSections = {
    usage: { tab: document.getElementById('settingsTabUsage'), panel: document.getElementById('settingsUsageSection'), load: loadModelUsage },
    dirs: { tab: document.getElementById('settingsTabDirs'), panel: document.getElementById('settingsDirsSection'), load: loadModelDirs },
    phone: { tab: document.getElementById('settingsTabPhone'), panel: document.getElementById('settingsPhoneSection'), load: loadQR },
    updates: { tab: document.getElementById('settingsTabUpdates'), panel: document.getElementById('settingsUpdatesSection') },
    logs: { tab: document.getElementById('settingsTabLogs'), panel: document.getElementById('settingsLogsSection'), load: loadLogs },
    data: { tab: document.getElementById('settingsTabData'), panel: document.getElementById('settingsDataSection') },
  };

  function showSettingsSection(name) {
    if (name !== 'logs') stopLogsPoll(); // only the Logs tab needs to keep polling while open
    for (const [key, s] of Object.entries(settingsSections)) {
      const active = key === name;
      s.tab.classList.toggle('active', active);
      s.panel.hidden = !active;
    }
    settingsSections[name].load?.();
  }
  for (const [key, s] of Object.entries(settingsSections)) {
    s.tab.onclick = () => showSettingsSection(key);
  }

  function openSettings() { settingsModal.hidden = false; showSettingsSection('usage'); }
  function closeSettings() { settingsModal.hidden = true; stopLogsPoll(); }
  settingsBtn.onclick = openSettings;
  settingsCloseBtn.onclick = closeSettings;
  settingsModal.onclick = (e) => { if (e.target === settingsModal) closeSettings(); };

  // --- Live log tail ---
  let logsCursor = 0;
  let logsPollTimer = null;

  function appendLogLines(lines) {
    if (!lines || !lines.length) return;
    // Only auto-scroll if the view was already at (or near) the bottom —
    // preserves a manual scroll-up to read older lines instead of yanking
    // the viewport back down on every new poll.
    const atBottom = logsView.scrollTop + logsView.clientHeight >= logsView.scrollHeight - 4;
    for (const line of lines) {
      logsView.appendChild(document.createTextNode(line + '\n'));
    }
    if (atBottom) logsView.scrollTop = logsView.scrollHeight;
  }

  async function loadLogs() {
    logsView.textContent = '';
    logsCursor = 0;
    try {
      const data = await api('/api/logs');
      appendLogLines(data.lines);
      logsCursor = data.cursor || 0;
    } catch (e) {
      logsView.textContent = 'Could not load logs: ' + e.message;
    }
    stopLogsPoll();
    logsPollTimer = setInterval(pollLogs, 1500);
  }

  async function pollLogs() {
    try {
      const data = await api('/api/logs?since=' + logsCursor);
      appendLogLines(data.lines);
      logsCursor = data.cursor || logsCursor;
    } catch (e) {
      // Transient — the modal may have closed mid-request; next tick retries.
    }
  }

  function stopLogsPoll() {
    if (logsPollTimer) { clearInterval(logsPollTimer); logsPollTimer = null; }
  }

  async function loadQR() {
    qrUrl.textContent = 'Loading…';
    qrImg.removeAttribute('src');
    try {
      // Always the machine's real LAN IP, never window.location.origin —
      // that's 127.0.0.1 whenever the desktop app opened this locally,
      // which is meaningless to a phone.
      const { url } = await api('/api/lan-url');
      qrImg.onload = () => { qrUrl.textContent = url; };
      qrImg.onerror = () => { qrUrl.textContent = 'Could not load the QR code — the app may have restarted. Try again.'; };
      qrImg.src = '/api/qr?url=' + encodeURIComponent(url);
    } catch (e) {
      qrUrl.textContent = 'No LAN address available (the app may be running with LAN access disabled).';
    }
  }

  updateCheckBtn.onclick = async () => {
    updateStatus.hidden = false;
    updateStatus.textContent = 'Checking…';
    updateCheckBtn.disabled = true;
    try {
      const st = await api('/api/self-update/check');
      if (st.error) {
        updateStatus.textContent = 'Could not check: ' + st.error;
      } else if (st.update_available) {
        updateStatus.innerHTML = `Update available: v${st.current} → v${st.latest}. ` +
          `Run the update script, or see <a href="${st.repo_url}" target="_blank" rel="noopener">${st.repo_url}</a>.`;
      } else {
        updateStatus.textContent = `Up to date (v${st.current}).`;
      }
    } catch (e) {
      updateStatus.textContent = 'Could not check: ' + e.message;
    } finally {
      updateCheckBtn.disabled = false;
    }
  };

  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    if (!settingsModal.hidden) closeSettings();
    if (!projectModal.hidden) closeProjectModal();
  });

  // --- Model folders (extra scan locations) ---
  function renderDirsList(listEl, dirs, kind) {
    listEl.innerHTML = '';
    for (const d of dirs) {
      const row = document.createElement('div');
      row.className = 'model-dirs-item';
      row.innerHTML = `<span>${escapeHtml(d)}</span><button title="Remove">✕</button>`;
      row.querySelector('button').onclick = () => removeModelDir(kind, d);
      listEl.appendChild(row);
    }
  }

  let modelDirsCache = { text: { default: '', extra: [] }, image: { default: '', extra: [] } };

  async function loadModelDirs() {
    modelDirsError.hidden = true;
    try {
      modelDirsCache = await api('/api/model-dirs');
      textDirsDefault.textContent = modelDirsCache.text.default;
      imageDirsDefault.textContent = modelDirsCache.image.default;
      renderDirsList(textDirsList, modelDirsCache.text.extra || [], 'text');
      renderDirsList(imageDirsList, modelDirsCache.image.extra || [], 'image');
    } catch (e) {
      modelDirsError.hidden = false;
      modelDirsError.textContent = 'Could not load: ' + e.message;
    }
  }

  async function saveModelDirs(kind, dirs) {
    modelDirsError.hidden = true;
    try {
      await api('/api/model-dirs', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ kind, dirs }),
      });
      await loadModelDirs();
      await refreshSuggestions(); // the newly (un)scanned folder's models should show up/disappear right away
    } catch (e) {
      modelDirsError.hidden = false;
      modelDirsError.textContent = e.message;
    }
  }

  function addModelDir(kind) {
    const input = kind === 'text' ? textDirInput : imageDirInput;
    const path = input.value.trim();
    if (!path) return;
    const current = modelDirsCache[kind].extra || [];
    if (current.includes(path)) { input.value = ''; return; }
    saveModelDirs(kind, [...current, path]);
    input.value = '';
  }

  function removeModelDir(kind, path) {
    const current = modelDirsCache[kind].extra || [];
    saveModelDirs(kind, current.filter(d => d !== path));
  }

  textDirAddBtn.onclick = () => addModelDir('text');
  imageDirAddBtn.onclick = () => addModelDir('image');
  textDirInput.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); addModelDir('text'); } });
  imageDirInput.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); addModelDir('image'); } });

  // --- System usage badge ---
  const ACTIVITY_LABELS = {
    idle: null,
    generating_text: 'thinking…',
    generating_image: 'drawing…',
    downloading_model: 'downloading model…',
    bootstrapping_engine: 'setting up engine…',
    awaiting_approval: 'waiting for approval',
  };

  async function pollSystemUsage() {
    try {
      const u = await api('/api/system/usage');
      const cpu = u.cpu_available ? Math.round(u.cpu_percent) + '%' : '–';
      const ram = Math.round(u.ram_percent) + '%';
      let label = 'CPU ' + cpu + ' · RAM ' + ram;
      if (u.gpu_vendor && u.gpu_vendor !== 'none') {
        label += ' · GPU ' + (u.gpu_available ? Math.round(u.gpu_percent) + '%' : '–');
      }
      const act = u.activity || {};
      const activityLabel = ACTIVITY_LABELS[act.kind];

      statusDot.classList.remove('working', 'stuck');
      if (act.kind && act.kind !== 'idle') {
        statusDot.classList.add(act.possibly_stuck ? 'stuck' : 'working');
        if (activityLabel) label += ' · ' + activityLabel;
        if (act.count > 1) label += ` (+${act.count - 1} more)`;
        if (act.possibly_stuck) label += ' (taking longer than usual)';
      }
      statusText.textContent = label;
      let title = act.possibly_stuck
        ? 'Still running, but slower than expected — this can happen on a big model with no GPU. No action needed unless it never finishes.'
        : 'System load';
      if (u.gpu_name) title += '\nGPU: ' + u.gpu_name + (u.gpu_available ? '' : ' (live % not available on this OS)');
      statusBadge.title = title;
    } catch (e) {
      // Cosmetic feature: fail silent, keep showing the last good reading.
    }
  }
  pollSystemUsage();
  setInterval(pollSystemUsage, 2000);

  // --- Active models widget ---
  // Shows which specific text/image model is actually loaded or generating
  // right now, next to the CPU/RAM badge above — a plain "45% CPU" reading
  // doesn't say which of several installed models is responsible for it.
  function describeModelSide(entries, icon) {
    if (!entries || !entries.length) return '';
    const e = entries[0];
    let text = icon + ' ' + e.name;
    if (e.busy) text += ' (generating)';
    else if (e.loaded) text += ' (loaded)';
    else text += ' (last used)';
    if (entries.length > 1) text += ` +${entries.length - 1} more`;
    return text;
  }

  async function pollActiveModels() {
    try {
      const data = await api('/api/usage/active-models');
      const parts = [describeModelSide(data.text, '💬'), describeModelSide(data.image, '🖼')].filter(Boolean);
      if (!parts.length) {
        activeModelsBadge.hidden = true;
        return;
      }
      activeModelsText.textContent = parts.join(' · ');
      activeModelsBadge.hidden = false;
    } catch (e) {
      // Cosmetic feature: fail silent, keep showing the last good reading.
    }
  }
  pollActiveModels();
  setInterval(pollActiveModels, 2000);

  // --- Sending / Stop ---
  // The Send button doubles as Stop while a response is generating for the
  // currently-viewed session — same button, its current action is whatever
  // it currently says. That lets you interrupt a response, edit the draft
  // you were typing underneath it, and send that instead without waiting.
  function setComposerGenerating(isGenerating) {
    state.generating = isGenerating;
    sendBtn.textContent = isGenerating ? 'Stop' : 'Send';
    sendBtn.classList.toggle('stop-mode', isGenerating);
  }

  async function stopGeneration() {
    const id = state.activeId;
    try {
      await api('/api/sessions/' + id + '/stop', { method: 'POST' });
    } catch (err) {
      // Most likely it already finished on its own right as Stop was
      // clicked — the terminal stream event will have already reset the
      // button, so there's nothing more to do here.
    }
  }

  composer.addEventListener('submit', async (e) => {
    e.preventDefault();
    if (state.generating) {
      stopGeneration();
      return;
    }
    const text = input.value.trim();
    if (!text) return;

    const sessionID = state.activeId;
    const attachments = state.attachments;
    const body = { session_id: sessionID, message: text };
    if (attachments.length) body.attachment_data_uris = attachments;
    input.value = '';
    autoGrow();
    state.attachments = [];
    renderAttachPreview();
    attachInput.value = '';

    // Render the sent message immediately rather than waiting on the live
    // SSE echo — a turn that fails fast (e.g. no matching model installed,
    // which needs no actual inference) can finish before the stream even
    // connects, and without this the message looks like it just vanished.
    // The user_message SSE handler's "already shown" check keys off this
    // same text, so the live event won't double-render it when it does
    // arrive in time.
    appendMessageEl('user', text, attachments, new Date().toISOString());

    try {
      await api('/api/chat', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
    } catch (err) {
      appendMessageEl('assistant', 'Could not send: ' + err.message, [], new Date().toISOString());
      return;
    }
    // The response streams in via this session's live event feed — attach
    // (or re-attach) to it. Nothing here blocks on the response, so sending
    // in one chat and immediately switching to another works exactly as
    // it should: this request keeps running server-side regardless.
    if (state.activeId === sessionID) {
      if (currentStream) currentStream.close();
      currentAssistantBubble = null;
      attachStream(sessionID);
    }
    refreshSessionList();
  });

  function ensureAssistantBubble() {
    if (!currentAssistantBubble) {
      currentAssistantBubble = appendMessageEl('assistant', '', [], new Date().toISOString());
      currentAssistantBubble.classList.add('pending');
    }
    return currentAssistantBubble;
  }

  function handleEvent(evt) {
    switch (evt.type) {
      case 'idle':
        break;
      case 'user_message': {
        // The sender's own tab already rendered this optimistically on
        // submit (see the composer submit handler) — a live event here is
        // either that same message catching up, or this chat being viewed
        // from a different tab/device that never rendered it locally.
        const last = messagesEl.lastElementChild;
        const alreadyShown = last && last.classList.contains('user') &&
          last.querySelector('.bubble').textContent === evt.content;
        if (!alreadyShown) {
          const urls = (evt.image_paths || []).map(p => '/images/' + p);
          appendMessageEl('user', evt.content, urls, new Date().toISOString());
        }
        break;
      }
      case 'model': {
        // Always reflects the latest attempt, not just the first — a turn
        // that falls back to a different model after the first one failed
        // (see 'model_fallback' below) fires this again with the model
        // that's actually generating now, and the tag should track that.
        const bubble = ensureAssistantBubble();
        let tag = bubble.querySelector('.model-tag');
        if (!tag) {
          tag = document.createElement('span');
          tag.className = 'model-tag';
          bubble.insertBefore(tag, bubble.firstChild);
        }
        tag.textContent = evt.name;
        break;
      }
      case 'model_fallback': {
        // The explicitly-selected model (dropdown pick, not Auto) failed to
        // load/generate and a different installed model answered instead —
        // shown so this never looks like the selection was silently
        // ignored (see handlers_chat.go's runTextTurn/runImageTurn).
        const bubble = ensureAssistantBubble();
        if (!bubble.querySelector('.fallback-note')) {
          const note = document.createElement('div');
          note.className = 'fallback-note';
          note.textContent = `⚠ ${evt.requested} didn't respond — answered with ${evt.used} instead.`;
          bubble.insertBefore(note, bubble.firstChild);
        }
        break;
      }
      case 'text_delta': {
        const bubble = ensureAssistantBubble();
        bubble.appendChild(document.createTextNode(evt.text));
        messagesEl.scrollTop = messagesEl.scrollHeight;
        break;
      }
      case 'image_prompt': {
        const bubble = ensureAssistantBubble();
        const note = document.createElement('div');
        note.className = 'caption';
        note.textContent = 'Prompt: ' + evt.prompt;
        bubble.appendChild(note);
        break;
      }
      case 'image': {
        const bubble = ensureAssistantBubble();
        const img = document.createElement('img');
        img.className = 'gen-image';
        img.src = evt.url;
        bubble.appendChild(img);
        bubble.dataset.handled = '1';
        messagesEl.scrollTop = messagesEl.scrollHeight;
        break;
      }
      case 'engine_approval_needed': {
        const bubble = ensureAssistantBubble();
        bubble.dataset.handled = '1';
        suggestionArea.appendChild(approvalCard(evt.component, evt.status));
        bubble.textContent = "Setting up the " + evt.component + " engine for the first time — approve the download above, then resend your message.";
        break;
      }
      case 'no_model': {
        const bubble = ensureAssistantBubble();
        bubble.dataset.handled = '1';
        if (evt.suggestion) {
          suggestionArea.appendChild(suggestionCard(evt.suggestion, 'No ' + evt.kind + ' model installed yet'));
          bubble.textContent = 'No installed model fits this request yet — approve the download above, then resend your message.';
        } else {
          bubble.textContent = 'No installed model fits this request, and no suggestion is available for this hardware.';
        }
        break;
      }
      case 'error': {
        const bubble = ensureAssistantBubble();
        bubble.dataset.handled = '1';
        bubble.textContent = evt.message;
        break;
      }
      case 'cancelled': {
        // The backend saves a plain "(stopped)" to history on cancellation
        // (no partial text) — mirrored here rather than left inconsistent
        // with whatever partial text this live view happened to already
        // show before Stop was clicked.
        if (currentAssistantBubble) {
          currentAssistantBubble.classList.remove('pending');
          currentAssistantBubble.dataset.handled = '1';
          const note = document.createElement('div');
          note.className = 'caption';
          note.textContent = '(stopped)';
          currentAssistantBubble.appendChild(note);
          refreshMessageTime(currentAssistantBubble);
        }
        currentAssistantBubble = null;
        break;
      }
      case 'done': {
        if (currentAssistantBubble) {
          currentAssistantBubble.classList.remove('pending');
          if (!currentAssistantBubble.textContent && !currentAssistantBubble.querySelector('img')) {
            currentAssistantBubble.parentElement.remove();
          } else {
            refreshMessageTime(currentAssistantBubble);
          }
        }
        currentAssistantBubble = null;
        break;
      }
    }
  }

  boot().catch(err => {
    messagesEl.innerHTML = '<div class="system-note">Failed to load: ' + err.message + '</div>';
  });
})();
