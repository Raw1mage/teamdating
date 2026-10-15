// 行程統合 — 零依賴前端（DD-17：原生 ES module、手寫 CSS、無框架/打包器/CDN）
// 後端契約掛在 base-path 底下，故 API 前綴由 document.baseURI 推導，不寫死 /api。
const base = new URL('.', document.baseURI).pathname.replace(/\/$/, '');
const api = p => `${base}/api${p}`;
const $ = id => document.getElementById(id);
const e = (t, c, x) => { const n = document.createElement(t); if (c) n.className = c; if (x != null) n.textContent = x; return n; };

const S = {
  // ★ email 進 me 而不是另存一份：它是「我的身分的一個欄位」，第二份副本只會
  //   在改了 email 卻忘了同步時跟後端不一致，而畫面不會報錯。
  me: { registered: false, uid: '', name: '', email: '' },
  recovery: '',
  // 撞名流程的暫存：使用者剛才想登記的名字。改名/接受編號都要用它重送。
  pendingName: '',
  route: null,        // pid or null
  proposal: null,     // { pid, ownerUid, ownerName, title, desc, options:{}|[], createdAt }
  presence: [],       // [{ uid, name }]
  es: null,
  seq: 0,
};

const PALETTE = ['#d85338','#176e5b','#c98a1a','#3f6fb0','#8046c4','#b8397f','#4e8a2f','#0f766e','#a3562b','#5c57c9'];
const colorOf = k => { if (!k) return '#9a958b'; let h = 0; for (const c of k) h = (c.codePointAt(0) + ((h << 5) - h)) | 0; return PALETTE[Math.abs(h) % PALETTE.length]; };
// 依背景色亮度自適應字色：淺底用深字、深底用白字（解決淺色背景配白字閱讀困難）。
// hex → 感知亮度（WCAG 相對亮度近似），> 0.6 視為淺底。
const inkOn = hex => { const m = /^#?([\da-f]{6})$/i.exec(hex || ''); if (!m) return '#fff'; const n = parseInt(m[1], 16), r = (n >> 16) & 255, g = (n >> 8) & 255, b = n & 255; const lum = (0.299 * r + 0.587 * g + 0.114 * b) / 255; return lum > 0.6 ? '#1c1a17' : '#fff'; };

// ─── 快照正規化：GET 回 options=array，SSE snapshot 回 options=map。統一成 array。 ───
function normProposal(p) {
  if (!p) return p;
  const toArr = v => Array.isArray(v) ? v : (v && typeof v === 'object' ? Object.values(v) : []);
  const opts = toArr(p.options).map(o => ({ ...o, voters: o.voters || [], voteCount: o.voteCount ?? (o.voters || []).length }));
  return { ...p, options: opts };
}
const opts = () => S.proposal?.options || [];
const myUid = () => S.me.uid;
const votersOf = o => o.voters || [];
const humanName = u => typeof u?.name === 'string' ? u.name.trim() : '';
// ★ 已表態 ≠ 支持。myWeight 是指標語意：null＝沒表態，0＝明確表示不行。
// 用 votersOf().some() 判斷會把「表態不行」也算成一票支持，而畫面不會報錯。
const myStance = o => (o.myWeight === undefined || o.myWeight === null) ? null : o.myWeight;
const iResponded = o => myStance(o) !== null;
const iVoted = o => (myStance(o) ?? 0) > 0;
const days = (a, b) => Math.round((new Date(b + 'T00:00:00') - new Date(a + 'T00:00:00')) / 86400000) + 1;
const ymd = d => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;

// ─── HTTP ───
async function req(p, o = {}) {
  const r = await fetch(api(p), { credentials: 'same-origin', headers: { 'Content-Type': 'application/json', accept: 'application/json' }, ...o });
  // ★ 錯誤碼要帶在 Error 上，不能只留訊息文字。呼叫端要區分「未設 email 被擋」
  //   與其他失敗，而訊息文字是給人看的——拿它做分支判斷等於把 UI 行為綁在一串
  //   隨時可能改的中文上，改一個字就静默失效。
  if (!r.ok) {
    let m = `HTTP ${r.status}`, code = '';
    try { const j = await r.json(); m = j.msg || j.code || m; code = j.code || ''; } catch {}
    const err = new Error(m); err.code = code; err.status = r.status; throw err;
  }
  return r.status === 204 ? null : r.json();
}
function toast(x, bad = false) { const n = e('div', `toast${bad ? ' error' : ''}`, x); $('toast').append(n); setTimeout(() => n.remove(), 3200); }
function gate(x) { $('gate-title').textContent = '無法連線到後端'; $('gate-body').textContent = x; $('gate').hidden = false; }

// ─── 路由 ───
const go = h => { location.hash = h; };
function route() {
  if (S.es) { S.es.close(); S.es = null; }
  // #/p/<pid>        → 詳情頁
  // #/p/<pid>/cal    → 該提案的獨立全螢幕月曆頁（手機優先視覺化）
  const mc = location.hash.match(/^#\/p\/([^/]+)\/cal$/);
  const m = mc || location.hash.match(/^#\/p\/([^/]+)$/);
  S.route = m ? m[1] : null;
  S.view = mc ? 'cal' : (m ? 'detail' : 'list');
  S.proposal = null; S.presence = [];
  render();
}

function render() {
  $('view-list').hidden = S.view !== 'list';
  // 可選項（detail）與日曆（cal）共用同一個 view-detail 殼（含共用第二 header）；
  // 兩模式只差在下方 detail-body 內顯示哪個主體（pane-options / pane-calendar）。
  const inProposal = S.view === 'detail' || S.view === 'cal';
  $('view-detail').hidden = !inProposal;
  if (inProposal) {
    $('pane-options').hidden = S.view !== 'detail';
    $('pane-calendar').hidden = S.view !== 'cal';
    // 模式 class：日曆主體需要比可選項閱讀寬度更寬的容器（對齊日曆原設計寬度）。
    $('view-detail').classList.toggle('is-cal', S.view === 'cal');
    // 雙向切換鈕：在可選項模式顯示「📅 日曆」（點了去日曆），
    //   在日曆模式顯示「☰ 可選項」（點了回可選項），來回切換。
    setText($('b-mode-toggle'), S.view === 'cal' ? '☰ 可選項' : '📅 日曆');
  }
  // 齒輪（提案設定）只在提案內頁顯示；分享/切換已歸第二 header。
  $('head-actions').hidden = !inProposal;
  // 返回鍵只在有上一層可回時顯示：列表頁（首頁）無得返回，故隱藏。
  $('nav-back').hidden = S.view === 'list';
  renderMe();
  if (S.view === 'list') { $('crumb').textContent = '行程統合'; loadList(); return; }
  // 詳情頁與月曆頁都需要提案資料 + SSE；差別只在渲染哪個主體。
  $('crumb').textContent = S.view === 'cal' ? '月曆' : '';
  openDetail();
}
function renderMe() {
  if (document.activeElement !== $('me-name')) $('me-name').value = S.me.name || '';
}

// ─── 第一層：只有自己參與過的（無全域列表，R8/5.3）───
async function loadList() {
  try {
    const res = await req('/my/proposals');
    const list = Array.isArray(res) ? res : (res.proposals || []);
    const b = $('proposal-list'); b.replaceChildren();
    if (!list.length) { b.append(e('div', 'card proposal-card', '還沒有參與過的提案。按「＋ 新增提案」建立，或開啟別人分享的網址。')); return; }
    list.forEach(p => {
      const c = e('article', 'card proposal-card'); c.style.borderLeftColor = colorOf(p.pid);
      c.append(e('h3', '', p.title));
      if (p.desc) c.append(e('p', 'sub', p.desc));
      const st = e('div', 'proposal-stats');
      st.append(e('span', 'chip', `${(Array.isArray(p.options) ? p.options : Object.values(p.options || {})).length} 個可選項`));
      st.append(e('span', 'chip', `發起：${p.ownerName || ''}`));
      c.append(st);
      c.onclick = () => go(`#/p/${p.pid}`);
      b.append(c);
    });
  } catch (x) { gate(x.message); }
}

// ─── 第二層：先取 snapshot 再連 SSE ───
async function openDetail() {
  try {
    S.proposal = normProposal(await req(`/proposals/${S.route}`));
    renderDetailFull();
    maybeOwnerTakeover();
    // 直接開 #/p/<pid>/cal 網址（分享情境）時，view 已是 'cal'，
    // 需要在此完成月曆頁的首次渲染；renderDetailFull 只畫詳情頁的節點。
    if (S.view === 'cal') renderCalendarPage();
    connectSSE();
  } catch (x) { toast(x.message, true); go('#/'); }
}

function connectSSE() {
  if (S.es) S.es.close();
  const es = new EventSource(api(`/proposals/${S.route}/stream`));
  S.es = es;
  // 後端用具名事件（event: snapshot / delta / presence / cursor …）
  // ★ snapshot 會用全新物件替換 S.proposal（含 options 陣列裡每一個 o）。若當前在
  //   月曆頁，日曆 bar 的 onclick closure 仍捕捉著 openDetail 首渲染時那批「舊 o」，
  //   而 renderDetailFull 只重畫詳情頁、不動日曆——於是單擊改的是舊 o、
  //   patchCalendarStance 讀的是新 o，兩者 oid 相同但不是同一物件，樂觀更新永遠
  //   patch 不到視覺（實測：vote 有送、server 有記、bar class 停在 dim，reload 才對）。
  //   在月曆頁時用新 S.proposal 重建日曆，讓 bar 與其 closure 都指向新 o。
  es.addEventListener('snapshot', ev => { const d = JSON.parse(ev.data); S.seq = d.seq || 0; S.proposal = normProposal(d.proposal); S.presence = d.presence || []; renderDetailFull(); if (S.view === 'cal') renderCalendarPage(); });
  const incr = ev => { const d = JSON.parse(ev.data); applyDelta(d); };
  // ★ 這份清單必須涵蓋後端 store 的每一個 Ev* 常數。漏掉一個不會報錯——
  //   後端照送、前端沒掛、es.onmessage 對具名事件也不觸發，於是那類變更
  //   靜默地不更新畫面。已由 Go 端的 source guard 測試釘住（見
  //   internal/httpapi/source_guard_test.go：TestFrontendHandlesEveryEventType）。
  const SERVER_EVENTS = ['delta',
    'user_create', 'user_rename',
    // 一 user 多 token：新增/撤銷/續期一把鑰匙。這三類不改變畫面上任何內容，
    // 但仍必須掛上——SERVER_EVENTS 的契約是「涵蓋後端每一個 Ev* 常數」，
    // 而「掛了但沒事發生」與「漏掛」在畫面上長得一樣，只有這份清單分得出來。
    'user_add_token', 'user_revoke_token', 'user_touch_token',
    // email 設定同樣不改變畫面內容，掛上是為了守住上面那條契約：
    // 後端每多一個 Ev* 常數，這裡就必須跟上，否則「掛了但沒事發生」與
    // 「漏掛」在畫面上無從分辨。
    'user_set_email',
    // user_merge 確實會改畫面：被吸收的身分離開名單、票歸給存活者、
    // 行程擁有權轉移。上面幾行是「掛了但不改畫面」，這一行不是。
    'user_merge',
    'proposal_create', 'proposal_update', 'proposal_delete', 'proposal_decide', 'proposal_repid',
    'option_create', 'option_update', 'option_delete',
    'vote_set', 'vote_delete'];
  SERVER_EVENTS.forEach(t => es.addEventListener(t, incr));
  // cursor 事件刻意不掛：詳情頁的內嵌月曆（座標畫布）已移除，手機上無滑鼠游標
  //   可畫，故前端不消費 cursor SSE。後端 cursor 子系統（handleCursor + hub 廣播
  //   + 專屬 rate limiter）仍在但前端不再觸發 POST /cursor，流量自然歸零。這是
  //   刻意的「後端有、前端不接」，不是 SERVER_EVENTS 漏掛的缺陷。
  // presence 有兩種形狀：snapshot 給整包、hub 給單筆 delta {uid,online}。
  // 舊寫法的 `d.presence || []` 對單筆 delta 會落到 []，於是「有人加入」
  // 反而清空整份在線名單——錯誤方向與正確方向相反且不報錯。
  es.addEventListener('presence', ev => {
    const d = JSON.parse(ev.data);
    const whole = d.presence || d.list;
    if (Array.isArray(whole)) S.presence = whole;
    else if (d.uid) {
      const rest = S.presence.filter(x => x.uid !== d.uid);
      S.presence = d.online ? [...rest, { uid: d.uid, name: d.name || '' }] : rest;
    }
    renderPresence();
  });
  // 相容：若後端把增量走預設 message 通道
  es.onmessage = ev => { try { const d = JSON.parse(ev.data); if (d.proposal) { S.proposal = normProposal(d.proposal); S.presence = d.presence || []; renderDetailFull(); } else applyDelta(d); } catch {} };
}

// ─── diff 渲染的核心：增量事件只重取快照到記憶體，再做「節點級」更新，
//     日曆與可選項清單都保留捲動位置，絕不整片 innerHTML='' 重建（DD-12 / 5.8）。───
async function applyDelta(d) {
  // 後端增量事件形狀未完全固定；以「重取快照 + 節點級 diff 更新」保證正確性且不跳動。
  try {
    const fresh = normProposal(await req(`/proposals/${S.route}`));
    S.proposal = fresh;
    patchDetail();   // ← 節點級更新，不重建 DOM 樹、不動捲動位置
    // ★ patchDetail 只碰詳情頁節點；在月曆頁時日曆 bar 的三態 class 也必須跟著重畫，
    //   否則「別人投票」的 delta 到達後，我的日曆線條停在舊狀態直到 reload（與
    //   snapshot 漏畫日曆同族的缺陷）。patchCalendarStance 是節點級、保留捲動位置。
    if (S.view === 'cal') patchCalendarStance();
  } catch {}
}

// 首次 / snapshot：建立完整 DOM。
function renderDetailFull() {
  const p = S.proposal; if (!p) return;
  $('d-title').textContent = p.title;
  $('d-desc').textContent = p.desc || ''; $('d-desc').hidden = !p.desc;
  renderGear(p);
  renderPresence();
  renderOptionsFull();
}

// 增量：只改動變動的節點，保留 scrollTop / hover。
//
// ★ 標題與說明**必須**在這裡更新，不能只留在 renderDetailFull()。
//   2026-08-05 實測的真缺陷：proposal_update 事件後端有推、前端有掛 listener、
//   applyDelta() 也確實重取到新標題的快照 —— 但 patchDetail() 從不寫這兩個節點，
//   於是改名對其他人的畫面永遠不生效，直到他重整或離開再進來。
//   source guard 保證的是「每個事件都有人接」，接了之後有沒有畫是另一回事，
//   而兩者共用「不報錯」這一個輸出。setText 只在值不同時寫，維持節點級語意。
function patchDetail() {
  const p = S.proposal; if (!p) return;
  setText($('d-title'), p.title || '');
  const desc = $('d-desc');
  setText(desc, p.desc || '');
  desc.hidden = !p.desc;
  $('d-meta').textContent = `發起人 ${p.ownerName || ''} · ${opts().length} 個可選項`;
  renderGear(p);
  patchOptions();
}

// 齒輪可見性是 **逐項** 的，不是整個選單一起開關。
//
// ★ 身分設定 / 復原碼是「我的帳號」，任何登入者都該碰得到；
//   更換分享網址 / 刪除提案是「這個行程」，只有 owner 能碰。
//   舊寫法把整個 wrap 綁在 owner 上，於是非 owner 連自己的身分設定都進不去。
//   而直接把 wrap 改成「登入即可見」則走到另一個極端：非 owner 會看到
//   「刪除提案」。伺服器仍會擋（A6），所以不是安全破口——但那是 UI 謊言：
//   給人看得到、按不動的破壞性按鈕。
function renderGear(p) {
  const wrap = $('gear-wrap');
  if (!wrap) return;
  const me = myUid();
  const isOwner = !!me && p.ownerUid === me;
  wrap.hidden = !me;
  const setItem = (id, on) => { const n = $(id); if (n) n.hidden = !on; };
  setItem('me-identity', !!me);
  setItem('me-recovery', !!me);
  setItem('b-repid', isOwner);
  setItem('b-del-proposal', isOwner);
  if (!me) closeGear();
}

// 值相同就不寫 DOM——避免抹掉使用者的文字選取與觸發不必要的重排。
function setText(node, v) { if (node.textContent !== v) node.textContent = v; }

// ── 定案（6.7 / DD-5）──
// isDecided 讀的是伺服器回報的 decidedOptionId。前端不自行推導凍結狀態：
// 任何客戶端推導都會在 SSE 抵達前的那一瞬間與伺服器不一致，而使用者看到的
// 「還能按」與伺服器的 403 是兩個不同的事實。
const isDecided = () => !!(S.proposal && S.proposal.decidedOptionId);
const decidedOid = () => (S.proposal && S.proposal.decidedOptionId) || '';

function renderPresence() {
  // 在線 dot 已依使用者要求從詳情頁移除；後端仍送 presence 事件、S.presence 仍更新，
  // 但無 DOM 容器可畫，故 null-guard 直接返回（避免對已移除的 #presence 呼叫 replaceChildren）。
  const h = $('presence'); if (!h) return;
  S.presence.forEach(x => { const d = e('span', 'dot'); d.style.background = colorOf(x.uid); d.title = x.name || x.uid; h.append(d); });
}

// ── 可選項清單 ──
function optionNode(o) {
  const c = e('article', `option${iVoted(o) ? ' mine' : ''}`); c.dataset.oid = o.oid;
  const top = e('div', 'option-top');
  // ★ 兩態分開顯示，不折成一個分數（DD-6）：「一人可以」與「一人可以＋三人不行」
  //   分數都是 1，而提案人必須分得出這案是沒人注意還是被否決。
  const y = o.yes ?? 0, n = o.no ?? 0;
  const sc = o.score ?? (o.voteCount ?? votersOf(o).length);
  top.append(e('strong', '', `${o.startDate} → ${o.endDate}`), e('span', 'muted', `${sc} 分`));
  c.append(top);
  const tally = e('div', 'tally');
  tally.append(e('span', 'st st-yes', `可以 ${y}`), e('span', 'st st-no', `不行 ${n}`));
  c.append(tally);
  // 表態者名單：可以標 (o)、不行標 (x)。未表態者不列入。
  const who = votersOf(o).map(v => `${v.name || v.uid}(${v.weight === 0 ? 'x' : 'o'})`).join('、');
  c.append(e('div', 'sub', `${days(o.startDate, o.endDate)} 天${o.note ? ' · ' + o.note : ''} · ${o.authorName || ''} 提出`));
  c.append(e('div', 'sub', who || '尚無人表態'));
  const a = e('div', 'option-actions');
  const frozen = isDecided();
  if (o.decided || o.oid === decidedOid()) {
    c.classList.add('decided');
    top.append(e('span', 'badge-decided', '★ 定案'));
  }
  // 兩顆表態鈕，目前表態的那顆標 on。再按同一顆＝收回表態（DELETE）。
  const cur = myStance(o);
  [[1, '可以'], [0, '不行']].forEach(([w, label]) => {
    const b = e('button', `vote st-btn${cur === w ? ' on' : ''}`, cur === w ? `✓ ${label}` : label);
    b.onclick = () => vote(o, w);
    if (frozen) { b.disabled = true; b.title = '提案已定案，投票已凍結'; }
    a.append(b);
  });
  if (o.authorUid === myUid() && (o.canEdit ?? true) && !frozen) {
    const del = e('button', 'btn btn-danger-ghost', '刪除');
    del.onclick = () => delOption(o);
    a.append(del);
  }
  if (S.proposal && S.proposal.ownerUid === myUid()) {
    const db = e('button', 'btn', o.decided ? '解除定案' : '設為定案');
    db.onclick = () => decide(o.decided ? '' : o.oid);
    a.append(db);
  }
  c.append(a);
  return c;
}

async function decide(oid) {
  try {
    await req(`/proposals/${S.route}/decision`, { method: 'POST', body: JSON.stringify({ optionId: oid }) });
    toast(oid ? '已標記定案，投票已凍結' : '已解除定案');
  } catch (x) { toast(x.message, true); }
}
function renderOptionsFull() {
  $('o-count').textContent = `${opts().length} 個`;
  const h = $('option-list'); h.replaceChildren();
  if (!opts().length) { h.append(e('p', 'sub', '還沒有可選項。按上方按鈕新增一段日期。')); }
  else opts().forEach(o => h.append(optionNode(o)));
  renderNotVoted();
}
// 節點級：只替換內容變動的 option 卡、移除已刪的、附加新的，保留清單捲動位置。
function patchOptions() {
  $('o-count').textContent = `${opts().length} 個`;
  const h = $('option-list');
  const seen = new Set();
  opts().forEach(o => {
    seen.add(o.oid);
    const old = h.querySelector(`[data-oid="${o.oid}"]`);
    const fresh = optionNode(o);
    if (old) old.replaceWith(fresh); else h.append(fresh);
  });
  h.querySelectorAll('[data-oid]').forEach(n => { if (!seen.has(n.dataset.oid)) n.remove(); });
  if (!opts().length && !h.querySelector('.sub')) h.append(e('p', 'sub', '還沒有可選項。'));
  renderNotVoted();
}
// 表態後的重繪：列表一定要 patch；若當前在月曆頁，日曆 overlay 橫條的三態
// class 也必須跟著重畫，否則點了 bar 只有列表變、日曆線條的勾/叉/未選永遠停在
// 舊狀態（使用者實測「打勾永遠打勾」的根因）。renderCalendarPage 自帶 scrollTop
// 保留，在日曆頁重繪不跳位。
function patchVotes() {
  patchOptions();
  if (S.view === 'cal') patchCalendarStance();
}
// 表態後只「節點級」改日曆 bar 的三態 class + label，絕不 replaceChildren 重建。
// 根因（真人「要點兩下」）：renderCalendarPage 同步重建會在 click 事件派發途中
// 銷毀被點的 bar，使 mousedown-target ≠ mouseup-target，瀏覽器判定 click 不成立，
// onclick 不觸發（DD-12「絕不整片重建」也正是為此）。改成原地改 class 後，被點的
// 元素恆存活，單擊必成立。
function patchCalendarStance() {
  const over = $('calpage-scroll'); if (!over) return;
  calValidOpts().forEach(o => {
    const s = myStance(o), cls = s === 1 ? 'yes' : s === 0 ? 'no' : 'dim';
    const yesTag = `${o.yes ?? 0}人`;
    over.querySelectorAll(`.calpage-bar[data-oid="${o.oid}"]`).forEach(bar => {
      bar.classList.remove('yes', 'no', 'dim'); bar.classList.add(cls);
      // 段首（data-realstart）顯示完整同款；其餘格只顯示標記＋人數。與 monthBlockPage 一致。
      bar.textContent = bar.dataset.realstart
        ? [stanceMark(o), yesTag, o.note || o.authorName || ''].filter(Boolean).join(' ')
        : `${stanceMark(o)} ${yesTag}`;
    });
  });
}
function renderNotVoted() {
  // ★ 「還沒表態」問的是有沒有票列，不是有沒有投「可以」。表態不行的人
  //   已經回答過了，把他們列進待催名單等於要求他們再答一次同一題。
  const responded = new Set(opts().flatMap(o => votersOf(o).map(v => v.uid || v)));
  const names = S.presence.filter(u => !responded.has(u.uid)).map(humanName).filter(Boolean);
  $('not-voted-box').hidden = names.length === 0;
  $('not-voted').textContent = names.join('、');
}

// ── 連續多月捲動日曆 + 軌道對齊（5.6/5.7）──
function assignLanes(os) {
  const lanes = [], map = new Map();
  [...os].sort((a, b) => a.startDate.localeCompare(b.startDate) || a.endDate.localeCompare(b.endDate)).forEach(o => {
    let i = lanes.findIndex(last => last < o.startDate);
    if (i === -1) { i = lanes.length; lanes.push(o.endDate); } else lanes[i] = o.endDate;
    map.set(o.oid, i);
  });
  return { map, count: Math.max(1, lanes.length) };
}

// ─── 獨立全螢幕月曆頁（手機優先視覺化，#/p/<pid>/cal）───
//
// ★ 防呆日期缺值是這個函式的第一等公民。舊內嵌月曆的 regression 根因：
//   buildCalendar 用 marks.sort()[0] 當月份起點，只要任一選項的 startDate/
//   endDate 是空/undefined，new Date('undefinedT00:00:00')=Invalid Date，
//   迴圈 cur<end 恆 false → 一格都不畫；assignLanes 的 localeCompare 對
//   undefined 直接 throw。兩者都讓「有選項」與「日期壞掉」共用「整片空白」
//   這一個輸出。這裡先過濾出日期完整的選項，壞資料不參與範圍計算。
const calValidOpts = () => opts().filter(o =>
  typeof o.startDate === 'string' && /^\d{4}-\d{2}-\d{2}$/.test(o.startDate) &&
  typeof o.endDate === 'string' && /^\d{4}-\d{2}-\d{2}$/.test(o.endDate) &&
  o.startDate <= o.endDate);

function renderCalendarPage() {
  const p = S.proposal; if (!p) return;
  // 日曆標題已改由共用第二 header 的 d-title 顯示；原日曆頁的 calpage-title h1 已隨 view-calendar 刪除。
  const os = calValidOpts();
  const empty = $('calpage-empty'), scroll = $('calpage-scroll');
  if (!os.length) { empty.hidden = false; scroll.replaceChildren(); return; }
  empty.hidden = true;
  const h = scroll; const keep = h.scrollTop;
  h.replaceChildren(buildCalendarPage(os));
  // 增量更新保留容器內捲動位置；首次進入「不」自動捲動——
  // 日曆現為 view-detail 內主體，上方有 header1/header2 需置頂保留，
  // 舊的 scrollIntoView({block:'start'}) 會捲整個文件把兩個 header 頂出畫面。
  if (keep) h.scrollTop = keep;
}

// 月曆頁自己的建構器：與內嵌 buildCalendar 同構，但 (1) 只吃已過濾的乾淨選項，
// (2) 色塊更大、點整格即可對該日活躍選項表態，(3) 不畫游標圖層（獨立頁無共游標需求）。
function buildCalendarPage(os) {
  const frag = document.createDocumentFragment();
  const { map: lane, count } = assignLanes(os);
  const marks = [ymd(new Date()), ...os.map(o => o.startDate), ...os.map(o => o.endDate)].sort();
  const first = new Date(marks[0] + 'T00:00:00'), last = new Date(marks[marks.length - 1] + 'T00:00:00');
  let cur = new Date(first.getFullYear(), first.getMonth(), 1);
  const end = new Date(last.getFullYear(), last.getMonth() + 1, 1);
  const today = ymd(new Date());
  for (; cur <= end; cur = new Date(cur.getFullYear(), cur.getMonth() + 1, 1))
    frag.append(monthBlockPage(cur.getFullYear(), cur.getMonth(), os, lane, count, today));
  return frag;
}

// 三態循環：未選(null) → 可以(1) → 不行(0) → 未選。
// vote(o, w) 本身「再按同態=收回」，所以循環只需選對目標 weight：
//   目前 null → 投 1；目前 1 → 投 0；目前 0 → 投 0（=同態，觸發收回）。
function cycleStance(o) {
  const cur = myStance(o);
  if (cur === null) return vote(o, 1);   // 未選 → 可以
  if (cur === 1) return vote(o, 0);      // 可以 → 不行
  return vote(o, 0);                     // 不行 → 收回（再按同態）
}
// 每個可選項目前表態的標記與標題（實心色塊上直接看得到，不再是空白）。
const stanceMark = o => { const s = myStance(o); return s === 1 ? '✓' : s === 0 ? '✗' : '·'; };

function monthBlockPage(y, m, os, lane, count, today) {
  const w = e('section', 'calpage-month'); w.dataset.month = `${y}-${String(m + 1).padStart(2, '0')}`;
  w.append(e('div', 'calpage-mtitle', `${y} 年 ${m + 1} 月`));
  const wd = e('div', 'calpage-weekdays'); ['日', '一', '二', '三', '四', '五', '六'].forEach(x => wd.append(e('div', '', x))); w.append(wd);
  // ── 週列 + overlay 橫條渲染（取代舊的逐格 seg）──
  // 舊法逐格畫 seg，CSS grid 每格是獨立 item，seg 吃不到格間 gap → 線條斷成每日一格。
  // 新法：逐「週列」渲染，每週一個 relative 容器；每個行程區間在它涵蓋的每週內
  // 畫「一條」absolute 橫條（left/width 用該週佔格比例算），多日自然連成一條連續線；
  // 跨週界時同一區間在下一週另起一段（自然斷開換行）。lane 垂直堆疊避免重疊互蓋。
  const lead = new Date(y, m, 1).getDay(), n = new Date(y, m + 1, 0).getDate();
  const g = e('div', 'calpage-weeks');
  const iso = d => `${y}-${String(m + 1).padStart(2, '0')}-${String(d).padStart(2, '0')}`;
  const LANE_H = 30, LANE_TOP = 24;   // 每條 lane 高度、日期數字下方起始位移（放寬讓多條線不擠）
  // slot 0..(lead+n-1) 依序對應：前導空格 + 當月各日。切成每 7 個一列。
  const totalSlots = lead + n, weeks = Math.ceil(totalSlots / 7);
  for (let wk = 0; wk < weeks; wk++) {
    const week = e('div', 'calpage-week');
    const cells = e('div', 'calpage-wrow');       // 背景 7 格
    for (let col = 0; col < 7; col++) {
      const slot = wk * 7 + col, day = slot - lead + 1;
      if (slot < lead || day > n) { cells.append(e('div', 'calpage-wcell pad')); continue; }
      const ds = iso(day);
      const c = e('div', 'calpage-wcell' + (ds === today ? ' today' : ''));
      c.append(e('div', 'calpage-day', String(day)));
      cells.append(c);
    }
    week.append(cells);
    // overlay：畫本週內每個區間的橫條
    const over = e('div', 'calpage-over');
    // 本週涵蓋的日期範圍
    const wkFirstDay = wk * 7 - lead + 1, wkLastDay = Math.min(wkFirstDay + 6, n);
    const wkStart = Math.max(wkFirstDay, 1);
    os.forEach(o => {
      // 此區間與本週的交集（用日字串比較，restrict 到當月當週）
      const segStartDay = Math.max(wkStart, dayOfMonthOr(o.startDate, y, m, wkStart));
      const segEndDay = Math.min(wkLastDay, dayOfMonthOr(o.endDate, y, m, wkLastDay));
      // o 是否真的涵蓋本週任一天
      const sFirst = iso(Math.max(wkStart, 1)), sLast = iso(wkLastDay);
      if (o.endDate < sFirst || o.startDate > sLast) return;    // 本週無交集
      const a = Math.max(segStartDay, wkStart), b = Math.min(segEndDay, wkLastDay);
      if (b < a) return;
      const startCol = (a - 1 + lead) % 7, span = b - a + 1;
      const li = lane.get(o.oid) || 0;
      const s = myStance(o), cls = s === 1 ? ' yes' : s === 0 ? ' no' : ' dim';
      const bg = colorOf(o.oid);
      const isRealStart = o.startDate >= sFirst;    // 區間真起點落在本週 → 左圓角
      const isRealEnd = o.endDate <= sLast;         // 區間真終點落在本週 → 右圓角
      const yesTag = `${o.yes ?? 0}人`;
      const label = isRealStart ? [stanceMark(o), yesTag, o.note || o.authorName || ''].filter(Boolean).join(' ') : `${stanceMark(o)} ${yesTag}`;
      const bar = e('div', `calpage-bar${cls}`, label);
      bar.dataset.oid = o.oid;              // 穩定識別：讓 patchCalendarStance 節點級更新三態，不重建 DOM
      bar.dataset.realstart = isRealStart ? '1' : '';   // 段首(顯示 note)才需要，patch label 時要重建同款
      bar.style.left = `${startCol / 7 * 100}%`;
      bar.style.width = `${span / 7 * 100}%`;
      bar.style.top = `${LANE_TOP + li * LANE_H}px`;
      bar.style.background = bg;
      bar.style.color = inkOn(bg);
      if (!isRealStart) bar.style.borderTopLeftRadius = bar.style.borderBottomLeftRadius = '0';
      if (!isRealEnd) bar.style.borderTopRightRadius = bar.style.borderBottomRightRadius = '0';
      bar.title = `${o.startDate} → ${o.endDate}（${days(o.startDate, o.endDate)}天）\n${o.authorName || ''} · ${o.score ?? 0} 分（可以 ${o.yes ?? 0}／不行 ${o.no ?? 0}）\n點擊循環：可以 → 不行 → 未選`;
      bar.onclick = () => cycleStance(o);
      over.append(bar);
    });
    over.style.minHeight = `${LANE_TOP + count * LANE_H + 4}px`;
    week.append(over);
    g.append(week);
  }
  w.append(g); return w;
}
// 把 ISO 日期字串換算成「當月的日」；不在當月則回退給定的界限值。
function dayOfMonthOr(dateStr, y, m, fallback) {
  const parts = (dateStr || '').split('-');
  if (parts.length !== 3) return fallback;
  const yy = +parts[0], mm = +parts[1] - 1, dd = +parts[2];
  if (yy !== y || mm !== m) return fallback;
  return dd;
}

// ─── 投票（樂觀更新 + 失敗回滾）───
// weight: 1 可以 / 0 不行。再按同一顆＝收回表態（DELETE）。
async function vote(o, weight = 1) {
  if (!S.me.registered) { toast('請先在右上角登記名字', true); $('me-name').focus(); return; }
  // 凍結時直接早退：樂觀更新後再被 403 回滾，畫面會閃一下票數然後彈回，
  // 使用者看到的是「投進去又被吃掉」而不是「這個提案已定案」。
  if (isDecided()) { toast('這個提案已經定案，投票已凍結', true); return; }
  const prev = myStance(o);
  const prevVoters = votersOf(o).slice();
  const clear = prev === weight;   // 按下當前表態 ⇒ 收回
  const me = { uid: myUid(), name: S.me.name, weight };
  // 樂觀：先在記憶體改，立即 patch。三態計數一併重算，否則畫面會出現
  // 「按鈕已切換但計數未動」的中間態，看起來像沒生效。
  o.voters = clear ? prevVoters.filter(v => (v.uid || v) !== myUid())
                   : [...prevVoters.filter(v => (v.uid || v) !== myUid()), me];
  o.myWeight = clear ? null : weight;
  recount(o);
  patchVotes();
  try {
    await req(`/proposals/${S.route}/votes${clear ? '/' + o.oid : ''}`, {
      method: clear ? 'DELETE' : 'POST',
      body: clear ? undefined : JSON.stringify({ optionId: o.oid, weight })
    });
  } catch (x) {
    o.voters = prevVoters; o.myWeight = prev; recount(o);
    patchVotes();
    toast('表態失敗：' + x.message, true);
  }
}

// recount 由 voters 重算三態與分數，讓樂觀更新與伺服器用同一套算式。
function recount(o) {
  const vs = votersOf(o);
  o.yes = vs.filter(v => (v.weight ?? 1) === 1).length;
  o.no = vs.filter(v => v.weight === 0).length;
  o.score = vs.reduce((a, v) => a + (v.weight ?? 1), 0);
  o.voteCount = vs.length;
}
async function delOption(o) {
  if (!confirm('刪除這個可選項？')) return;
  try { await req(`/proposals/${S.route}/options/${o.oid}`, { method: 'DELETE' }); toast('已刪除'); }
  catch (x) { toast(x.message, true); }
}

// ─── modal / 事件 ───
function closeAll() { document.querySelectorAll('.modal').forEach(m => m.hidden = true); }
document.querySelectorAll('[data-close]').forEach(b => b.onclick = closeAll);
document.querySelectorAll('.modal').forEach(m => m.addEventListener('mousedown', ev => { if (ev.target === m) m.hidden = true; }));
document.addEventListener('keydown', ev => { if (ev.key === 'Escape') closeAll(); });

$('nav-home').onclick = () => go('#/');
$('nav-back').onclick = () => go('#/');
$('gate-retry').onclick = () => location.reload();

// ─── 齒輪下拉選單（提案設定：更換網址 / 刑除）───
function closeGear() { const m = $('gear-menu'); if (m) { m.hidden = true; $('b-gear').setAttribute('aria-expanded', 'false'); } }
$('b-gear').onclick = (ev) => {
  ev.stopPropagation();
  const m = $('gear-menu'), open = m.hidden;
  m.hidden = !open;
  $('b-gear').setAttribute('aria-expanded', String(open));
};
// 點選單以外的任何地方收合（選單項自己的 onclick 在下方已各自 confirm+動作）
document.addEventListener('click', ev => { if ($('gear-wrap') && !$('gear-wrap').contains(ev.target)) closeGear(); });
// 模式雙向切換（可選項 ⇄ 日曆）：日曆模式點了回可選項，可選項模式點了去日曆。
// 兩模式共用同一第二 header，切換只換下方主體（經 route → S.view → render）。
// 返回上一層統一交給全域 header 的 nav-back（與可選項模式一致）。
$('b-mode-toggle').onclick = () => {
  if (!S.route) return;
  go(S.view === 'cal' ? `#/p/${S.route}` : `#/p/${S.route}/cal`);
};

// 同名身分挑選：同一個名字底下有多個既有身分時，列出來讓本人認領。
//
// ★ 這裡不能静默接一個。兩個同名身分在「能不能登入」上完全等價，所以接錯了
//   不會有任何錯誤——使用者只會看到一個空的 app，而那正是最難自己診斷的故障：
//   畫面正常、網路正常、日誌也正常，只有「東西不見了」。
//
// ★ 這份清單刻意「剛好夠本人認出自己、不夠冒名者挑走別人」。標題曾經在這裡，
//   後來被拿掉了（見後端 candidateView）：對本人而言標題只是再確認一次，對打
//   錯名字的人而言它是一張「哪一個值得拿」的菜單——會把隨機碰撞升級成有指引
//   的碰撞。所以這裡渲染的每一個欄位都必須通過同一個問題：**它對本人的幫助，
//   是否大於它對冒名者的幫助？** 想加欄位（email、最近活動、提案標題…）之前先
//   回答這一題；答不出來就不要加。
function pickIdentity(name, candidates) {
  const box = $('whoami-list');
  box.textContent = '';
  for (const c of candidates) {
    const row = e('button', 'btn btn-ghost full whoami-row');
    row.type = 'button';
    row.append(e('div', 'whoami-name', c.name));
    // 數量，不是標題。後端刻意不給標題（見 candidateView）：對本人而言數量已經足夠
    // 分辨，對打錯名字的人而言標題則是一張「哪一個值得拿」的菜單。
    const detail = c.proposals > 0
      ? `建立了 ${c.proposals} 個提案`
      : '沒有建立過提案';
    row.append(e('div', 'whoami-detail sub', detail));
    // 兩個候選都沒提案時，上面那行會一模一樣——這正是「引導使用者選錯」的
    // 形狀。建立日期對本人有意義（他知道自己何時開始用），對冒名者則幾乎
    // 無法判斷，是這裡性價比最好的一個辨識訊號。
    if (c.createdAt) {
      row.append(e('div', 'whoami-detail sub', `建立於 ${c.createdAt.slice(0, 10)}`));
    }
    row.onclick = () => joinIdentity(name, c.uid);
    box.append(row);
  }
  $('m-whoami').hidden = false;
}

// 使用者選定了哪一個身分，再發一次帶 uid 的登記。
//
// ★ 後端會驗證這個 uid 確實屬於這個名字的候選名單（CANDIDATE_INVALID）。前端這邊
//   送什麼都不會擴大權限，這是刻意的：權限邊界永遠在後端，前端只是輸入法。
async function joinIdentity(name, uid) {
  try {
    const r = await req('/register', { method: 'POST', body: JSON.stringify({ name, uid }) });
    S.me = { ...r, registered: true };
    if (r.recovery) S.recovery = r.recovery;
    localStorage.setItem('td_had_identity', '1');
    closeAll();
    renderMe();
    // 接回後手上的清單還是未登入時拿的，重跑一次路由才看得到自己的提案。
    route();
    // 接回後回報「你名下實際有幾個提案」，讓選錯的人當場看得出來。
    //
    // ★ 這裡重新向後端問，而不是把剛才候選清單上的數字回顯。兩者在接對時一模
    //   一樣，在接錯時則不同：回顯的那個數字會跟著錯（它描述的是使用者點下去
    //   的那一項，不是實際接到的身分），於是在唯一需要它講真話的時候說謎。
    //   只有量測結果的訊號才有偵測力。
    try {
      const mine = await req('/my/proposals');
      const n = (mine.proposals || []).length;
      toast(n > 0
        ? `已接回「${r.name}」，你名下有 ${n} 個提案`
        : `已接回「${r.name}」，但這個身分名下沒有提案。若不是你要的，重新輸入名字可以再選一次`);
    } catch {
      toast(`已接回「${r.name}」的身分（本裝置已加入）`);
    }
  }
  catch (x) { toast(x.message, true); }
}

// 登記名字：不再用按鈕，輸入後失焦（blur）或 Enter 即自動登記。
// 名字未變或空白則不發請求（避免 blur 風暴）。
async function registerName() {
  const name = $('me-name').value.trim();
  if (!name || name === S.me.name) return;
  // 碰名接回的確認：沒有身分（未登入）而送出一個名字時，後端若查到同名的既有
  // 身分，會把這台裝置接到那個身分上（DD-8）。這對「換手機的本人」正是想要的，
  // 對「剛好同名的另一個人」則是接管了別人的身分——兩者送出的請求完全相同，
  // 後端無從分辨，所以唯一能問的地方在這裡。
  //
  // ★ 只在「本來就沒有身分」時問。已登入者送名字是改名，不會碰 token，
  //   在那裡跳確認只會變成每次改名都被騷擾一次。
  const reattaching = !S.me.uid;
  try {
    const r = await req('/register', { method: 'POST', body: JSON.stringify({ name }) });
    // 這個名字底下有不只一個既有身分：後端刻意不猜，把候選交回來讓本人認領。
    //
    // ★ 為什麼一定要問，而不是沿用「接第一個」：接回時各身分在 token 模型上
    //   確實等價，但掛在 uid 底下的「內容」不等價。實測 live 資料上有人在碰名
    //   接回上線前就有兩個身分，只有其中一個有提案；隨機接一個等於一半機率
    //   把人接進空帳號，而且畫面正常、日誌無錯，使用者只會覺得東西不見了。
    if (Array.isArray(r.candidates) && r.candidates.length) {
      pickIdentity(name, r.candidates);
      return;
    }
    // 撞名分流（DD-3）。三種回應都是「還沒綁任何東西」的中間態，共用同一個 modal：
    //   needEmailVerify — 有對得上的身分，去收信
    //   emailRequired   — 同名者有 email，你沒給，先問你是不是本人
    //   nameCollision   — email 對不上，真的是別人，選改名或編號
    if (r.needEmailVerify || r.emailRequired || r.nameCollision) {
      openSameName(name, r);
      return;
    }
    if (reattaching && r.uid && r.registered) {
      // 後端已完成接回；這裡是事後告知而非事前攔截，因為「這個名字存不存在」
      // 只有後端知道，而多開一個查詢端點等於把 roster 變成可列舉的
      // （R8 的存取模型刻意不提供列舉）。
      toast(`已接回「${r.name}」的既有身分（本裝置已加入）`);
    } else {
      toast(`已登記為「${r.name}」`);
    }
    S.me = { ...r, registered: true };
    if (r.recovery) S.recovery = r.recovery;
    localStorage.setItem('td_had_identity', '1');
    renderMe();
  }
  catch (x) { toast(x.message, true); }
}
$('me-name').addEventListener('blur', registerName);
$('me-name').addEventListener('keydown', ev => { if (ev.key === 'Enter') { ev.preventDefault(); $('me-name').blur(); } });

// 進入行程時的擁有權挑戰（owner-inheritance）。
//
// ★ 提示式，不阻擋（使用者裁示 Q4）。modal 可關、行程照看照投——知道 pid
//   本來就是進場憑證（R8）。阻擋會讓「剛好同名的另一個人」連被邀請的行程
//   都看不到，而那不是他的錯。認證只決定他是被合併還是被請求改名。
//
// ★ 旗標由伺服器算，前端不自己推。前端比對姓名需要它不擁有的資料
//   （owner 的 email 是否已設），而只比姓名會在 owner 未設 email 時誤彈——
//   那正是 DD-9 刻意寬容的那條路徑。
//
// ★ 一個行程只彈一次。SSE 每一個 delta 都會重算旗標，不記住就會每次別人投票
//   都把 modal 重新推到使用者臉上，把提示變成騷擾。
let ownerTakeoverShown = '';
function maybeOwnerTakeover() {
  const p = S.proposal;
  if (!p || !p.ownerTakeoverChallenge) return;
  if (ownerTakeoverShown === p.pid) return;
  ownerTakeoverShown = p.pid;
  openSameName(S.me.name || '', {
    msg: `「${p.ownerName || ''}」這個名字已經是這個行程的發起人。如果那就是你本人，` +
      `輸入該身分的 email 完成認證，這台裝置就會接回你原來的身分（含管理權）。` +
      `如果你是剛好同名的另一個人，可以直接關閉——行程照看照投，只是名單上需要區分彼此。`,
    nameCollision: true,
  });
}

// ─── 撞名分流 modal（DD-3 / GRAFCET 步3→4→6）───
//
// ★ 這個 modal 本身不改任何狀態。它只把後端已經做出的分流結果呈現出來，並把
//   使用者的選擇送回去——所有判斷都在伺服器。前端如果自己推論「這應該是同
//   一人」並省掉認證，那條路徑就成了繞過授權閘的後門。
function openSameName(name, r) {
  S.pendingName = name;
  $('sn-msg').textContent = r.msg || '';
  $('sn-sent').hidden = true;
  $('sn-email').value = '';
  // 改名/編號只在「真撞名不同人」時才給（nameCollision）。在 needEmailVerify 時露出
  // 它等於邀請本人綕過認證另開一個帳號，那正好是本功能要避免的分裂身分。
  $('sn-collision').hidden = !r.nameCollision;
  // email 輸入框：nameCollision 時使用者已經給過 email 且對不上，再問一次也無害
  // （他可能只是打錯），所以三種情況都保留。
  $('f-sn-verify').hidden = false;
  $('m-samename').hidden = false;
  setTimeout(() => $('sn-email').focus(), 40);
}

// 分支 A：寄認證信。
//
// ★ 成功回應只代表「信寄出了」，不代表綁定。綁定發生在使用者點連結那一刻（步5）。
//   這裡若把畫面直接切成已登入，就是把一個尚未發生的事畫成已發生。
$('f-sn-verify').onsubmit = async ev => {
  ev.preventDefault();
  const email = $('sn-email').value.trim();
  if (!email) { toast('請輸入 email', true); return; }
  try {
    const r = await req('/verify-email', { method: 'POST', body: JSON.stringify({ name: S.pendingName, email }) });
    $('f-sn-verify').hidden = true;
    $('sn-sent').hidden = false;
    toast(r.msg || '認證信已寄出');
  } catch (x) {
    // SMTP_SEND_FAILED 是 fail-loud：使用者必須看得到信沒寄成功，否則他會去等
    // 一封永遠不會到的信。這裡不把它吐成「請稍後再試」一句帶過。
    toast(x.message, true);
  }
};

// 分支 B-1：改名（GRAFCET 步7）——回到輸入框重新來一次，不在前端自己推名字。
$('sn-rename').onclick = () => {
  closeAll();
  $('me-name').value = '';
  $('me-name').focus();
  toast('請輸入另一個名字');
};

// 分支 B-2：接受編號（GRAFCET 步8）。後端在鎖內挖最小未占用序號並建帳，
// 前端不自己算 -N（算出來的那一瞬間就已經可能過期）。
$('sn-suffix').onclick = async () => {
  try {
    const email = $('sn-email').value.trim();
    const r = await req('/register', { method: 'POST', body: JSON.stringify({ name: S.pendingName, email, choice: 'suffix' }) });
    S.me = { ...S.me, ...r, registered: true };
    if (r.recovery) S.recovery = r.recovery;
    localStorage.setItem('td_had_identity', '1');
    closeAll(); renderMe(); route();
    toast(`已登記為「${r.name}」`);
  } catch (x) { toast(x.message, true); }
};

// ─── 身分後台 modal：姓名 + email（DD-6 / tasks 4.4）───
function openIdentityModal(hint) {
  closeGear();
  $('i-name').value = S.me.name || '';
  $('i-email').value = S.me.email || '';
  $('m-identity').hidden = false;
  // hint 為真時是被提案閘導過來的，游標直接落在缺的那一格。
  setTimeout(() => (hint ? $('i-email') : $('i-name')).focus(), 40);
}
$('me-identity').onclick = () => openIdentityModal(false);
$('f-identity').onsubmit = async ev => {
  ev.preventDefault();
  const name = $('i-name').value.trim();
  const email = $('i-email').value.trim();
  if (!name) { toast('請輸入姓名', true); return; }
  try {
    // ★ 兩個欄位都送，且 email 用空字串表示「清除」。省略欄位與清成空字串在
    //   後端是不同語意（identityReq 用指標分辨），前端這裡永遠明講，才不會
    //   出現「改個名字順手把 email 洗掉」這種沒有錯誤訊息的損失。
    const r = await req('/identity', { method: 'POST', body: JSON.stringify({ name, email }) });
    S.me = { ...S.me, ...r, registered: true };
    renderMe(); closeAll();
    toast(email ? '已更新身分設定' : '已更新姓名（email 未設定）');
  } catch (x) { toast(x.message, true); }
};

$('me-recovery').onclick = () => { closeGear(); $('my-recovery').textContent = S.recovery || '（本次頁面尚未登記，登記後顯示）'; $('m-recovery').hidden = false; };
$('f-recovery').onsubmit = async ev => {
  ev.preventDefault();
  const code = $('r-paste').value.trim(); if (!code) { closeAll(); return; }
  try { const r = await req('/register', { method: 'POST', body: JSON.stringify({ recovery: code }) }); S.me = { ...r, registered: true }; renderMe(); closeAll(); toast('已接回身分'); route(); }
  catch (x) { toast('接回失敗：' + x.message, true); }
};

$('b-new-proposal').onclick = () => { if (!S.me.registered) { toast('請先在右上角登記名字', true); $('me-name').focus(); return; } $('f-proposal').reset(); $('m-proposal').hidden = false; setTimeout(() => $('p-title').focus(), 40); };
$('f-proposal').onsubmit = async ev => {
  ev.preventDefault();
  const title = $('p-title').value.trim(); if (!title) return;
  try { const p = await req('/proposals', { method: 'POST', body: JSON.stringify({ title, desc: $('p-desc').value.trim() }) }); closeAll(); toast('已建立提案'); go(`#/p/${p.pid}`); }
  catch (x) {
    // DD-7：email 強制的唯一自動觸發點。擋下之後直接把補填的地方打開，
    // 不是只丟一句「需要 email」讓使用者自己去找齒輪。
    if (x.code === 'EMAIL_REQUIRED_FOR_PROPOSAL') {
      closeAll(); toast(x.message, true); openIdentityModal(true); return;
    }
    toast(x.message, true);
  }
};

$('b-new-option').onclick = () => {
  if (!S.me.registered) { toast('請先在右上角登記名字', true); return; }
  const t1 = new Date(); t1.setDate(t1.getDate() + 7); const t2 = new Date(); t2.setDate(t2.getDate() + 10);
  $('f-option').reset(); $('o-start').value = ymd(t1); $('o-end').value = ymd(t2); updateLen(); $('m-option').hidden = false;
};
function updateLen() { const s = $('o-start').value, en = $('o-end').value; $('o-len').textContent = (s && en && s <= en) ? `共 ${days(s, en)} 天` : (s && en ? '⚠ 結束早於開始' : ''); }
$('o-start').addEventListener('change', updateLen); $('o-end').addEventListener('change', updateLen);
$('f-option').onsubmit = async ev => {
  ev.preventDefault();
  const s = $('o-start').value, en = $('o-end').value; if (!s || !en) return;
  if (s > en) { toast('結束日期不能早於開始日期', true); return; }
  try { await req(`/proposals/${S.route}/options`, { method: 'POST', body: JSON.stringify({ startDate: s, endDate: en, note: $('o-note').value.trim() }) }); closeAll(); toast('已新增可選項'); }
  catch (x) { toast(x.message, true); }
};
// ★ 更換網址是撤銷手段（6.9／風險 B）：分享網址本身就是憑證，沒有密碼可改、
//   沒有 session 可撤，換掉 id 是這個設計唯一容許的撤銷方式。舊網址直接 404，
//   不轉址——轉址會讓外洩的網址繼續可用，卻回報說已經撤銷了。
$('b-repid').onclick = async () => {
  closeGear();
  if (!confirm('更換分享網址？\n\n舊網址會立刻失效，拿著舊連結的人（包含你已經分享過的所有人）都必須重新取得新網址。')) return;
  try {
    const p = await req(`/proposals/${S.route}/repid`, { method: 'POST' });
    toast('已更換網址，舊連結已失效');
    go(`#/p/${p.pid}`);
  } catch (x) { toast(x.message, true); }
};

// ── 分享邀請 ──
// ★ 分享網址「就是」憑證：拿到網址的人即可投票（設計如此，撤銷手段是上面的
//   更換網址）。所以這裡不發新 token、不做權限檢查，只把當前提案的**絕對**
//   網址攤開給使用者——相對網址在轉貼到 LINE/Slack 之後一律失效。
//
// ★ navigator.clipboard 只存在於 secure context（https 或 localhost）。本站
//   同時經 https://vote.example.com/teamdating/ 與 http://<lan-ip>:PORT/teamdating/
//   對外，後者 navigator.clipboard 是 undefined。直接呼叫會 TypeError；而包進
//   try/catch 再 toast 成功，會讓「真的複製了」與「瀏覽器根本沒這個 API」
//   共用同一個成功訊息——使用者以為複製好了，貼上卻是舊剪貼簿內容。
//   故：能力先探測 → 失敗顯式降級 → 三種結果三種訊息，永不謊報成功。
function shareURL() {
  const pid = S.route;
  if (!pid) return '';
  return `${location.origin}${base}/#/p/${pid}`;
}

$('b-share').onclick = () => {
  const u = shareURL();
  if (!u) { toast('請先開啟一個提案', true); return; }
  $('share-url').textContent = u;
  $('share-hint').textContent = navigator.clipboard
    ? '拿到這個連結的人就能投票，不需註冊帳號。要收回請按「更換網址」。'
    : '此連線不是 HTTPS，瀏覽器停用一鍵複製——請手動選取上方連結複製。';
  $('m-share').hidden = false;
};

$('b-share-copy').onclick = async () => {
  const u = shareURL();
  if (!u) return;
  if (navigator.clipboard && navigator.clipboard.writeText) {
    try { await navigator.clipboard.writeText(u); toast('已複製邀請連結'); closeAll(); return; }
    catch { /* 權限被拒 → 落到手動路徑，不謊報成功 */ }
  }
  // 降級：只做「把文字選起來」這件一定成立的事，不宣稱已複製。
  try {
    const r = document.createRange(); r.selectNodeContents($('share-url'));
    const sel = getSelection(); sel.removeAllRanges(); sel.addRange(r);
    toast('已選取連結，請按 Ctrl+C／⌘C 複製', true);
  } catch { toast('無法自動複製，請手動選取上方連結', true); }
};

$('b-del-proposal').onclick = async () => {
  closeGear();
  if (!confirm('刪除整個提案？其下所有可選項與票都會移除。')) return;
  try { await req(`/proposals/${S.route}`, { method: 'DELETE' }); toast('已刪除提案'); go('#/'); }
  catch (x) { toast(x.message, true); }
};

window.addEventListener('hashchange', route);

// ─── 啟動 ───
(async () => {
  try { S.me = { ...(await req('/me')), registered: false }; S.me.registered = !!S.me.name; $('me-name').value = S.me.name || ''; }
  catch (x) { gate(x.message); return; }
  // 每個身分最多 5 把鑰匙，第 6 台裝置登入時最久沒用的那一把會被踢掉
  // （store.MaxTokensPerUser）。被踢掉的裝置下次開啟時就是「未登入」，而且
  // 後端沒有任何東西可以告訴它為什麼——那把 token 已經不存在了，請求看起來
  // 就跟一位全新訪客一模一樣。
  //
  // ★ 所以「曾經登入過」這件事只能記在本地。沒有這個旗標，「被踢掉」與
  //   「第一次來」在前端是同一個狀態，使用者只會看到自己的身分無故消失。
  if (!S.me.registered && localStorage.getItem('td_had_identity')) {
    localStorage.removeItem('td_had_identity');
    toast('這個裝置的登入已過期，請重新輸入名字接回', true);
    setTimeout(() => $('me-name') && $('me-name').focus(), 400);
  } else if (S.me.registered) {
    localStorage.setItem('td_had_identity', '1');
  }
  route();
})();
