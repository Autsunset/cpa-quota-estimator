// A self-contained adaptation of the shadcn/ui Sidebar, Tabs and Command
// patterns. The plugin serves one embedded HTML document, so these controls
// use native buttons and dialog instead of a React runtime.
const wbCopy = {
  'zh-CN': {
    brand: '额度工作台', brandTag: 'QUOTA / WORKSPACE', search: '搜索账号', searchHint: '搜索账号或套餐',
    accounts: '全部账号', compare: '对比', accountsCaption: '按风险与剩余额度排列', noAccounts: '尚无账号',
    noMatches: '没有匹配的账号', waiting: '等待首次请求',
    disabled: '已停用', unavailable: '暂不可用',
    back: '返回账号列表', detail: '账号详情', overview: '概览', usage: '用量',
    pricing: '模型与计价', history: '历史与额度', settings: '设置',
    loading: '正在读取额度数据…', updating: '正在更新…', loaded: '数据已更新',
    recalculating: '正在重算历史计价 · {percent}%',
    offline: '当前处于离线状态，显示上次读取的数据。网络恢复后可重试。',
    error: '数据读取失败，当前显示上次成功读取的内容。', retry: '重试',
    emptyTitle: '等待第一条额度记录', emptyBody: '让此账号通过 CPA 完成一次真实的 Codex 请求，这里就会显示剩余额度与重置时间。',
    emptyNoAccount: '添加 Codex 凭证并通过 CPA 完成一次真实请求后，这里会出现账号与额度。',
    emptyDisabled: '这个凭证已停用。启用后让它通过 CPA 完成一次真实请求，即可开始记录额度。',
    emptyUnavailable: '这个凭证目前不可用。恢复后通过 CPA 完成一次真实请求，即可开始记录额度。',
    resetAt: '重置', plan: '套餐', sampled: '最近观测',
    healthy: '当前节奏预计可持续到重置', low: '剩余额度偏低，建议查看近期用量',
    exhaust: '预计会在重置前耗尽', exhausted: '本周期额度已用完',
    awaiting: '已到重置时间，等待新请求确认额度', learning: '样本仍在积累，预测会逐渐稳定',
    seeUsage: '查看用量', searchDialog: '快速定位账号', close: '关闭',
    total: '个账号', attention: '个需要关注',
    scopeNote: '以下预测基于插件已记录的请求；额度百分比来自上游响应头。',
    historyIntro: '查看已确认的额度周期、月度汇总与独立额度。',
    settingsIntro: '管理当前账号的采集范围。改动会立即影响估算的可用性。',
    pricingIntro: '选择计价口径，查看模型价格与被动学习结果。',
    usageIntro: '按真实请求查看模型用量，再比较剩余容量。',
    commandEmpty: '找不到匹配账号。', commandHelp: '输入账号名称或套餐；↑↓ 选择，Enter 打开。',
    unselected: '选择一个账号查看详情', keyboardHint: 'Ctrl K', dataDetails: '数据说明'
  },
  en: {
    brand: 'Quota workspace', brandTag: 'QUOTA / WORKSPACE', search: 'Find account', searchHint: 'Search account or plan',
    accounts: 'All accounts', compare: 'Compare', accountsCaption: 'Sorted by risk and remaining quota', noAccounts: 'No accounts yet',
    noMatches: 'No matching accounts', waiting: 'Awaiting first request',
    disabled: 'Disabled', unavailable: 'Unavailable',
    back: 'Back to accounts', detail: 'Account detail', overview: 'Overview', usage: 'Usage',
    pricing: 'Models & pricing', history: 'History & quotas', settings: 'Settings',
    loading: 'Loading quota data…', updating: 'Updating…', loaded: 'Data updated',
    recalculating: 'Recalculating historical pricing · {percent}%',
    offline: 'You are offline. Showing the last loaded data. Retry when the connection returns.',
    error: 'Could not load data. Showing the last successfully loaded view.', retry: 'Retry',
    emptyTitle: 'Waiting for the first quota sample', emptyBody: 'Complete a real Codex request through CPA for this account to see its remaining quota and reset time.',
    emptyNoAccount: 'Add a Codex credential and complete a real request through CPA to see accounts and quota here.',
    emptyDisabled: 'This credential is disabled. Enable it, then complete a real request through CPA to start recording quota.',
    emptyUnavailable: 'This credential is unavailable. Once restored, complete a real request through CPA to start recording quota.',
    resetAt: 'Resets', plan: 'Plan', sampled: 'Last observed',
    healthy: 'Current pace is expected to last until reset', low: 'Quota is running low; review recent usage',
    exhaust: 'Expected to run out before reset', exhausted: 'Quota exhausted for this cycle',
    awaiting: 'Reset time reached; waiting for a new request to confirm quota', learning: 'More samples will improve this forecast',
    seeUsage: 'View usage', searchDialog: 'Find an account', close: 'Close',
    total: 'accounts', attention: 'need attention',
    scopeNote: 'Forecasts use requests recorded by the plugin; quota percentages come from upstream headers.',
    historyIntro: 'Review confirmed cycles, monthly totals and separate quota scopes.',
    settingsIntro: 'Manage collection coverage for this account. Changes affect whether estimates are available.',
    pricingIntro: 'Choose a pricing basis and review model rates and passive learning.',
    usageIntro: 'Review actual requests by model, then compare remaining capacity.',
    commandEmpty: 'No matching accounts.', commandHelp: 'Type an account or plan; use ↑↓ and Enter to open.',
    unselected: 'Choose an account to see details', keyboardHint: 'Ctrl K', dataDetails: 'About this data'
  }
};

let workbenchView = matchMedia('(max-width: 760px)').matches ? 'accounts' : 'overview';
let workbenchAccounts = [];
let workbenchSelectedAccount = '';
let workbenchLastLoaded = false;
const workbenchViews = ['overview', 'usage', 'pricing', 'history', 'settings'];
const wb = key => (wbCopy[language] || wbCopy['zh-CN'])[key] || key;
const wbSearchIcon = '<svg class="wb-search-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" aria-hidden="true"><circle cx="10.8" cy="10.8" r="6.8"/><path d="m16 16 4.3 4.3"/></svg>';

function setupWorkbench() {
  const root = document.querySelector('main.wrap');
  const shell = document.createElement('div');
  shell.className = 'workbench';
  shell.id = 'workbench';
  shell.innerHTML = [
    '<header class="wb-topbar">',
      '<div class="wb-brand"><span class="wb-brand-mark" aria-hidden="true"><svg viewBox="0 0 40 40" fill="none"><circle cx="20" cy="20" r="13.5" stroke="currentColor" stroke-width="4"/><path d="M20 6.5A13.5 13.5 0 0 1 33.5 20" stroke="var(--brand-glow)" stroke-width="4" stroke-linecap="round"/><circle cx="30.5" cy="30.5" r="4" fill="var(--brand-glow)"/></svg></span><span class="wb-brand-name"><small data-wb-text="brandTag"></small><strong data-wb-text="brand"></strong></span></div>',
      '<div class="wb-top-actions"><button id="wbSearchTrigger" class="wb-search-trigger" type="button" aria-keyshortcuts="Control+K Meta+K">' + wbSearchIcon + '<span data-wb-text="search"></span><kbd id="wbShortcut">⌘ K</kbd></button><div id="wbGlobalControls" class="wb-global-controls"></div></div>',
    '</header>',
    '<div class="wb-layout">',
      '<aside class="wb-sidebar" id="wbSidebar" aria-label="Accounts">',
        '<div class="wb-sidebar-head"><div><span class="wb-overline" data-wb-text="accounts"></span><p id="wbSidebarSummary" class="wb-sidebar-summary"></p></div><button id="wbAllAccounts" class="wb-sidebar-all" type="button" data-wb-text="compare"></button></div>',
        '<label class="wb-search-field">' + wbSearchIcon + '<input id="wbAccountSearch" type="search" autocomplete="off" spellcheck="false"></label>',
        '<div id="wbSidebarError" class="wb-sidebar-error" role="alert" hidden><p id="wbSidebarErrorText"></p><button id="wbSidebarRetry" type="button" data-wb-text="retry"></button></div>',
        '<div id="wbAccountList" class="wb-account-list" aria-live="polite"></div>',
        '<p class="wb-sidebar-foot" data-wb-text="accountsCaption"></p>',
      '</aside>',
      '<div class="wb-main">',
        '<div class="wb-feedback" id="wbFeedback" role="status"><span class="wb-feedback-dot" aria-hidden="true"></span><span id="wbStateText"></span><span id="wbToolbarStatusSlot"></span><button id="wbRetry" type="button" hidden data-wb-text="retry"></button></div>',
        '<div class="wb-loading" id="wbLoading" aria-hidden="true"><div class="wb-skeleton wb-skeleton-title"></div><div class="wb-skeleton wb-skeleton-line"></div><div class="wb-skeleton wb-skeleton-hero"></div><div class="wb-skeleton wb-skeleton-chart"></div></div>',
        '<header class="wb-detail-header" id="wbDetailHeader"><div class="wb-detail-heading"><button id="wbBack" type="button" class="wb-back" data-wb-text="back"></button><span class="wb-overline" data-wb-text="detail"></span><h1 id="wbDetailName"></h1><p id="wbDetailMeta"></p></div><div id="wbDetailControls" class="wb-detail-controls"></div></header>',
        '<nav class="wb-tabs" id="wbTabs" role="tablist" aria-label="Account workspace"><button type="button" role="tab" data-wb-view="overview" data-wb-text="overview"></button><button type="button" role="tab" data-wb-view="usage" data-wb-text="usage"></button><button type="button" role="tab" data-wb-view="pricing" data-wb-text="pricing"></button><button type="button" role="tab" data-wb-view="history" data-wb-text="history"></button><button type="button" role="tab" data-wb-view="settings" data-wb-text="settings"></button></nav>',
        '<div class="wb-empty" id="wbEmpty" role="status" hidden><span class="wb-empty-glyph" aria-hidden="true">○</span><h2 data-wb-text="emptyTitle"></h2><p data-wb-text="emptyBody"></p><button id="wbEmptyAccounts" type="button" data-wb-text="accounts"></button></div>',
        '<div class="wb-panels">',
          '<section id="wbPanelAccounts" class="wb-panel wb-panel-accounts" data-wb-panel="accounts"><div class="wb-panel-head"><div><span class="wb-overline" data-wb-text="accounts"></span><h2 data-wb-text="accounts"></h2></div></div></section>',
          '<section id="wbPanelOverview" class="wb-panel" data-wb-panel="overview"><div class="wb-insight" id="wbInsight"><span class="wb-insight-icon" aria-hidden="true">●</span><span id="wbInsightText"></span><button id="wbSeeUsage" type="button" data-wb-text="seeUsage"></button></div></section>',
          '<section id="wbPanelUsage" class="wb-panel" data-wb-panel="usage"><p class="wb-panel-intro" data-wb-text="usageIntro"></p></section>',
          '<section id="wbPanelPricing" class="wb-panel" data-wb-panel="pricing"><div class="wb-panel-head"><div><span class="wb-overline" data-wb-text="pricing"></span><h2 data-wb-text="pricing"></h2><p data-wb-text="pricingIntro"></p></div><div id="wbPricingActions"></div></div></section>',
          '<section id="wbPanelHistory" class="wb-panel" data-wb-panel="history"><div class="wb-panel-head"><div><span class="wb-overline" data-wb-text="history"></span><h2 data-wb-text="history"></h2><p data-wb-text="historyIntro"></p></div><div id="wbHistoryActions"></div></div></section>',
          '<section id="wbPanelSettings" class="wb-panel" data-wb-panel="settings"><div class="wb-panel-head"><div><span class="wb-overline" data-wb-text="settings"></span><h2 data-wb-text="settings"></h2><p data-wb-text="settingsIntro"></p></div></div></section>',
        '</div>',
        '<footer class="wb-footer" id="wbFooter"><span data-wb-text="scopeNote"></span><details id="wbDataDetails"><summary data-wb-text="dataDetails"></summary></details></footer>',
      '</div>',
    '</div>',
    '<dialog class="wb-command" id="wbCommand" aria-modal="true"><div class="wb-command-head">' + wbSearchIcon + '<input id="wbCommandSearch" type="search" autocomplete="off" spellcheck="false"><button id="wbCommandClose" type="button" aria-label="Close">×</button></div><div id="wbCommandList" class="wb-command-list"></div><p class="wb-command-help" data-wb-text="commandHelp"></p></dialog>'
  ].join('');
  root.prepend(shell);

  const move = (selector, destination) => {
    const item = root.querySelector(selector);
    if (item) shell.querySelector(destination).append(item);
  };
  move('.overview', '#wbPanelAccounts');
  move('.grid', '#wbPanelOverview');
  move('.pace-section', '#wbPanelOverview');
  move('.chart-range', '#wbPanelOverview');
  move('#quotaAnomalyPanel', '#wbPanelOverview');
  move('#quotaTrendScope', '#wbPanelOverview');
  move('.charts', '#wbPanelOverview');
  move('.details', '#wbPanelOverview');
  move('.usage-breakdown', '#wbPanelUsage');
  move('.model-allowances', '#wbPanelUsage');
  move('.pricing-settings', '#wbPanelPricing');
  move('.price-preview', '#wbPanelPricing');
  move('.weight-card', '#wbPanelPricing');
  move('#guidedCalibration', '#wbPanelPricing');
  move('.capacity-history', '#wbPanelHistory');
  move('.monthly', '#wbPanelHistory');
  move('#weeklyQuota', '#wbPanelHistory');
  move('#sparkQuota', '#wbPanelHistory');
  move('.collection-settings', '#wbPanelSettings');
  move('#note', '#wbDataDetails');

  shell.querySelector('#wbGlobalControls').append($('#language'), $('#refresh'));
  shell.querySelector('#wbDetailControls').append($('#account'), $('#period'));
  shell.querySelector('#wbPricingActions').append($('#sync'));
  shell.querySelector('#wbHistoryActions').append($('#showSpark').closest('label'));
  shell.querySelector('#wbToolbarStatusSlot').append($('#toolbarStatus'));
  root.querySelector('.top').remove();

  const firstMetric = shell.querySelector('.grid .card');
  if (firstMetric) {
    const track = document.createElement('div');
    track.className = 'wb-quota-track';
    track.id = 'wbQuotaTrack';
    track.setAttribute('role', 'progressbar');
    track.setAttribute('aria-valuemin', '0');
    track.setAttribute('aria-valuemax', '100');
    track.innerHTML = '<span id="wbQuotaFill"></span>';
    firstMetric.append(track);
  }
  const grid = shell.querySelector('.grid');
  if (grid && grid.children.length === 4) grid.insertBefore(grid.lastElementChild, grid.children[1]);

  $('#wbAccountSearch').oninput = () => renderWorkbenchAccounts();
  $('#wbAccountSearch').onkeydown = event => {
    if (event.key === 'ArrowDown') {
      const first = $('#wbAccountList button:not(:disabled)');
      if (first) { event.preventDefault(); first.focus(); }
    }
  };
  $('#wbAccountList').onkeydown = event => moveWorkbenchListFocus(event, '#wbAccountList button:not(:disabled)');
  $('#wbAllAccounts').onclick = () => setWorkbenchView('accounts');
  $('#wbBack').onclick = () => {
    setWorkbenchView('accounts');
    $('#wbAccountSearch').focus({preventScroll: true});
  };
  $('#wbEmptyAccounts').onclick = () => setWorkbenchView('accounts');
  $('#wbSeeUsage').onclick = () => setWorkbenchView('usage');
  $('#wbRetry').onclick = () => load();
  $('#wbSidebarRetry').onclick = () => load();
  $('#wbSearchTrigger').onclick = () => openWorkbenchCommand();
  $('#wbCommandClose').onclick = () => $('#wbCommand').close();
  $('#wbCommandSearch').oninput = () => renderWorkbenchCommand();
  $('#wbCommandSearch').onkeydown = event => {
    if (event.key === 'ArrowDown') {
      const first = $('#wbCommandList button:not(:disabled)');
      if (first) { event.preventDefault(); first.focus(); }
    }
  };
  $('#wbCommandList').onkeydown = event => moveWorkbenchListFocus(event, '#wbCommandList button:not(:disabled)');
  document.addEventListener('keydown', event => {
    if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k') {
      event.preventDefault();
      openWorkbenchCommand();
    }
  });
  for (const tab of shell.querySelectorAll('[data-wb-view]')) {
    const view = tab.dataset.wbView;
    const panel = shell.querySelector('[data-wb-panel="' + view + '"]');
    tab.id = 'wbTab-' + view;
    tab.setAttribute('aria-controls', panel.id);
    panel.setAttribute('role', 'tabpanel');
    panel.setAttribute('aria-labelledby', tab.id);
    tab.onclick = () => setWorkbenchView(tab.dataset.wbView);
    tab.onkeydown = event => {
      if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
      event.preventDefault();
      const enabled = [...shell.querySelectorAll('[data-wb-view]:not(:disabled)')];
      let index = enabled.indexOf(tab);
      index = event.key === 'Home' ? 0 : event.key === 'End' ? enabled.length - 1
        : (index + (event.key === 'ArrowRight' ? 1 : -1) + enabled.length) % enabled.length;
      const next = enabled[index];
      next.focus();
      setWorkbenchView(next.dataset.wbView, false);
    };
  }
  const previousAccountChange = $('#account').onchange;
  $('#account').onchange = () => {
    setWorkbenchView('overview');
    previousAccountChange && previousAccountChange();
  };
  window.addEventListener('offline', () => workbenchLoadEnd(false, new Error(wb('offline'))));
  window.addEventListener('online', () => load());
  if (!embedded) {
    const saved = history.state && history.state.cqeWorkbench ? history.state : null;
    if (saved && ['accounts'].concat(workbenchViews).includes(saved.view)) workbenchView = saved.view;
    else history.replaceState({cqeWorkbench: true, view: workbenchView, account: ''}, '');
    if (saved && saved.account) {
      const option = document.createElement('option');
      option.value = saved.account;
      option.textContent = saved.account;
      $('#account').append(option);
      $('#account').value = saved.account;
    }
    window.addEventListener('popstate', event => {
      const next = event.state && event.state.cqeWorkbench ? event.state : {view: 'accounts'};
      setWorkbenchView(next.view, false);
      if (next.account && [...$('#account').options].some(option => option.value === next.account) && $('#account').value !== next.account) {
        $('#account').value = next.account;
        load();
      }
    });
  }
  setWorkbenchView(workbenchView, false);
  renderWorkbenchText();
  workbenchLoadStart('');
}

function moveWorkbenchListFocus(event, selector) {
  if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) return;
  const buttons = [...document.querySelectorAll(selector)];
  if (!buttons.length) return;
  event.preventDefault();
  let index = buttons.indexOf(document.activeElement);
  index = event.key === 'Home' ? 0 : event.key === 'End' ? buttons.length - 1
    : (index + (event.key === 'ArrowDown' ? 1 : -1) + buttons.length) % buttons.length;
  buttons[index].focus();
}

function renderWorkbenchText() {
  const shell = $('#workbench');
  if (!shell) return;
  shell.querySelectorAll('[data-wb-text]').forEach(node => { node.textContent = wb(node.dataset.wbText); });
  $('#wbAccountSearch').placeholder = wb('searchHint');
  $('#wbAccountSearch').setAttribute('aria-label', wb('searchHint'));
  $('#wbCommandSearch').placeholder = wb('searchHint');
  $('#wbCommandSearch').setAttribute('aria-label', wb('searchDialog'));
  $('#wbCommand').setAttribute('aria-label', wb('searchDialog'));
  $('#wbCommandClose').setAttribute('aria-label', wb('close'));
  $('#wbSearchTrigger').setAttribute('aria-label', wb('searchDialog'));
  $('#refresh').setAttribute('aria-label', language === 'en' ? 'Reload recorded data' : '刷新已记录数据');
  $('#wbSidebar').setAttribute('aria-label', wb('accounts'));
  $('#wbTabs').setAttribute('aria-label', wb('detail'));
  $('#wbShortcut').textContent = /Mac|iPhone|iPad/.test(navigator.platform) ? '⌘ K' : wb('keyboardHint');
  renderWorkbenchAccounts();
  renderWorkbenchCommand();
  if (state) renderWorkbenchSummary(state);
  if (!$('#wbSidebarError').hidden) {
    $('#wbSidebarErrorText').textContent = wb($('#wbFeedback').dataset.state === 'offline' ? 'offline' : 'error');
  }
}

function setWorkbenchView(view, push = true) {
  if (!['accounts'].concat(workbenchViews).includes(view)) view = 'overview';
  if (['usage', 'history'].includes(view) && $('#workbench').dataset.empty === 'true') view = 'overview';
  const previousView = workbenchView;
  workbenchView = view;
  const shell = $('#workbench');
  if (!shell) return;
  shell.dataset.view = view;
  $('#wbDetailHeader').hidden = view === 'accounts';
  $('#wbTabs').hidden = view === 'accounts' || !state || !state.account;
  shell.querySelectorAll('[data-wb-panel]').forEach(panel => {
    panel.hidden = panel.dataset.wbPanel !== view;
  });
  shell.querySelectorAll('[data-wb-view]').forEach(tab => {
    const selected = tab.dataset.wbView === view;
    tab.setAttribute('aria-selected', String(selected));
    tab.tabIndex = selected ? 0 : -1;
  });
  $('#wbEmpty').hidden = view === 'accounts' || Boolean(state && state.latest);
  $('#wbAllAccounts').setAttribute('aria-current', view === 'accounts' ? 'page' : 'false');
  if (push && !embedded && (previousView !== view || !history.state || history.state.account !== $('#account').value)) {
    history.pushState({cqeWorkbench: true, view: view, account: $('#account').value}, '');
  }
  if (push && previousView !== view) window.scrollTo({top: 0, behavior: 'instant'});
}

function selectWorkbenchAccount(account) {
  if (!account) return;
  const changed = $('#account').value !== account;
  if (changed) {
    if (![...$('#account').options].some(option => option.value === account)) {
      const option = document.createElement('option');
      option.value = account;
      option.textContent = account;
      $('#account').append(option);
    }
    $('#account').value = account;
    $('#period').value = '';
    $('#month').value = '';
    chartRangeManual = false;
    chartRange = null;
  }
  setWorkbenchView('overview');
  renderWorkbenchAccounts();
  if (matchMedia('(max-width: 760px)').matches) {
    $('#wbDetailName').tabIndex = -1;
    $('#wbDetailName').focus({preventScroll: true});
  }
  if (changed) load();
}

function renderWorkbenchAccounts(items, selected) {
  if (items) {
    workbenchAccounts = items.slice();
    workbenchSelectedAccount = selected || '';
  }
  const list = $('#wbAccountList');
  if (!list) return;
  const query = ($('#wbAccountSearch').value || '').trim().toLocaleLowerCase();
  const priority = entry => {
    if (entry.item.disabled || entry.item.unavailable) return 4;
    if (entry.item.sampled === false) return 3;
    if (entry.row.sort.forecast >= 3 || entry.row.sort.remaining <= 20) return 0;
    if (entry.row.sort.remaining <= 40) return 1;
    return 2;
  };
  const ordered = workbenchAccounts.map(item => ({item: item, row: overviewRowData(item)})).sort((a, b) => {
    const category = priority(a) - priority(b);
    if (category) return category;
    const ar = Number.isFinite(a.row.sort.remaining) ? a.row.sort.remaining : 101;
    const br = Number.isFinite(b.row.sort.remaining) ? b.row.sort.remaining : 101;
    return ar - br || String(a.item.account).localeCompare(String(b.item.account));
  });
  const filtered = ordered.filter(entry => {
    const item = entry.item;
    return !query || (String(item.account) + ' ' + String(item.plan_type || '')).toLocaleLowerCase().includes(query);
  });
  const attention = ordered.filter(entry => entry.item.sampled !== false && (entry.row.sort.remaining <= 20 || entry.row.sort.forecast >= 3)).length;
  $('#wbSidebarSummary').textContent = workbenchAccounts.length + ' ' + wb('total') + (attention ? ' · ' + attention + ' ' + wb('attention') : '');
  list.replaceChildren();
  if (!filtered.length) {
    const empty = document.createElement('div');
    empty.className = 'wb-list-empty';
    empty.innerHTML = wbSearchIcon + '<p></p>';
    empty.querySelector('p').textContent = workbenchAccounts.length ? wb('noMatches')
      : workbenchLastLoaded ? wb('noAccounts') : wb('loading');
    list.append(empty);
    return;
  }
  filtered.forEach(({item, row}) => {
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'wb-account-item';
    if (item.account === workbenchSelectedAccount) button.classList.add('is-selected');
    const waiting = item.sampled === false;
    const disabled = Boolean(item.disabled || item.unavailable);
    const remaining = Number(row.sort.remaining);
    button.dataset.state = disabled ? 'muted' : waiting ? 'waiting'
      : remaining <= 20 || row.sort.forecast >= 3 ? 'danger' : remaining <= 40 ? 'warn' : 'good';
    button.title = item.account || '';
    button.setAttribute('aria-label', String(item.account || '') + ', ' + (waiting ? wb('waiting') : row.text.remaining));
    const heading = document.createElement('span');
    heading.className = 'wb-account-heading';
    const dot = document.createElement('i');
    dot.className = 'wb-account-dot';
    dot.setAttribute('aria-hidden', 'true');
    const name = document.createElement('strong');
    name.textContent = item.account || '—';
    const value = document.createElement('b');
    value.textContent = waiting ? '—' : Number.isFinite(remaining) ? remaining.toFixed(0) + '%' : '—';
    heading.append(dot, name, value);
    const meta = document.createElement('span');
    meta.className = 'wb-account-meta';
    const plan = document.createElement('span');
    plan.textContent = item.plan_type || 'Codex';
    const status = document.createElement('span');
    status.textContent = item.disabled ? wb('disabled') : item.unavailable ? wb('unavailable')
      : waiting ? wb('waiting') : row.weekly ? row.text.remaining.replaceAll('\n', ' · ') : row.forecastClass === 'status-fast' ? wb('exhaust') : '';
    meta.append(plan, status);
    button.append(heading, meta);
    button.onclick = () => selectWorkbenchAccount(item.account);
    list.append(button);
  });
}

function openWorkbenchCommand() {
  const dialog = $('#wbCommand');
  if (dialog.open) return;
  $('#wbCommandSearch').value = '';
  renderWorkbenchCommand();
  dialog.showModal();
  $('#wbCommandSearch').focus();
}

function renderWorkbenchCommand() {
  const list = $('#wbCommandList');
  if (!list) return;
  const query = ($('#wbCommandSearch').value || '').trim().toLocaleLowerCase();
  list.replaceChildren();
  const matches = workbenchAccounts.filter(item =>
    !query || (String(item.account) + ' ' + String(item.plan_type || '')).toLocaleLowerCase().includes(query));
  if (!matches.length) {
    const empty = document.createElement('p');
    empty.className = 'wb-command-empty';
    empty.textContent = wb('commandEmpty');
    list.append(empty);
    return;
  }
  matches.forEach(item => {
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'wb-command-item';
    const name = document.createElement('strong');
    name.textContent = item.account || '—';
    const meta = document.createElement('span');
    meta.textContent = item.sampled === false ? wb('waiting') : item.plan_type || 'Codex';
    button.append(name, meta);
    button.onclick = () => {
      $('#wbCommand').close();
      selectWorkbenchAccount(item.account);
    };
    list.append(button);
  });
}

function renderWorkbenchSummary(summary) {
  const account = summary && summary.account || '';
  $('#wbDetailName').textContent = account || wb('unselected');
  $('#wbDetailName').title = account;
  const point = summary && summary.latest;
  const configured = workbenchAccounts.find(item => item.account === account);
  const parts = [];
  if (summary && (summary.plan_type || configured && configured.plan_type)) {
    parts.push(wb('plan') + ' ' + (summary.plan_type || configured.plan_type));
  }
  if (point && point.sampled_at) parts.push(wb('sampled') + ' ' + dt(point.sampled_at));
  if (point && point.reset_at) parts.push(wb('resetAt') + ' ' + dt(point.reset_at));
  $('#wbDetailMeta').textContent = parts.join(' · ');
  $('#workbench').dataset.empty = String(!point);
  $('#wbEmpty').hidden = Boolean(point || workbenchView === 'accounts');
  $('#wbTabs').hidden = workbenchView === 'accounts' || !account;
  for (const tab of document.querySelectorAll('[data-wb-view]')) {
    tab.disabled = !point && ['usage', 'history'].includes(tab.dataset.wbView);
  }
  $('#wbEmpty h2').textContent = account ? wb('emptyTitle') : wb('unselected');
  $('#wbEmpty p').textContent = !account ? wb('emptyNoAccount')
    : configured && configured.disabled ? wb('emptyDisabled')
    : configured && configured.unavailable ? wb('emptyUnavailable') : wb('emptyBody');
  const remaining = point
    ? Number.isFinite(+summary.remaining_percent)
      ? +summary.remaining_percent
      : Math.max(0, 100 - Number(point.used_percent || 0))
    : NaN;
  const track = $('#wbQuotaTrack');
  if (track) {
    track.setAttribute('aria-label', language === 'en' ? 'Quota remaining' : '剩余额度');
    if (Number.isFinite(remaining)) track.setAttribute('aria-valuenow', String(Math.max(0, Math.min(100, remaining))));
    else track.removeAttribute('aria-valuenow');
    $('#wbQuotaFill').style.transform = 'scaleX(' + (Number.isFinite(remaining) ? Math.max(0, Math.min(100, remaining)) / 100 : 0) + ')';
  }
  const burn = summary && summary.burn_forecast || {};
  const exhaustSoon = burn.recent_available ? burn.recent_will_exhaust_before_reset : burn.will_exhaust_before_reset;
  const status = summary && summary.quota_status || '';
  const insight = $('#wbInsight');
  let stateName = 'good', key = 'healthy';
  if (!point) { stateName = 'muted'; key = 'learning'; }
  else if (status === 'exhausted' || remaining <= 0) { stateName = 'danger'; key = 'exhausted'; }
  else if (status === 'awaiting_refresh') { stateName = 'warn'; key = 'awaiting'; }
  else if (exhaustSoon) { stateName = 'danger'; key = 'exhaust'; }
  else if (remaining <= 20) { stateName = 'warn'; key = 'low'; }
  else if (!burn.available) { stateName = 'muted'; key = 'learning'; }
  insight.dataset.state = stateName;
  $('#wbInsightText').textContent = wb(key);
  $('#wbSeeUsage').hidden = !point;
}

function workbenchLoadStart(account) {
  const shell = $('#workbench');
  if (!shell) return;
  const switching = !workbenchLastLoaded || (state && account && account !== state.account);
  shell.dataset.loading = switching ? 'true' : 'false';
  $('#wbLoading').hidden = !switching;
  if (switching) $('#wbEmpty').hidden = true;
  $('#wbSidebarError').hidden = true;
  $('#wbAccountList').hidden = false;
  $('#wbFeedback').dataset.state = 'loading';
  $('#wbStateText').textContent = wb(workbenchLastLoaded ? 'updating' : 'loading');
  $('#wbRetry').hidden = true;
}

function workbenchPricingTaskStatus(task, percent) {
  if (!$('#workbench') || !['queued', 'running', 'rolling_back'].includes(task.status)) return;
  $('#wbFeedback').dataset.state = 'loading';
  $('#wbStateText').textContent = wb('recalculating').replace('{percent}', percent.toFixed(0));
  if (workbenchLastLoaded) {
    $('#workbench').dataset.loading = 'false';
    $('#wbLoading').hidden = true;
  }
}

function workbenchLoadEnd(success, error) {
  const shell = $('#workbench');
  if (!shell) return;
  shell.dataset.loading = 'false';
  $('#wbLoading').hidden = true;
  const offline = !navigator.onLine;
  $('#wbFeedback').dataset.state = success ? 'good' : offline ? 'offline' : 'error';
  $('#wbStateText').textContent = success ? wb('loaded') : offline ? wb('offline') : wb('error');
  $('#wbRetry').hidden = Boolean(success);
  $('#wbSidebarError').hidden = Boolean(success);
  $('#wbSidebarErrorText').textContent = offline ? wb('offline') : wb('error');
  $('#wbAccountList').hidden = !success && !workbenchAccounts.length;
  if (success) {
    workbenchLastLoaded = true;
    renderWorkbenchAccounts();
    $('#toolbarStatus').textContent = language === 'en'
      ? 'Reload reads recorded data; quota updates after a new request'
      : '刷新只重读已记录数据；额度在新请求后更新';
    renderWorkbenchSummary(state);
    if (!embedded && history.state && history.state.cqeWorkbench) {
      history.replaceState({cqeWorkbench: true, view: workbenchView, account: state && state.account || ''}, '');
    }
  } else if (state) {
    renderWorkbenchSummary(state);
  }
}
